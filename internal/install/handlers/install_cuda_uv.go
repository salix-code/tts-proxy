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

// InstallCUDAUVRule 表示「在虚拟环境中安装 CUDA 运行时（通过 uv pip）或回退到脚本」的规则。
//
//	{
//	  "type": "install_cuda_uv",
//	  "name": "在 venv 中安装 CUDA 运行时",
//	  "venv_dir": ".venv",                       // 可选，默认 .venv
//	  "packages": [                              // 可选，要装的 NVIDIA CUDA 运行时 wheel
//	    "nvidia-cuda-runtime-cu12",
//	    "nvidia-cuda-nvrtc-cu12",
//	    "nvidia-cublas-cu12",
//	    "nvidia-cudnn-cu12"
//	  ],
//	  "import_as": ["nvidia.cuda_runtime"],      // 可选，用于「是否已安装」的探测
//	  "install_args": ["--index-url", "https://pypi.org/simple"],   // 可选，附加给 uv pip
//	  "script": {                                // 可选，uv 不可用 / 安装失败时的回退脚本
//	    "windows": ["powershell", "-File", "scripts\\install_cuda.ps1"],
//	    "linux":   ["bash",       "scripts/install_cuda.sh"],
//	    "darwin":  []
//	  },
//	  "install_hint": { ... }                    // 可选，最终失败时的提示
//	}
//
// 行为：
//  1. 解析 venv 路径与其中的 python 解释器（沿用 install_uv_package 的工具函数）；
//  2. 用 venv 解释器逐个 import `import_as` 列表，若全部成功则视为已安装并跳过；
//  3. 否则优先调用 `uv pip install --python <venv-python> [install_args...] <packages...>`；
//  4. uv 不可用或返回非 0 时，执行 script 中匹配当前 OS 的命令作为回退；
//     脚本通过环境变量 TTS_PROXY_VENV / TTS_PROXY_VENV_PYTHON 拿到 venv 路径，便于把内容装进 venv。
type InstallCUDAUVRule struct {
	VenvDir     string              `json:"venv_dir,omitempty"`
	Packages    []string            `json:"packages,omitempty"`
	ImportAs    []string            `json:"import_as,omitempty"`
	InstallArgs []string            `json:"install_args,omitempty"`
	Script      map[string][]string `json:"script,omitempty"`
	InstallHint *InstallHint        `json:"install_hint,omitempty"`
}

