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

// RunVenvCLIRule 表示「在 venv 的 Scripts/bin 目录里找一个可执行文件，按需询问用户后执行它」。
//
//	{
//	  "type": "run_venv_cli",
//	  "name": "试着用 voxcpm 生成第一个音效",
//	  "venv_dir": ".venv",                      // 可选，默认 .venv
//	  "cli": "voxcpm",                          // 必填，Windows 自动加 .exe
//	  "args": ["design", "--text", "Hello from VoxCPM!", "--output", "out.wav"],
//	                                            //   实际命令行参数
//	  "env": {                                  // 可选；注入到子进程的环境变量（覆盖父进程同名项）
//	    "HF_ENDPOINT": "https://hf-mirror.com"   //   例如让 huggingface_hub 走国内镜像
//	  },
//	  "confirm_prompt": "是否创建第一个音效来测试一下? [Y/N]: ",
//	                                            //   可选；非空时执行前先询问 [Y/N]，N 则跳过
//	  "skip_if_missing": true,                  // 可选，默认 true：找不到 cli 就跳过（视作成功）
//	  "output_file": "out.wav",                 // 可选；执行完后展示这个文件是否生成、大小多少
//	  "install_hint": { ... }                   // 可选，运行失败时的提示
//	}
//
// 行为：
//  1. 定位 venv = <exe 所在目录>/<venv_dir>，并在其 Scripts(Windows) / bin(*nix) 子目录下找 cli。
//  2. 找不到时按 skip_if_missing 决定「跳过」或「报错」。
//  3. confirm_prompt 非空时先询问 [Y/N]；用户 N → 跳过（返回成功，不阻塞后续步骤）。
//  4. 在 exe 所在目录执行 cli，stdout/stderr 一并展示。
//  5. 配置了 output_file 时，最后报告它是否产出 / 体积多少（相对 exe 目录）。
type RunVenvCLIRule struct {
	VenvDir       string            `json:"venv_dir,omitempty"`
	CLI           string            `json:"cli"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	ConfirmPrompt string            `json:"confirm_prompt,omitempty"`
	SkipIfMissing *bool             `json:"skip_if_missing,omitempty"`
	OutputFile    string            `json:"output_file,omitempty"`
	InstallHint   *InstallHint      `json:"install_hint,omitempty"`
}

// RunVenvCLI 是 run_venv_cli 规则的处理器。
func RunVenvCLI(rule install.Rule, ctx *install.HandlerContext) (string, error) {
	var spec RunVenvCLIRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if strings.TrimSpace(spec.CLI) == "" {
		return "", fmt.Errorf("cli 字段不能为空")
	}
	if spec.VenvDir == "" {
		spec.VenvDir = ".venv"
	}
	skipIfMissing := true
	if spec.SkipIfMissing != nil {
		skipIfMissing = *spec.SkipIfMissing
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

	var b strings.Builder
	fmt.Fprintf(&b, "venv 目录: %s\n", venvDir)

	if !isExistingVenv(venvDir) {
		// venv 都不在，按 skip_if_missing 决定行为；语义上等同于 cli 缺失。
		if skipIfMissing {
			fmt.Fprintf(&b, "↺ 未找到虚拟环境，跳过 %s\n", spec.CLI)
			return strings.TrimRight(b.String(), "\n"), nil
		}
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("未在 %s 找到虚拟环境", venvDir)
	}

	cliPath, ok := venvCLIPath(venvDir, spec.CLI)
	if !ok {
		if skipIfMissing {
			fmt.Fprintf(&b, "↺ 未在 venv 中找到 %s，跳过\n", spec.CLI)
			return strings.TrimRight(b.String(), "\n"), nil
		}
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("未在 venv 的 Scripts/bin 中找到 %q%s",
				spec.CLI, renderHint(spec.InstallHint, spec.CLI))
	}
	fmt.Fprintf(&b, "找到 cli: %s\n", cliPath)

	// 询问用户（confirm_prompt 为空则跳过询问，直接执行）
	if strings.TrimSpace(spec.ConfirmPrompt) != "" {
		if ctx == nil || ctx.Confirm == nil {
			fmt.Fprintf(&b, "↺ 没有交互回调可发起询问，跳过执行\n")
			return strings.TrimRight(b.String(), "\n"), nil
		}
		yes, askErr := ctx.Confirm(spec.ConfirmPrompt)
		if askErr != nil {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("读取用户确认失败: %w", askErr)
		}
		if !yes {
			fmt.Fprintf(&b, "↺ 用户选择跳过 %s\n", spec.CLI)
			return strings.TrimRight(b.String(), "\n"), nil
		}
	}

	// 真正执行：在 exe 目录运行，输出文件（如 out.wav）落到那里。
	displayCmd := cliPath
	if len(spec.Args) > 0 {
		displayCmd = cliPath + " " + strings.Join(spec.Args, " ")
	}
	fmt.Fprintf(&b, "执行: %s\n", displayCmd)
	fmt.Fprintf(&b, "工作目录: %s\n", exeDir)

	// 把 spec.Env 拍平成 KEY=VALUE 列表附加到 utf8Env() 末尾（覆盖前面的同名项）。
	// 同时回显一遍，便于排错。
	extraEnv := make([]string, 0, len(spec.Env))
	if len(spec.Env) > 0 {
		for k, v := range spec.Env {
			extraEnv = append(extraEnv, k+"="+v)
			fmt.Fprintf(&b, "环境变量: %s=%s\n", k, v)
		}
	}

	cmd := exec.Command(cliPath, spec.Args...)
	cmd.Dir = exeDir
	cmd.Env = utf8Env(extraEnv...)
	out, runErr := cmd.CombinedOutput()
	indentInto(&b, string(out))
	if runErr != nil {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("%s 执行失败: %v%s", spec.CLI, runErr,
				renderHint(spec.InstallHint, spec.CLI))
	}

	// 报告输出文件
	if spec.OutputFile != "" {
		outPath := spec.OutputFile
		if !filepath.IsAbs(outPath) {
			outPath = filepath.Join(exeDir, outPath)
		}
		if info, statErr := os.Stat(outPath); statErr == nil && !info.IsDir() {
			fmt.Fprintf(&b, "√ 已生成 %s（大小 %d 字节）\n", outPath, info.Size())
		} else {
			fmt.Fprintf(&b, "× 命令成功返回，但未在 %s 找到产出文件\n", outPath)
		}
	} else {
		fmt.Fprintf(&b, "√ %s 执行完成\n", spec.CLI)
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// venvCLIPath 在 venv 的 Scripts(Windows) / bin(*nix) 目录里查 cli；Windows 自动尝试加 .exe。
// 返回 (绝对路径, 是否存在)。
func venvCLIPath(venvDir, cli string) (string, bool) {
	var subdir string
	var candidates []string
	if runtime.GOOS == "windows" {
		subdir = "Scripts"
		base := filepath.Join(venvDir, subdir, cli)
		// 用户可能写 "voxcpm" 也可能写 "voxcpm.exe"，两种都试。
		if strings.EqualFold(filepath.Ext(cli), ".exe") {
			candidates = []string{base}
		} else {
			candidates = []string{base + ".exe", base}
		}
	} else {
		subdir = "bin"
		candidates = []string{filepath.Join(venvDir, subdir, cli)}
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
}
