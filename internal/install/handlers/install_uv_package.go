package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"tts-proxy/internal/install"
)

// InstallUVPackageRule 表示「使用 uv 在指定虚拟环境中检查 / 安装某个 Python 包」的规则。
//
//	{
//	  "type": "install_uv_package",
//	  "name": "检查并安装 PyTorch",
//	  "package": "torch",                   // 必填，pip 安装名
//	  "import_as": "torch",                 // 可选，import 用名（兼容老写法）
//	  "import_as": ["torch", "voxcpm"],     // 推荐：按顺序逐个 import；
//	                                        //   - 第一个失败立刻报错，避免被后续假阳性掩盖；
//	                                        //   - 最后一项被视为「目标包」，用它读 __version__ 做版本校验。
//	  "venv_dir": ".venv",                  // 可选，venv 目录（相对 exe 所在目录），默认 .venv
//	  "min_version": "2.5.0",               // 可选，最低版本（前闭区间）
//	  "max_version": "3.0.0",               // 可选，最高版本（前开区间）
//	  "install_args": ["--index-url", "https://download.pytorch.org/whl/cu121"],
//	                                        // 可选，附加给 uv pip install 的参数
//	  "spec":   "torch>=2.5",               // 可选，pip 版本规格；缺省 = "<package>>=<min_version>"
//	  "install_hint": { ... }               // 可选，安装失败时的提示
//	}
//
// 行为：
//  1. 定位 venv = <exe 所在目录>/<venv_dir>，并解析其中的 python 解释器；
//     找不到 venv 或解释器则报错（应先执行 create_uv_venv）。
//  2. 按 import_as 顺序逐个 import；最后一项的 __version__ 用于版本判断。
//     全部 import 通过且版本满足要求 → 跳过安装。
//  3. 否则调用 `uv pip install --python <venv-python> [install_args...] <spec>`
//     完成安装，再次按相同顺序校验 import 与版本。
type InstallUVPackageRule struct {
	Package     string       `json:"package"`
	ImportAs    StringList   `json:"import_as,omitempty"`
	VenvDir     string       `json:"venv_dir,omitempty"`
	MinVersion  string       `json:"min_version,omitempty"`
	MaxVersion  string       `json:"max_version,omitempty"`
	InstallArgs []string     `json:"install_args,omitempty"`
	Spec        string       `json:"spec,omitempty"`
	InstallHint *InstallHint `json:"install_hint,omitempty"`
}

// InstallUVPackage 是 install_uv_package 规则的处理器。
func InstallUVPackage(rule install.Rule, _ *install.HandlerContext) (string, error) {
	var spec InstallUVPackageRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if spec.Package == "" {
		return "", fmt.Errorf("package 不能为空")
	}
	if len(spec.ImportAs) == 0 {
		spec.ImportAs = StringList{spec.Package}
	}
	if spec.VenvDir == "" {
		spec.VenvDir = ".venv"
	}

	// import_as 的最后一项视为「目标包」：读它的版本来做 min/max 校验。
	versionTarget := spec.ImportAs[len(spec.ImportAs)-1]

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
	if len(spec.ImportAs) > 1 {
		fmt.Fprintf(&b, "import 顺序: %s\n", strings.Join(spec.ImportAs, " → "))
	}

	// 1) 先按顺序导入除最后一项以外的依赖；最后一项用 readPackageVersion 处理（同时拿版本）。
	preImportErr := verifyImports(&b, pyPath, spec.ImportAs[:len(spec.ImportAs)-1])
	var curVer string
	var curErr error
	if preImportErr == nil {
		curVer, curErr = readPackageVersion(pyPath, versionTarget)
	}
	switch {
	case preImportErr != nil:
		fmt.Fprintf(&b, "前置 import 失败：%v —— 进入安装流程\n", preImportErr)
	case curErr == nil:
		fmt.Fprintf(&b, "已安装 %s 版本=%s\n", spec.Package, curVer)
		if ok, why := versionInRange(curVer, spec.MinVersion, spec.MaxVersion); ok {
			fmt.Fprintf(&b, "√ 版本满足要求%s，跳过安装\n", why)
			return strings.TrimRight(b.String(), "\n"), nil
		}
		why := versionRangeDesc(spec.MinVersion, spec.MaxVersion)
		fmt.Fprintf(&b, "× 版本不满足要求%s，将执行升级/重装\n", why)
	case isImportError(curErr):
		fmt.Fprintf(&b, "未安装 %s（%s import 失败），准备安装\n", spec.Package, versionTarget)
	default:
		// NO_VERSION_ATTR 或其它解释器调用错误：继续走安装流程
		fmt.Fprintf(&b, "无法读取 %s 版本（%v），将尝试安装\n", spec.Package, curErr)
	}

	// 2) 组装并执行 uv pip install
	installSpec := spec.Spec
	if installSpec == "" {
		installSpec = spec.Package
		if spec.MinVersion != "" {
			installSpec = fmt.Sprintf("%s>=%s", spec.Package, spec.MinVersion)
		}
	}

	args := []string{"pip", "install", "--python", pyPath}
	args = append(args, spec.InstallArgs...)
	args = append(args, installSpec)

	fmt.Fprintf(&b, "执行: uv %s\n", strings.Join(args, " "))

	cmd := exec.Command(uvPath, args...)
	cmd.Dir = exeDir
	cmd.Env = utf8Env()
	out, runErr := cmd.CombinedOutput()
	if output := strings.TrimRight(string(out), "\n"); output != "" {
		for _, line := range strings.Split(output, "\n") {
			fmt.Fprintf(&b, "  > %s\n", line)
		}
	}
	if runErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv pip install 失败: %v%s", runErr,
				renderHint(spec.InstallHint, spec.Package))
	}

	// 3) 安装后再校验：先按完整顺序 import，第一个失败即停。
	if err := verifyImports(&b, pyPath, spec.ImportAs); err != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装后导入校验失败: %v%s", err,
				renderHint(spec.InstallHint, spec.Package))
	}

	newVer, verErr := readPackageVersion(pyPath, versionTarget)
	if verErr != nil {
		// 没有 __version__ 不是致命问题：import 已经能通过就放行。
		if strings.HasPrefix(verErr.Error(), "NO_VERSION_ATTR") {
			fmt.Fprintf(&b, "√ %s 已可导入（未暴露 __version__，跳过版本校验）\n", versionTarget)
			return strings.TrimRight(b.String(), "\n"), nil
		}
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装后仍无法读取 %s 版本: %v", spec.Package, verErr)
	}
	if ok, why := versionInRange(newVer, spec.MinVersion, spec.MaxVersion); !ok {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装后 %s 版本 %s 仍不满足要求%s", spec.Package, newVer, why)
	}
	fmt.Fprintf(&b, "√ 已安装 %s 版本=%s\n", spec.Package, newVer)

	return strings.TrimRight(b.String(), "\n"), nil
}

