package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"tts-proxy/internal/install"
)

// InstallPytorchRule 表示「按显卡 Compute Capability 自适应安装 PyTorch」的规则。
//
//	{
//	  "type": "install_pytorch",
//	  "name": "安装 PyTorch (GPU 自适应)",
//	  "venv_dir": ".venv",
//	  "import_as": ["torch", "torchaudio"],   // 装完后用来校验 import 与版本
//	  "extra_packages": ["torchaudio"],       // 与 torch 一起装的附加包
//	  "sm_matrix": [
//	    { "sm": "7.5",  "spec": "torch>=1.13", "index_url": "https://download.pytorch.org/whl/cu118", "note": "RTX 20xx" },
//	    { "sm": "8.6",  "spec": "torch>=2.0",  "index_url": "https://download.pytorch.org/whl/cu121", "note": "RTX 30xx" },
//	    { "sm": "8.9",  "spec": "torch>=2.1",  "index_url": "https://download.pytorch.org/whl/cu124", "note": "RTX 40xx" },
//	    { "sm": "9.0",  "spec": "torch>=2.1",  "index_url": "https://download.pytorch.org/whl/cu124", "note": "H100" },
//	    { "sm": "10.0", "spec": "torch>=2.6",  "index_url": "https://download.pytorch.org/whl/cu126", "note": "B200" },
//	    { "sm": "12.0", "spec": "torch",       "index_url": "https://download.pytorch.org/whl/nightly/cu128", "pre": true, "note": "RTX 50xx" }
//	  ],
//	  "nightly": {                                       // 显卡 sm 不在 sm_matrix 中或默认兜底时使用
//	    "spec": "torch",
//	    "index_url": "https://download.pytorch.org/whl/nightly/cu128",
//	    "pre": true
//	  },
//	  "cpu_fallback": {                                  // 没有 NVIDIA 驱动时的兜底
//	    "spec": "torch",
//	    "index_url": "https://download.pytorch.org/whl/cpu"
//	  },
//	  "verify_gpu": true,                                // 装完后跑 torch.cuda.is_available() 校验
//	  "upgrade_on_gpu_fail": true,                       // 校验失败时询问 → 卸载旧版 → 装 nightly
//	  "install_hint": { ... }
//	}
//
// 流程：
//  1. 用 nvidia-smi 探测显卡 Compute Capability（compute_cap，如 8.9）和驱动支持的 CUDA Runtime；
//  2. 在 sm_matrix 中精确匹配当前 sm，命中则使用其 spec/index_url；未命中且配置了 nightly 则用 nightly；
//     完全没有 NVIDIA 驱动 → 用 cpu_fallback；
//  3. 用 uv pip install 安装 torch 与 extra_packages；
//  4. 若 verify_gpu=true：跑 torch.cuda.is_available() / get_arch_list() 校验；
//  5. 校验失败且 upgrade_on_gpu_fail=true → 询问用户是否卸载并安装 nightly 重试一次。
type InstallPytorchRule struct {
	VenvDir           string         `json:"venv_dir,omitempty"`
	ImportAs          StringList     `json:"import_as,omitempty"`
	ExtraPackages     []string       `json:"extra_packages,omitempty"`
	SMMatrix          []PytorchEntry `json:"sm_matrix,omitempty"`
	Nightly           *PytorchEntry  `json:"nightly,omitempty"`
	CPUFallback       *PytorchEntry  `json:"cpu_fallback,omitempty"`
	VerifyGPU         bool           `json:"verify_gpu,omitempty"`
	UpgradeOnGPUFail  bool           `json:"upgrade_on_gpu_fail,omitempty"`
	InstallHint       *InstallHint   `json:"install_hint,omitempty"`
}

// PytorchEntry 是一条 PyTorch 安装方案。
type PytorchEntry struct {
	SM       string `json:"sm,omitempty"`        // sm_matrix 项需要；nightly/cpu_fallback 不用
	Spec     string `json:"spec,omitempty"`      // 默认 "torch"
	IndexURL string `json:"index_url,omitempty"` // 传给 uv pip --index-url
	Pre      bool   `json:"pre,omitempty"`       // true 时附加 --pre
	Note     string `json:"note,omitempty"`
}