// InstallCUDAUV 是 install_cuda_uv 规则的处理器。
func InstallCUDAUV(rule install.Rule, _ *install.HandlerContext) (string, error) {
	var spec InstallCUDAUVRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if spec.VenvDir == "" {
		spec.VenvDir = ".venv"
	}
	if len(spec.Packages) == 0 && len(spec.Script) == 0 {
		return "", fmt.Errorf("packages 与 script 至少要配置一个")
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
	fmt.Fprintf(&b, "venv 目录: %s\n", venvDir)
	fmt.Fprintf(&b, "venv 解释器: %s\n", pyPath)

	// 1) 已安装检测：import_as 列表全部能 import 即视为已装。
	if installed, detail := allImportable(pyPath, spec.ImportAs); installed {
		fmt.Fprintf(&b, "√ 检测到 CUDA 运行时已安装%s，跳过\n", detail)
		return strings.TrimRight(b.String(), "\n"), nil
	} else if detail != "" {
		fmt.Fprintf(&b, "× 现有环境检测：%s\n", detail)
	}

	// 2) 优先使用 uv pip
	if len(spec.Packages) > 0 {
		uvPath, lookErr := exec.LookPath("uv")
		if lookErr != nil {
			fmt.Fprintf(&b, "未找到 uv，跳过 uv pip 路径，尝试回退脚本\n")
		} else {
			args := []string{"pip", "install", "--python", pyPath}
			args = append(args, spec.InstallArgs...)
			args = append(args, spec.Packages...)
			fmt.Fprintf(&b, "执行: uv %s\n", strings.Join(args, " "))

			cmd := exec.Command(uvPath, args...)
			cmd.Dir = exeDir
			out, runErr := cmd.CombinedOutput()
			indentInto(&b, string(out))

			if runErr == nil {
				installed, detail := allImportable(pyPath, spec.ImportAs)
				if installed || len(spec.ImportAs) == 0 {
					fmt.Fprintf(&b, "√ uv pip 安装完成%s\n", detail)
					return strings.TrimRight(b.String(), "\n"), nil
				}
				fmt.Fprintf(&b, "× uv pip 完成但 import 仍失败：%s\n", detail)
			} else {
				fmt.Fprintf(&b, "× uv pip 失败: %v\n", runErr)
			}
		}
	}

	// 3) 回退：执行平台脚本
	cmdLine, ok := pickScriptForOS(spec.Script, runtime.GOOS)
	if !ok || len(cmdLine) == 0 {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("无法通过 uv pip 安装 CUDA，且未配置当前系统(%s)的回退脚本%s",
				runtime.GOOS, renderHint(spec.InstallHint, "CUDA"))
	}
	fmt.Fprintf(&b, "执行回退脚本: %s\n", strings.Join(cmdLine, " "))

	scriptCmd := exec.Command(cmdLine[0], cmdLine[1:]...)
	scriptCmd.Dir = exeDir
	scriptCmd.Env = append(os.Environ(),
		"TTS_PROXY_VENV="+venvDir,
		"TTS_PROXY_VENV_PYTHON="+pyPath,
	)
	out, runErr := scriptCmd.CombinedOutput()
	indentInto(&b, string(out))
	if runErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("回退脚本执行失败: %v%s", runErr,
				renderHint(spec.InstallHint, "CUDA"))
	}

	// 4) 脚本结束后再检测一次
	installed, detail := allImportable(pyPath, spec.ImportAs)
	if installed || len(spec.ImportAs) == 0 {
		fmt.Fprintf(&b, "√ 回退脚本执行完成%s\n", detail)
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return strings.TrimRight(b.String(), "\n"),
		fmt.Errorf("脚本执行完毕但仍检测不到 CUDA 运行时：%s%s",
			detail, renderHint(spec.InstallHint, "CUDA"))
}

// allImportable 用 venv 的 python 检查 names 中的全部模块是否都能 import。
// names 为空时，返回 (false, "")，由调用方决定如何处理。
// 返回的 detail 形如「（√ nvidia.cuda_runtime）」便于追加到日志。
func allImportable(python string, names []string) (bool, string) {
	if len(names) == 0 {
		return false, ""
	}
	var detail strings.Builder
	allOK := true
	for _, n := range names {
		_, err := readPackageVersion(python, n)
		switch {
		case err == nil:
			fmt.Fprintf(&detail, " √ %s", n)
		case isImportError(err):
			fmt.Fprintf(&detail, " × %s(未安装)", n)
			allOK = false
		default:
			// 比如 NO_VERSION_ATTR：能 import 即可视作存在
			if strings.HasPrefix(err.Error(), "NO_VERSION_ATTR") {
				fmt.Fprintf(&detail, " √ %s(无版本属性)", n)
			} else {
				fmt.Fprintf(&detail, " ? %s(%v)", n, err)
				allOK = false
			}
		}
	}
	return allOK, "（" + strings.TrimSpace(detail.String()) + "）"
}

// pickScriptForOS 从 script 映射中选当前 OS 的命令。支持 "windows"/"linux"/"darwin"
// 以及别名 "mac"/"macos"/"win"。返回 (cmd, ok)。
func pickScriptForOS(scripts map[string][]string, currentOS string) ([]string, bool) {
	if scripts == nil {
		return nil, false
	}
	for key, cmd := range scripts {
		if matchOSAlias(key, currentOS) {
			return cmd, true
		}
	}
	return nil, false
}

func matchOSAlias(name, current string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mac", "macos", "osx":
		return current == "darwin"
	case "win":
		return current == "windows"
	default:
		return strings.EqualFold(name, current)
	}
}

// indentInto 把命令输出按行加 "  > " 缩进追加到 b。
func indentInto(b *strings.Builder, output string) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		fmt.Fprintf(b, "  > %s\n", line)
	}
}
