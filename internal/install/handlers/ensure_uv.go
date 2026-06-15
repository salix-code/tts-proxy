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

// EnsureUVRule 表示「检查 uv，未装/版本过旧时询问并自动安装」的规则。
//
//	{
//	  "type": "ensure_uv",
//	  "name": "检查并按需安装 uv",
//	  "min_version": "0.4.0",                 // 可选，低于该版本视作需要安装/升级
//	  "auto_install": true,                   // 可选，默认 true：检测到缺失/过旧后自动询问安装
//	  "install_command": {                    // 可选，覆盖默认的安装命令
//	    "windows": ["powershell","-NoProfile","-ExecutionPolicy","Bypass","-Command","irm https://astral.sh/uv/install.ps1 | iex"],
//	    "linux":   ["sh","-c","curl -LsSf https://astral.sh/uv/install.sh | sh"],
//	    "darwin":  ["sh","-c","curl -LsSf https://astral.sh/uv/install.sh | sh"]
//	  },
//	  "install_hint": { ... }
//	}
type EnsureUVRule struct {
	MinVersion     string              `json:"min_version,omitempty"`
	AutoInstall    *bool               `json:"auto_install,omitempty"`
	InstallCommand map[string][]string `json:"install_command,omitempty"`
	InstallHint    *InstallHint        `json:"install_hint,omitempty"`
}

// 默认的 uv 安装命令（来源: https://docs.astral.sh/uv/getting-started/installation/）
var defaultUVInstall = map[string][]string{
	"windows": {"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		"irm https://astral.sh/uv/install.ps1 | iex"},
	"linux":  {"sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh"},
	"darwin": {"sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh"},
}

// EnsureUV 是 ensure_uv 规则的处理器。
func EnsureUV(rule install.Rule, ctx *install.HandlerContext) (string, error) {
	var spec EnsureUVRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	autoInstall := true
	if spec.AutoInstall != nil {
		autoInstall = *spec.AutoInstall
	}

	var b strings.Builder

	// 1) 检查 uv 是否存在 + 版本
	ver, path, err := detectUV()
	switch {
	case err == nil && versionOK(ver, spec.MinVersion):
		fmt.Fprintf(&b, "√ uv 已安装 版本=%s 路径=%s\n", ver, path)
		if spec.MinVersion != "" {
			fmt.Fprintf(&b, "    满足最低版本 >= %s\n", spec.MinVersion)
		}
		return strings.TrimRight(b.String(), "\n"), nil

	case err == nil:
		// 装了，但版本不达标
		fmt.Fprintf(&b, "× uv 已安装但版本 %s 低于要求 >= %s\n", ver, spec.MinVersion)

	default:
		fmt.Fprintf(&b, "× 未检测到 uv: %v\n", err)
	}

	// 2) 询问是否自动安装
	if !autoInstall {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv 不可用%s", renderHint(spec.InstallHint, "uv"))
	}
	if ctx == nil || ctx.Confirm == nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv 不可用，且没有交互回调可发起询问%s",
				renderHint(spec.InstallHint, "uv"))
	}

	prompt := "未检测到可用的 uv。是否现在自动下载并安装 uv? [Y/N]: "
	if err == nil { // 装了但过旧
		prompt = fmt.Sprintf("uv 版本 %s 过旧（要求 >= %s），是否自动升级? [Y/N]: ",
			ver, spec.MinVersion)
	}
	yes, askErr := ctx.Confirm(prompt)
	if askErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("读取用户确认失败: %w", askErr)
	}
	if !yes {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("用户拒绝自动安装 uv%s", renderHint(spec.InstallHint, "uv"))
	}

	// 3) 选择并执行安装命令
	cmdLine := pickInstallCmd(spec.InstallCommand, runtime.GOOS)
	if len(cmdLine) == 0 {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("没有可用于当前系统(%s)的 uv 自动安装命令%s",
				runtime.GOOS, renderHint(spec.InstallHint, "uv"))
	}
	fmt.Fprintf(&b, "执行安装命令: %s\n", strings.Join(cmdLine, " "))

	cmd := exec.Command(cmdLine[0], cmdLine[1:]...)
	out, runErr := cmd.CombinedOutput()
	indentInto(&b, string(out))
	if runErr != nil {
		// 如果 uv 是已存在但版本过旧（前面 err == nil），优先建议 `uv self update`
		if err == nil {
			fmt.Fprintf(&b, "尝试使用 uv self update 兜底升级\n")
			selfOut, selfErr := exec.Command("uv", "self", "update").CombinedOutput()
			indentInto(&b, string(selfOut))
			if selfErr != nil {
				return strings.TrimRight(b.String(), "\n"),
					fmt.Errorf("uv 自动升级失败: %v / %v%s",
						runErr, selfErr, renderHint(spec.InstallHint, "uv"))
			}
		} else {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("uv 自动安装失败: %v%s", runErr,
					renderHint(spec.InstallHint, "uv"))
		}
	}

	// 4) 重新检测：直接 LookPath 可能会因为新装的 uv 不在当前进程 PATH 里失败，
	//    因此除了 LookPath，再尝试常见安装路径。
	ver2, path2, err2 := detectUVAfterInstall()
	if err2 != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装命令执行成功，但当前进程仍未找到 uv（可能需要重开终端使 PATH 生效）: %v", err2)
	}
	if !versionOK(ver2, spec.MinVersion) {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("安装/升级后 uv 版本 %s 仍低于要求 >= %s", ver2, spec.MinVersion)
	}
	fmt.Fprintf(&b, "√ uv 已就绪 版本=%s 路径=%s\n", ver2, path2)
	return strings.TrimRight(b.String(), "\n"), nil
}