// verifyImports 按顺序在 venv 解释器中 import 各模块。
// 第一个失败立刻返回，错误中带上「先失败的模块名」便于定位真正的根因。
// names 为空时不做任何事、视作通过。
func verifyImports(b *strings.Builder, python string, names []string) error {
	for _, n := range names {
		if _, err := readPackageVersion(python, n); err != nil {
			if isImportError(err) {
				fmt.Fprintf(b, "× import %s 失败：%v\n", n, err)
				return fmt.Errorf("import %s: %v", n, err)
			}
			// NO_VERSION_ATTR 表示能 import、只是没暴露 __version__：算通过。
			if strings.HasPrefix(err.Error(), "NO_VERSION_ATTR") {
				fmt.Fprintf(b, "√ import %s（无 __version__）\n", n)
				continue
			}
			fmt.Fprintf(b, "× import %s 异常：%v\n", n, err)
			return fmt.Errorf("import %s: %v", n, err)
		}
		fmt.Fprintf(b, "√ import %s\n", n)
	}
	return nil
}

// venvPython 返回 venv 中的 python 可执行文件路径。
func venvPython(venvDir string) (string, error) {
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = []string{
			filepath.Join(venvDir, "Scripts", "python.exe"),
			filepath.Join(venvDir, "Scripts", "python3.exe"),
		}
	} else {
		candidates = []string{
			filepath.Join(venvDir, "bin", "python"),
			filepath.Join(venvDir, "bin", "python3"),
		}
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("在 %s 中未找到 python 解释器（候选: %s）",
		venvDir, strings.Join(candidates, ", "))
}

// readPackageVersion 用指定 python 解释器导入包并读取 __version__。
// 错误的 message 中以 IMPORT_ERROR / NO_VERSION_ATTR 前缀区分原因，便于上层判断。
//
// 子进程的环境通过 utf8Env() 注入 PYTHONIOENCODING=utf-8 / PYTHONUTF8=1，避免在
// Windows GBK 控制台下报错时输出 "锟斤拷" 样的乱码而误导排错。
func readPackageVersion(python, importAs string) (string, error) {
	// 把 traceback 一起打到 stderr：DLL/ABI 类问题（Windows error 127、
	// "找不到指定的程序" 等）只看 repr(e) 完全定位不到是哪个 .pyd / .dll，
	// 多输出几行即可显著降低排错成本。
	script := fmt.Sprintf(
		`import importlib, sys, traceback
try:
    m = importlib.import_module(%q)
except Exception as e:
    sys.stderr.write("IMPORT_ERROR: " + repr(e) + "\n")
    sys.stderr.write("TRACEBACK:\n")
    traceback.print_exc(file=sys.stderr)
    sys.exit(2)
v = getattr(m, "__version__", None) or getattr(m, "VERSION", None)
if v is None:
    sys.stderr.write("NO_VERSION_ATTR")
    sys.exit(3)
print(v)
`, importAs)

	cmd := exec.Command(python, "-c", script)
	cmd.Env = utf8Env()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return "", fmt.Errorf("%s", stderr)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isImportError 判断 readPackageVersion 返回的错误是否是「包没装」。
func isImportError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(err.Error(), "IMPORT_ERROR")
}

// versionInRange 检查 ver 是否处于 [min, max) 区间；返回 (ok, 说明文字)。
// min/max 为空表示不约束该端。说明用于附在日志末尾，例如「（>= 2.5.0, < 3.0.0）」。
func versionInRange(ver, min, max string) (bool, string) {
	clean := versionRe.FindString(ver)
	if clean == "" {
		return false, fmt.Sprintf("（无法解析版本 %q）", ver)
	}
	desc := versionRangeDesc(min, max)
	if min != "" {
		if cmp, err := compareVersions(clean, min); err != nil || cmp < 0 {
			return false, desc
		}
	}
	if max != "" {
		if cmp, err := compareVersions(clean, max); err != nil || cmp >= 0 {
			return false, desc
		}
	}
	return true, desc
}

func versionRangeDesc(min, max string) string {
	switch {
	case min != "" && max != "":
		return fmt.Sprintf("（>= %s, < %s）", min, max)
	case min != "":
		return fmt.Sprintf("（>= %s）", min)
	case max != "":
		return fmt.Sprintf("（< %s）", max)
	default:
		return ""
	}
}
