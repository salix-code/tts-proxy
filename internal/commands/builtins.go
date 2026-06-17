package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"tts-proxy/internal/install"
	"tts-proxy/internal/install/handlers"
)

// RegisterBuiltins 注册一组示范命令。
// reader 用于交互式命令（如 install）读取用户输入。
// out 用于实时输出（如 install 的逐步进度），nil 时使用 os.Stdout。
func RegisterBuiltins(r *Registry, reader *bufio.Reader, out io.Writer) {
	if out == nil {
		out = os.Stdout
	}

	r.Register(Command{
		Name: "help",
		Help: "显示所有可用命令",
		Handler: func(args []string) (Result, error) {
			return Result{Output: r.helpText()}, nil
		},
	})

	r.Register(Command{
		Name: "echo",
		Help: "原样回显参数，例: echo hello world",
		Handler: func(args []string) (Result, error) {
			return Result{Output: strings.Join(args, " ")}, nil
		},
	})

	r.Register(Command{
		Name: "upper",
		Help: "把参数转换为大写",
		Handler: func(args []string) (Result, error) {
			return Result{Output: strings.ToUpper(strings.Join(args, " "))}, nil
		},
	})

	r.Register(Command{
		Name: "exit",
		Help: "退出程序",
		Handler: func(args []string) (Result, error) {
			return ResultExit, nil
		},
	})

	r.Register(Command{
		Name: "quit",
		Help: "退出程序（同 exit）",
		Handler: func(args []string) (Result, error) {
			return ResultExit, nil
		},
	})

	r.Register(Command{
		Name: "install",
		Help: "进入安装模式，读取 install.json 并按规则执行（用法: install [path]）",
		Handler: func(args []string) (Result, error) {
			path := "install.json"
			if len(args) > 0 {
				path = args[0]
			}

			cfg, err := install.Load(path)
			if err != nil {
				return Result{}, err
			}

			runner := install.NewRunner()
			runner.Register("check_command", handlers.CheckCommand)
			runner.Register("check_python_package", handlers.CheckPythonPackage)
			runner.Register("check_cuda", handlers.CheckCUDA)
			runner.Register("create_uv_venv", handlers.CreateUVVenv)
			runner.Register("install_uv_package", handlers.InstallUVPackage)
			runner.Register("install_cuda_uv", handlers.InstallCUDAUV)
			runner.Register("ensure_uv", handlers.EnsureUV)
			runner.Register("run_venv_cli", handlers.RunVenvCLI)
			runner.Register("download_models", handlers.DownloadModels)

			confirm := makeYesNoConfirm(reader, out)
			allOK, err := runner.Run(cfg, out, confirm)
			if err != nil {
				return Result{}, err
			}

			summary := "安装步骤全部通过 ✓"
			if !allOK {
				summary = "安装结束（存在未通过或已跳过的步骤）"
			}
			return Result{Output: summary}, nil
		},
	})

	RegisterVoiceCommands(r, out)
}

// makeYesNoConfirm 用同一份 stdin reader 做 [Y/N] 询问，避免与外层 REPL 抢输入。
// 空行/EOF 视为 N（保守处理）。
func makeYesNoConfirm(reader *bufio.Reader, out io.Writer) install.Confirm {
	if reader == nil {
		return nil
	}
	return func(prompt string) (bool, error) {
		fmt.Fprint(out, prompt)
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF / 输入流关闭：当作 N
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}