// detectUV 在 PATH 中查找 uv 并取版本号。
func detectUV() (version, path string, err error) {
	p, err := exec.LookPath("uv")
	if err != nil {
		return "", "", fmt.Errorf("未在 PATH 中找到 uv")
	}
	out, err := exec.Command(p, "--version").CombinedOutput()
	if err != nil {
		return "", p, fmt.Errorf("调用 uv --version 失败: %v", err)
	}
	v := versionRe.FindString(string(out))
	if v == "" {
		return "", p, fmt.Errorf("无法解析 uv 版本输出: %q", strings.TrimSpace(string(out)))
	}
	return v, p, nil
}

// detectUVAfterInstall 在安装脚本执行完后再找一次。
//
// 当前 Go 进程的 PATH 在启动时被快照、之后不会刷新（Windows 注册表更新只对
// 之后由 explorer.exe 启动的进程生效，*nix 的 ~/.bashrc 也只在交互式登录 shell
// 中被加载）；fork 出来的子进程同样继承我们这棵进程树的旧 PATH，所以光靠
// LookPath / "再开一个 shell" 都不可靠。
//
// 因此这里：
//  1. 先 LookPath 试一次；
//  2. 若失败，按 uv 安装脚本的固定输出路径硬探：
//     - Windows: %USERPROFILE%\.local\bin\uv.exe 与 %USERPROFILE%\.cargo\bin\uv.exe
//     - *nix:    $HOME/.local/bin/uv 与 $HOME/.cargo/bin/uv
//  3. 找到后用绝对路径调用 `uv --version` 校验，并把所在目录前置进
//     当前进程的 PATH，这样后续 handler（create_uv_venv / install_uv_package
//     / install_cuda_uv 等）的 LookPath("uv") 也能命中，无需用户重开终端。
func detectUVAfterInstall() (version, path string, err error) {
	if v, p, e := detectUV(); e == nil {
		return v, p, nil
	}

	for _, candidate := range uvFallbackPaths() {
		if !isExecutableFile(candidate) {
			continue
		}
		out, runErr := exec.Command(candidate, "--version").CombinedOutput()
		if runErr != nil {
			continue
		}
		v := versionRe.FindString(string(out))
		if v == "" {
			continue
		}
		// 把目录前置到当前进程的 PATH，影响后续所有 LookPath 与子进程。
		if dir := filepath.Dir(candidate); dir != "" {
			prependPATH(dir)
		}
		return v, candidate, nil
	}

	return "", "", fmt.Errorf("LookPath 与已知安装路径均未找到 uv")
}

// uvFallbackPaths 返回 uv 安装脚本可能放置的固定位置（按优先级）。
func uvFallbackPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	if runtime.GOOS == "windows" {
		return []string{
			filepath.Join(home, ".local", "bin", "uv.exe"),
			filepath.Join(home, ".cargo", "bin", "uv.exe"),
		}
	}
	return []string{
		filepath.Join(home, ".local", "bin", "uv"),
		filepath.Join(home, ".cargo", "bin", "uv"),
	}
}

// isExecutableFile 判断路径是不是一个常规文件（在 *nix 上额外要求带可执行位）。
func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

// prependPATH 把 dir 加到当前进程 PATH 最前；已存在时不重复添加。
func prependPATH(dir string) {
	sep := string(os.PathListSeparator)
	cur := os.Getenv("PATH")
	for _, existing := range strings.Split(cur, sep) {
		if pathsEqual(existing, dir) {
			return
		}
	}
	if cur == "" {
		_ = os.Setenv("PATH", dir)
		return
	}
	_ = os.Setenv("PATH", dir+sep+cur)
}

// pathsEqual 在 Windows 上不区分大小写比较路径。
func pathsEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// versionOK 比较 ver 是否满足 >= min。min 为空时永远满足。
func versionOK(ver, min string) bool {
	if min == "" {
		return true
	}
	clean := versionRe.FindString(ver)
	if clean == "" {
		return false
	}
	cmp, err := compareVersions(clean, min)
	if err != nil {
		return false
	}
	return cmp >= 0
}

// pickInstallCmd 选当前 OS 的安装命令，自定义优先于默认。
func pickInstallCmd(custom map[string][]string, currentOS string) []string {
	if cmd, ok := pickScriptForOS(custom, currentOS); ok && len(cmd) > 0 {
		return cmd
	}
	if cmd, ok := pickScriptForOS(defaultUVInstall, currentOS); ok {
		return cmd
	}
	return nil
}