// computeCapRe 匹配 nvidia-smi compute_cap 输出形如 "8.9" 的版本号。
var computeCapRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// InstallPytorch 是 install_pytorch 规则的处理器。
func InstallPytorch(rule install.Rule, ctx *install.HandlerContext) (string, error) {
	var spec InstallPytorchRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if spec.VenvDir == "" {
		spec.VenvDir = ".venv"
	}
	if len(spec.ImportAs) == 0 {
		spec.ImportAs = StringList{"torch"}
	}

	uvPath, err := exec.LookPath("uv")
	if err != nil {
		return "", fmt.Errorf("未找到 uv（请先完成 uv 检查）")
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exePath); rerr == nil {
		exePath = resolved
	}
	exeDir := filepath.Dir(exePath)
	venvDir := filepath.Join(exeDir, spec.VenvDir)

	if !isExistingVenv(venvDir) {
		return "", fmt.Errorf("未在 %s 找到虚拟环境（请先执行 create_uv_venv 步骤）", venvDir)
	}
	pyPath, err := venvPython(venvDir)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "venv 解释器: %s\n", pyPath)

	// 1) 用 nvidia-smi 探测显卡，**不依赖 PyTorch**
	gpuName, smVer, driverCUDA, smiErr := detectGPUBySmi()
	if smiErr == nil {
		fmt.Fprintf(&b, "GPU         : %s\n", gpuName)
		fmt.Fprintf(&b, "Compute Cap : %s (sm_%s)\n", smVer, smToTag(smVer))
		if driverCUDA != "" {
			fmt.Fprintf(&b, "Driver CUDA : %s（驱动支持的最高 CUDA Runtime）\n", driverCUDA)
		}
	} else {
		fmt.Fprintf(&b, "× 未检测到 NVIDIA 显卡: %v\n", smiErr)
	}

	// 2) 选择安装方案
	entry, source := chooseEntry(&spec, smVer, smiErr == nil)
	if entry == nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("没有可用的 PyTorch 安装方案：sm_matrix 未命中且未配置 nightly/cpu_fallback%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}
	fmt.Fprintf(&b, "选用方案    : %s%s\n", source, entryDesc(entry))

	// 3) 执行安装
	if err := runUVInstall(&b, uvPath, exeDir, pyPath, entry, spec.ExtraPackages); err != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv pip install 失败: %v%s", err,
				renderHint(spec.InstallHint, "PyTorch"))
	}

	// 4) 装完后 import 校验
	if err := verifyImports(&b, pyPath, spec.ImportAs); err != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装后导入校验失败: %v%s", err,
				renderHint(spec.InstallHint, "PyTorch"))
	}
	torchVer, _ := readPackageVersion(pyPath, "torch")
	if torchVer != "" {
		fmt.Fprintf(&b, "√ torch 已安装 版本=%s\n", torchVer)
	}

	// 5) GPU 校验
	if !spec.VerifyGPU || smiErr != nil {
		// 没有 GPU 或未要求校验，到此为止
		if smiErr != nil {
			fmt.Fprintf(&b, "ℹ 当前无 NVIDIA 驱动，跳过 GPU 校验\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}

	gpuOK, gpuDetail := verifyTorchGPU(pyPath)
	for _, line := range strings.Split(strings.TrimRight(gpuDetail, "\n"), "\n") {
		fmt.Fprintf(&b, "  > %s\n", line)
	}
	if gpuOK {
		fmt.Fprintf(&b, "√ torch.cuda.is_available() = True\n")
		return strings.TrimRight(b.String(), "\n"), nil
	}

	// 6) GPU 校验失败 → 询问是否升级到 nightly
	fmt.Fprintf(&b, "× PyTorch 已安装但无法使用 GPU（可能是显卡 sm 太新，wheel 编译时未覆盖）\n")
	if !spec.UpgradeOnGPUFail {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("PyTorch GPU 不可用且 upgrade_on_gpu_fail=false%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}
	if spec.Nightly == nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("PyTorch GPU 不可用且未配置 nightly 兜底%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}
	if ctx == nil || ctx.Confirm == nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("PyTorch GPU 不可用，且没有交互回调可发起升级询问%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}

	prompt := fmt.Sprintf(
		"当前安装的 PyTorch 不支持 GPU（sm_%s）。是否卸载并安装 nightly 版本重试? [Y/N]: ",
		smToTag(smVer))
	yes, askErr := ctx.Confirm(prompt)
	if askErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("读取用户确认失败: %w", askErr)
	}
	if !yes {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("用户拒绝升级；当前 PyTorch 无法使用 GPU%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}

	// 卸载旧包
	uninstallTargets := append([]string{"torch"}, spec.ExtraPackages...)
	fmt.Fprintf(&b, "卸载旧版本: %s\n", strings.Join(uninstallTargets, ", "))
	if err := runUVUninstall(&b, uvPath, exeDir, pyPath, uninstallTargets); err != nil {
		fmt.Fprintf(&b, "  > uninstall 警告: %v（继续尝试覆盖安装）\n", err)
	}

	// 安装 nightly
	fmt.Fprintf(&b, "改用方案    : nightly%s\n", entryDesc(spec.Nightly))
	if err := runUVInstall(&b, uvPath, exeDir, pyPath, spec.Nightly, spec.ExtraPackages); err != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("nightly 安装失败: %v%s", err,
				renderHint(spec.InstallHint, "PyTorch"))
	}
	if err := verifyImports(&b, pyPath, spec.ImportAs); err != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("nightly 安装后导入校验失败: %v%s", err,
				renderHint(spec.InstallHint, "PyTorch"))
	}
	gpuOK2, gpuDetail2 := verifyTorchGPU(pyPath)
	for _, line := range strings.Split(strings.TrimRight(gpuDetail2, "\n"), "\n") {
		fmt.Fprintf(&b, "  > %s\n", line)
	}
	if !gpuOK2 {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("nightly 也未启用 GPU；请人工排查显卡驱动版本与 torch wheel 兼容性%s",
				renderHint(spec.InstallHint, "PyTorch"))
	}
	fmt.Fprintf(&b, "√ nightly 安装成功且 GPU 可用\n")
	return strings.TrimRight(b.String(), "\n"), nil
}

// detectGPUBySmi 通过 nvidia-smi 获取 GPU 名称、compute_cap 与驱动 CUDA 版本。
// 这一步**不依赖 PyTorch / Python 包**，只要装了 NVIDIA 驱动就能用。
func detectGPUBySmi() (name, computeCap, driverCUDA string, err error) {
	smiPath, lookErr := exec.LookPath("nvidia-smi")
	if lookErr != nil {
		return "", "", "", fmt.Errorf("未在 PATH 中找到 nvidia-smi")
	}

	// 1) 优先用 --query-gpu 拿结构化字段（驱动 ≥ 510 支持 compute_cap）
	out, queryErr := exec.Command(smiPath,
		"--query-gpu=name,compute_cap",
		"--format=csv,noheader").CombinedOutput()
	if queryErr == nil {
		line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		parts := strings.Split(line, ",")
		if len(parts) >= 2 {
			n := strings.TrimSpace(parts[0])
			c := strings.TrimSpace(parts[1])
			if m := computeCapRe.FindString(c); m != "" {
				name = n
				computeCap = m
			}
		}
	}

	// 2) 再跑一次普通 nvidia-smi 拿驱动 CUDA 版本（兼容老驱动同时兜底 compute_cap）
	plainOut, plainErr := exec.Command(smiPath).CombinedOutput()
	if plainErr != nil && computeCap == "" {
		return "", "", "", fmt.Errorf("nvidia-smi 调用失败: %v", plainErr)
	}
	if plainErr == nil {
		if m := smiRe.FindStringSubmatch(string(plainOut)); m != nil {
			driverCUDA = m[1]
		}
	}

	if computeCap == "" {
		return "", "", "", fmt.Errorf("nvidia-smi 未输出 compute_cap（驱动可能过旧，建议升级到 ≥ 510）")
	}
	return name, computeCap, driverCUDA, nil
}

// smToTag 把 "8.9" / "12.0" 转换成 "89" / "120" —— 即 sm_xy 后缀。
func smToTag(cap string) string {
	m := computeCapRe.FindStringSubmatch(cap)
	if m == nil {
		return cap
	}
	return m[1] + m[2]
}

// chooseEntry 按 sm_matrix → nightly → cpu_fallback 的顺序选出安装方案。
// 返回 entry 及其来源描述（"sm_matrix 命中" / "nightly 兜底" 等）。
func chooseEntry(spec *InstallPytorchRule, smVer string, hasGPU bool) (*PytorchEntry, string) {
	if !hasGPU {
		if spec.CPUFallback != nil {
			return spec.CPUFallback, "cpu_fallback（未检测到 NVIDIA 驱动）"
		}
		// 没 GPU 也没 cpu_fallback：仍可让 sm_matrix 走一遍，但不太合理 → nightly 兜底
		if spec.Nightly != nil {
			return spec.Nightly, "nightly（未检测到 GPU 且无 cpu_fallback）"
		}
		return nil, ""
	}

	// 精确按 sm 匹配
	for i := range spec.SMMatrix {
		if smEqual(spec.SMMatrix[i].SM, smVer) {
			return &spec.SMMatrix[i], "sm_matrix 命中 sm=" + spec.SMMatrix[i].SM
		}
	}
	if spec.Nightly != nil {
		return spec.Nightly, fmt.Sprintf("nightly 兜底（sm=%s 不在 sm_matrix 中）", smVer)
	}
	return nil, ""
}

// smEqual 比较两个 compute_cap 字符串（如 "8.9" 与 "8.9"），归一化忽略前后空白与第三段 0。
func smEqual(a, b string) bool {
	ma := computeCapRe.FindStringSubmatch(strings.TrimSpace(a))
	mb := computeCapRe.FindStringSubmatch(strings.TrimSpace(b))
	if ma == nil || mb == nil {
		return false
	}
	return ma[1] == mb[1] && ma[2] == mb[2]
}

// entryDesc 渲染一条方案的简要描述：" spec=torch>=2.1 index=cu124 [note: RTX 40xx]"。
func entryDesc(e *PytorchEntry) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	spec := e.Spec
	if spec == "" {
		spec = "torch"
	}
	fmt.Fprintf(&b, " spec=%s", spec)
	if e.IndexURL != "" {
		fmt.Fprintf(&b, " index=%s", e.IndexURL)
	}
	if e.Pre {
		b.WriteString(" --pre")
	}
	if e.Note != "" {
		fmt.Fprintf(&b, " [%s]", e.Note)
	}
	return b.String()
}

// runUVInstall 执行 `uv pip install --python <py> [--index-url X] [--pre] <spec> [extras...]`。
func runUVInstall(b *strings.Builder, uvPath, exeDir, pyPath string, e *PytorchEntry, extras []string) error {
	args := []string{"pip", "install", "--python", pyPath}
	if e.IndexURL != "" {
		args = append(args, "--index-url", e.IndexURL)
	}
	if e.Pre {
		args = append(args, "--pre")
	}
	spec := e.Spec
	if spec == "" {
		spec = "torch"
	}
	args = append(args, spec)
	args = append(args, extras...)

	fmt.Fprintf(b, "执行: uv %s\n", strings.Join(args, " "))
	cmd := exec.Command(uvPath, args...)
	cmd.Dir = exeDir
	cmd.Env = utf8Env()
	out, runErr := cmd.CombinedOutput()
	indentInto(b, string(out))
	return runErr
}

// runUVUninstall 调用 `uv pip uninstall --python <py> <pkgs...>`。
func runUVUninstall(b *strings.Builder, uvPath, exeDir, pyPath string, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}
	args := []string{"pip", "uninstall", "--python", pyPath}
	args = append(args, pkgs...)
	fmt.Fprintf(b, "执行: uv %s\n", strings.Join(args, " "))
	cmd := exec.Command(uvPath, args...)
	cmd.Dir = exeDir
	cmd.Env = utf8Env()
	out, runErr := cmd.CombinedOutput()
	indentInto(b, string(out))
	return runErr
}

// verifyTorchGPU 用 venv 解释器跑一段脚本：
//   - 打印 torch 版本 / torch.version.cuda / 编译时支持的 sm 列表
//   - 返回 torch.cuda.is_available() 结果
//
// 返回 (gpuOK, 多行诊断文本)。
func verifyTorchGPU(python string) (bool, string) {
	script := `import torch
print("torch", torch.__version__)
print("torch.cuda", torch.version.cuda)
try:
    archs = torch.cuda.get_arch_list()
except Exception as e:
    archs = "(get_arch_list failed: %r)" % (e,)
print("arch_list", archs)
ok = torch.cuda.is_available()
print("is_available", ok)
if ok:
    try:
        print("device", torch.cuda.get_device_name(0))
        print("capability", torch.cuda.get_device_capability(0))
    except Exception as e:
        print("device_query_failed", repr(e))
import sys
sys.exit(0 if ok else 1)
`
	cmd := exec.Command(python, "-c", script)
	cmd.Env = utf8Env()
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}
