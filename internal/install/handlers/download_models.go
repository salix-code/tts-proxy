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

// DownloadModelsRule 表示「在 venv 中调用 huggingface-cli / modelscope 提前下载模型到本地缓存」的规则。
//
//	{
//	  "type": "download_models",
//	  "name": "预下载 voxcpm 用到的模型",
//	  "venv_dir": ".venv",
//	  "confirm_prompt": "首次运行常因网络问题下载失败，建议先预下载模型。是否现在下载? [Y/N]: ",
//	                                            // 可选；非空时执行前先询问，N 则跳过整步
//	  "env": {                                  // 可选，注入到子进程的环境变量（覆盖父进程同名项）
//	    "HF_ENDPOINT": "https://hf-mirror.com"
//	  },
//	  "models": [                               // 必填，要下载的模型列表
//	    {
//	      "source": "modelscope",
//	      "repo": "OpenBMB/VoxCPM-0.5B",
//	      "local_dir": "models/VoxCPM-0.5B",   // 可选，相对 exe 目录；不填则下到默认缓存
//	      "skip_if_exists": true                // 可选，local_dir 已存在且非空 → 跳过
//	    }
//	  ]
//	}
//
// 行为：
//  1. 定位 venv = <exe 所在目录>/<venv_dir>，并按 source 找对应 cli：
//       hf         → Scripts/huggingface-cli(.exe)（也兼容 hf）
//       modelscope → Scripts/modelscope(.exe)
//  2. confirm_prompt 非空时先整步询问 [Y/N]；N → 跳过整步（成功）。
//  3. 逐个模型下载，每个独立 try：
//       - skip_if_exists=true 且 local_dir 已存在且非空 → 跳过，计入成功；
//       - local_dir 非空时使用 --local-dir / --local_dir 下到固定路径；
//       - 失败不会让整步失败，只在最后汇总「成功 X/Y」。
//       - 全部失败 → 整步失败（error）。
//       - 至少有一个成功 → 整步成功。
type DownloadModelsRule struct {
	VenvDir       string            `json:"venv_dir,omitempty"`
	ConfirmPrompt string            `json:"confirm_prompt,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Models        []ModelSpec       `json:"models"`
	InstallHint   *InstallHint      `json:"install_hint,omitempty"`
}

// ModelSpec 描述一个待下载模型。
type ModelSpec struct {
	Source       string `json:"source"`                   // "hf" | "modelscope"
	Repo         string `json:"repo"`                     // 形如 "OpenBMB/VoxCPM-0.5B"
	LocalDir     string `json:"local_dir,omitempty"`      // 可选；指定后不再依赖默认缓存路径
	SkipIfExists bool   `json:"skip_if_exists,omitempty"` // 可选；local_dir 已存在且非空时跳过
	Note         string `json:"note,omitempty"`           // 可选，给人看的说明
}

// DownloadModels 是 download_models 规则的处理器。
func DownloadModels(rule install.Rule, ctx *install.HandlerContext) (string, error) {
	var spec DownloadModelsRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if len(spec.Models) == 0 {
		return "", fmt.Errorf("models 不能为空")
	}
	if spec.VenvDir == "" {
		spec.VenvDir = ".venv"
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
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("未在 %s 找到虚拟环境（请先执行 create_uv_venv 步骤）", venvDir)
	}

	// 先做一次整步询问；用户不想下，整步直接跳过为成功。
	if strings.TrimSpace(spec.ConfirmPrompt) != "" {
		if ctx == nil || ctx.Confirm == nil {
			fmt.Fprintf(&b, "↺ 没有交互回调可发起询问，跳过整步\n")
			return strings.TrimRight(b.String(), "\n"), nil
		}
		yes, askErr := ctx.Confirm(spec.ConfirmPrompt)
		if askErr != nil {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("读取用户确认失败: %w", askErr)
		}
		if !yes {
			fmt.Fprintf(&b, "↺ 用户选择不下载，跳过整步\n")
			return strings.TrimRight(b.String(), "\n"), nil
		}
	}

	// 注入到每个 cli 子进程的环境变量
	extraEnv := make([]string, 0, len(spec.Env))
	if len(spec.Env) > 0 {
		fmt.Fprintf(&b, "环境变量:\n")
		for k, v := range spec.Env {
			extraEnv = append(extraEnv, k+"="+v)
			fmt.Fprintf(&b, "  %s=%s\n", k, v)
		}
	}

	successCount := 0
	for i, m := range spec.Models {
		idx := i + 1
		label := fmt.Sprintf("[%d/%d] %s (%s)", idx, len(spec.Models), m.Repo, m.Source)
		if m.Note != "" {
			label += " — " + m.Note
		}
		fmt.Fprintf(&b, "\n%s\n", label)

		if strings.TrimSpace(m.Repo) == "" {
			fmt.Fprintf(&b, "× repo 为空，跳过\n")
			continue
		}

		// local_dir 是相对 exe 目录解析的；解析后传给 cli。
		var resolvedLocalDir string
		if strings.TrimSpace(m.LocalDir) != "" {
			resolvedLocalDir = m.LocalDir
			if !filepath.IsAbs(resolvedLocalDir) {
				resolvedLocalDir = filepath.Join(exeDir, resolvedLocalDir)
			}
			fmt.Fprintf(&b, "本地目录: %s\n", resolvedLocalDir)
		}

		// skip_if_exists: 仅在指定了 local_dir 时有效。
		// 判定标准：目录存在且至少含一个非隐藏文件/子目录。
		if m.SkipIfExists && resolvedLocalDir != "" && dirHasContent(resolvedLocalDir) {
			fmt.Fprintf(&b, "↺ 目录已存在且非空，跳过下载（视作成功）\n")
			successCount++
			continue
		}

		cliPath, downloadArgs, err := resolveDownloader(venvDir, m, resolvedLocalDir)
		if err != nil {
			fmt.Fprintf(&b, "× %v\n", err)
			continue
		}
		fmt.Fprintf(&b, "执行: %s %s\n", cliPath, strings.Join(downloadArgs, " "))

		cmd := exec.Command(cliPath, downloadArgs...)
		cmd.Dir = exeDir
		cmd.Env = utf8Env(extraEnv...)
		out, runErr := cmd.CombinedOutput()
		indentInto(&b, string(out))
		if runErr != nil {
			fmt.Fprintf(&b, "× 下载失败: %v\n", runErr)
			continue
		}
		fmt.Fprintf(&b, "√ 下载完成\n")
		successCount++
	}

	fmt.Fprintf(&b, "\n下载结果: %d/%d 成功\n", successCount, len(spec.Models))
	if successCount == 0 {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("所有模型下载均失败%s",
				renderHint(spec.InstallHint, "model"))
	}
	if successCount < len(spec.Models) {
		fmt.Fprintf(&b, "提示: 部分模型未下载成功，voxcpm 首次运行时仍会尝试自动下载\n")
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// dirHasContent 判断 dir 是否存在且包含至少一个文件/子目录。
// 用于 skip_if_exists：只有"已经下了点东西进去"才算可跳过；空目录不算。
func dirHasContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// resolveDownloader 根据 source 在 venv 里找对应 cli，并组装下载子命令。
// localDir 非空时会附加 --local-dir / --local_dir。
// 返回 (cli 绝对路径, 子命令参数, 错误)。
func resolveDownloader(venvDir string, m ModelSpec, localDir string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(m.Source)) {
	case "hf", "huggingface":
		// 优先用新版 hf 命令；兼容旧的 huggingface-cli。
		// 两者参数都是 `download <repo> [--local-dir <dir>]`。
		for _, name := range []string{"hf", "huggingface-cli"} {
			if p, ok := venvCLIPath(venvDir, name); ok {
				args := []string{"download", m.Repo}
				if localDir != "" {
					args = append(args, "--local-dir", localDir)
				}
				return p, args, nil
			}
		}
		return "", nil, fmt.Errorf("未在 venv 中找到 hf / huggingface-cli（请先安装 huggingface_hub[cli]）")
	case "modelscope", "ms":
		if p, ok := venvCLIPath(venvDir, "modelscope"); ok {
			args := []string{"download", "--model", m.Repo}
			if localDir != "" {
				// modelscope cli 用下划线: --local_dir
				args = append(args, "--local_dir", localDir)
			}
			return p, args, nil
		}
		return "", nil, fmt.Errorf("未在 venv 中找到 modelscope（请先安装 modelscope）")
	default:
		return "", nil, fmt.Errorf("未知 source=%q（仅支持 hf / modelscope）", m.Source)
	}
}
