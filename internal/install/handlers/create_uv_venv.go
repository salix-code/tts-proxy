package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tts-proxy/internal/install"
)

// CreateUVVenvRule 表示「使用 uv 在 exe 所在目录创建 Python 虚拟环境」的规则。
//
//	{
//	  "type": "create_uv_venv",
//	  "name": "使用 uv 创建 Python 3.11 虚拟环境",
//	  "python": "3.11",          // 必填，传给 uv venv --python 的参数
//	  "dir": ".venv",            // 可选，虚拟环境目录名（相对 exe 目录），默认 .venv
//	  "force": false,            // 可选，true 时覆盖已存在的目录
//	  "install_hint": { ... }    // 可选，失败时的提示
//	}
//
// 行为：
//   1. 确定虚拟环境路径 = <exe 所在目录>/<dir>
//   2. 若该目录已存在且看起来是合法 venv（含 pyvenv.cfg），则跳过创建并视为成功；
//      存在但不是 venv，会报错（除非 force=true）。
//   3. 调用 `uv venv --python <python> <abs_path>`，把 stdout/stderr 一并展示。
type CreateUVVenvRule struct {
	Python      string       `json:"python"`
	Dir         string       `json:"dir,omitempty"`
	Force       bool         `json:"force,omitempty"`
	InstallHint *InstallHint `json:"install_hint,omitempty"`
}

// CreateUVVenv 是 create_uv_venv 规则的处理器。
func CreateUVVenv(rule install.Rule, _ *install.HandlerContext) (string, error) {
	var spec CreateUVVenvRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if strings.TrimSpace(spec.Python) == "" {
		return "", fmt.Errorf("python 字段不能为空（如 \"3.11\"）")
	}
	if spec.Dir == "" {
		spec.Dir = ".venv"
	}

	// 1) 确认 uv 可用（前面已检查过，这里只是兜底）
	uvPath, err := exec.LookPath("uv")
	if err != nil {
		return "", fmt.Errorf("未找到 uv（请先完成上一步 uv 检查）%s",
			renderHint(spec.InstallHint, "uv"))
	}

	// 2) 计算 exe 所在目录
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exePath); rerr == nil {
		exePath = resolved
	}
	exeDir := filepath.Dir(exePath)
	venvDir := filepath.Join(exeDir, spec.Dir)

	var b strings.Builder
	fmt.Fprintf(&b, "uv 路径: %s\n", uvPath)
	fmt.Fprintf(&b, "exe 目录: %s\n", exeDir)
	fmt.Fprintf(&b, "venv 目录: %s\n", venvDir)

	// 3) 已存在则按情况处理
	if info, statErr := os.Stat(venvDir); statErr == nil {
		if !info.IsDir() {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("%s 已存在且不是目录", venvDir)
		}
		if isExistingVenv(venvDir) && !spec.Force {
			fmt.Fprintf(&b, "↺ 已检测到现有虚拟环境（含 pyvenv.cfg），跳过创建\n")
			return strings.TrimRight(b.String(), "\n"), nil
		}
		if !spec.Force {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("目录 %s 已存在但不是 venv（设置 \"force\": true 可覆盖）", venvDir)
		}
		fmt.Fprintf(&b, "force=true：将覆盖已存在的目录\n")
	} else if !os.IsNotExist(statErr) {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("检查 %s 状态失败: %w", venvDir, statErr)
	}

	// 4) 调用 uv venv
	args := []string{"venv", "--python", spec.Python}
	if spec.Force {
		args = append(args, "--force")
	}
	args = append(args, venvDir)

	fmt.Fprintf(&b, "执行: uv %s\n", strings.Join(args, " "))

	cmd := exec.Command(uvPath, args...)
	cmd.Dir = exeDir
	cmd.Env = utf8Env()
	out, runErr := cmd.CombinedOutput()
	output := strings.TrimRight(string(out), "\n")
	if output != "" {
		for _, line := range strings.Split(output, "\n") {
			fmt.Fprintf(&b, "  > %s\n", line)
		}
	}
	if runErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv venv 执行失败: %v%s", runErr,
				renderHint(spec.InstallHint, "Python "+spec.Python))
	}

	// 5) 验证创建成功
	if !isExistingVenv(venvDir) {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("uv venv 执行后仍未在 %s 看到 pyvenv.cfg", venvDir)
	}
	fmt.Fprintf(&b, "√ 虚拟环境已就绪: %s\n", venvDir)

	return strings.TrimRight(b.String(), "\n"), nil
}

// isExistingVenv 通过查找 pyvenv.cfg 判断目录是否已经是 Python 虚拟环境。
func isExistingVenv(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pyvenv.cfg"))
	return err == nil
}
