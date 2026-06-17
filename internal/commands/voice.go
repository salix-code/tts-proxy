package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// voice.go 实现两个面向用户的命令：
//   generate <name> <description>   创建一个 voice 档案，并合成两段模板音频
//   speek    <name> <content>       读取该 voice，按内容长度选模板做 clone
//
// 目录布局（相对 exe 所在目录）：
//   voices/<name>/audio.json
//   voices/<name>/template1.wav
//   voices/<name>/template2.wav
//   voices/<name>/out_<timestamp>.wav    speek 产物
//
// 与 install 阶段的契约：
//   - voxcpm CLI 已通过 install_uv_package 安装到 .venv
//   - models/VoxCPM2 与 models/ZipEnhancer 已通过 download_models 下载
//   - 全部加 --local-files-only，避免 voxcpm 联网拉模型

// Template 是单条录音模板的元数据。
type Template struct {
	ID        string `json:"id"`         // template1 / template2
	Audio     string `json:"audio"`      // 相对 voices/<name>/ 的音频文件名
	Text      string `json:"text"`       // 模板朗读文本（与 Audio 严格对应；clone 时作 prompt_text）
	SceneDesc string `json:"scene_desc"` // 场景描述：speek 时按内容选模板的依据
}

// VoiceProfile 是 voices/<name>/audio.json 的内容。
type VoiceProfile struct {
	Name        string     `json:"name"`
	Description string     `json:"description"` // generate 时的 --control，告诉模型音色风格
	CreatedAt   string     `json:"created_at"`
	Templates   []Template `json:"templates"`
}

// 内置模板文本与场景描述。和 README 中"录音文本模板"保持一致。
var builtinTemplates = []Template{
	{
		ID:    "template1",
		Audio: "template1.wav",
		Text: "今天天气不错，阳光透过窗户洒进来。" +
			"我准备先去咖啡馆坐一会儿，顺便整理一下这周的工作计划。" +
			"等晚些时候，再去运动场跑两圈。",
		SceneDesc: "短句、日常对话、平静语气；适合 30 字以内的轻松内容",
	},
	{
		ID:    "template2",
		Audio: "template2.wav",
		Text: "大家好，我是一名软件工程师，平时喜欢阅读、跑步和煮咖啡。" +
			"最近在研究语音合成技术，希望能做出一个真实自然的声音助手。" +
			"如果你也对这个方向感兴趣，欢迎一起交流，分享想法和经验。",
		SceneDesc: "中长句、自我介绍式、节奏稳定；适合 30 字以上的连贯叙述",
	},
}

// 选模板的字数门槛（按 rune 数；中文一字 = 一 rune）。
// ≤ 短句门槛 → template1；否则 template2。
const shortTextRuneLimit = 30

// RegisterVoiceCommands 把 generate / speek 注册到 Registry。
func RegisterVoiceCommands(r *Registry, out io.Writer) {
	if out == nil {
		out = os.Stdout
	}

	r.Register(Command{
		Name: "generate",
		Help: "创建音色档案：generate <人名> <描述>，例: generate alice warm female voice",
		Handler: func(args []string) (Result, error) {
			return handleGenerate(args, out)
		},
	})

	r.Register(Command{
		Name: "speek",
		Help: "用已创建的音色朗读：speek <人名> <内容>",
		Handler: func(args []string) (Result, error) {
			return handleSpeek(args, out)
		},
	})

	r.Register(Command{
		Name: "speek-fast",
		Help: "用 sherpa-onnx ZipVoice 克隆朗读（更快、CPU 友好）：speek-fast <人名> <内容>",
		Handler: func(args []string) (Result, error) {
			return handleSpeekFast(args, out)
		},
	})
}

// ─── generate ──────────────────────────────────────────────────────────────

func handleGenerate(args []string, out io.Writer) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("用法: generate <人名> <描述>，例: generate alice warm female voice, calm")
	}
	name := args[0]
	description := strings.Join(args[1:], " ")

	if err := validateName(name); err != nil {
		return Result{}, err
	}

	exeDir, err := exeDirAbs()
	if err != nil {
		return Result{}, err
	}
	voiceDir := filepath.Join(exeDir, "voices", name)
	if err := os.MkdirAll(voiceDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("创建 %s 失败: %w", voiceDir, err)
	}

	cli, err := locateVoxcpm(exeDir)
	if err != nil {
		return Result{}, err
	}

	fmt.Fprintf(out, "音色档案目录: %s\n描述: %s\n", voiceDir, description)

	// 逐个模板调用 voxcpm design 生成音频
	for i, tpl := range builtinTemplates {
		fmt.Fprintf(out, "\n[%d/%d] 生成 %s ...\n", i+1, len(builtinTemplates), tpl.ID)
		audioPath := filepath.Join(voiceDir, tpl.Audio)
		cmd := exec.Command(cli,
			"design",
			"--text", tpl.Text,
			"--control", description,
			"--output", audioPath,
			"--model-path", filepath.Join(exeDir, "models", "VoxCPM2"),
			"--zipenhancer-path", filepath.Join(exeDir, "models", "ZipEnhancer"),
			"--local-files-only",
		)
		cmd.Dir = exeDir
		cmd.Stdout = out
		cmd.Stderr = out
		if runErr := cmd.Run(); runErr != nil {
			return Result{}, fmt.Errorf("voxcpm design 失败 (%s): %w", tpl.ID, runErr)
		}
		if !fileExistsNonEmpty(audioPath) {
			return Result{}, fmt.Errorf("voxcpm 已退出但 %s 未生成或为空", audioPath)
		}
	}

	profile := VoiceProfile{
		Name:        name,
		Description: description,
		CreatedAt:   time.Now().Format(time.RFC3339),
		Templates:   builtinTemplates,
	}
	if err := writeJSON(filepath.Join(voiceDir, "audio.json"), profile); err != nil {
		return Result{}, err
	}

	return Result{Output: fmt.Sprintf("√ 音色档案 %q 已就绪（%d 个模板，目录: %s）",
		name, len(builtinTemplates), voiceDir)}, nil
}

// ─── speek ─────────────────────────────────────────────────────────────────

func handleSpeek(args []string, out io.Writer) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("用法: speek <人名> <内容>")
	}
	name := args[0]
	content := strings.Join(args[1:], " ")

	if err := validateName(name); err != nil {
		return Result{}, err
	}

	exeDir, err := exeDirAbs()
	if err != nil {
		return Result{}, err
	}
	voiceDir := filepath.Join(exeDir, "voices", name)

	profile, err := readProfile(filepath.Join(voiceDir, "audio.json"))
	if err != nil {
		return Result{}, fmt.Errorf("读取音色档案失败: %w（请先 generate %s ...）", err, name)
	}
	if len(profile.Templates) == 0 {
		return Result{}, fmt.Errorf("音色档案 %q 没有任何模板", name)
	}

	tpl := pickTemplate(profile.Templates, content)
	audioPath := filepath.Join(voiceDir, tpl.Audio)
	if !fileExistsNonEmpty(audioPath) {
		return Result{}, fmt.Errorf("模板音频缺失: %s（建议重新 generate %s）", audioPath, name)
	}
	fmt.Fprintf(out, "选用模板: %s — %s\n", tpl.ID, tpl.SceneDesc)

	cli, err := locateVoxcpm(exeDir)
	if err != nil {
		return Result{}, err
	}

	outPath := filepath.Join(voiceDir, fmt.Sprintf("out_%s.wav", time.Now().Format("20060102_150405")))
	cmd := exec.Command(cli,
		"clone",
		"--text", content,
		"--prompt-audio", audioPath,
		"--prompt-text", tpl.Text,
		"--output", outPath,
		"--model-path", filepath.Join(exeDir, "models", "VoxCPM2"),
		"--zipenhancer-path", filepath.Join(exeDir, "models", "ZipEnhancer"),
		"--local-files-only",
	)
	cmd.Dir = exeDir
	cmd.Stdout = out
	cmd.Stderr = out
	if runErr := cmd.Run(); runErr != nil {
		return Result{}, fmt.Errorf("voxcpm clone 失败: %w", runErr)
	}
	if !fileExistsNonEmpty(outPath) {
		return Result{}, fmt.Errorf("voxcpm 已退出但 %s 未生成", outPath)
	}

	return Result{Output: fmt.Sprintf("√ 已生成: %s", outPath)}, nil
}

// pickTemplate 当前策略：按内容字符数（rune）判断。
// 短内容 → template1，长内容 → template2。
// 找不到对应 ID 时回退到列表第一个。
func pickTemplate(tpls []Template, content string) Template {
	want := "template2"
	if utf8.RuneCountInString(content) <= shortTextRuneLimit {
		want = "template1"
	}
	for _, t := range tpls {
		if t.ID == want {
			return t
		}
	}
	return tpls[0]
}

// ─── speek-fast ────────────────────────────────────────────────────────────

// ZipVoice tar.bz2 解压后的顶层目录名（来自 sherpa-onnx 官方 release 命名）。
const zipvoiceModelSubdir = "sherpa-onnx-zipvoice-distill-zh-en-emilia"

// handleSpeekFast 走 sherpa-onnx ZipVoice 路径：复用 voices/<name>/audio.json
// 选模板，把模板音频 + 文字交给 scripts/zipvoice_clone.py 推理。
//
// 用法：speek-fast <人名> <内容>... [--steps N]
//   --steps 可放在任意位置；默认 8。
func handleSpeekFast(args []string, out io.Writer) (Result, error) {
	if len(args) < 2 {
		return Result{}, fmt.Errorf("用法: speek-fast <人名> <内容> [--steps N]")
	}

	rest, steps, err := parseStepsFlag(args)
	if err != nil {
		return Result{}, err
	}
	if len(rest) < 2 {
		return Result{}, fmt.Errorf("用法: speek-fast <人名> <内容> [--steps N]")
	}
	name := rest[0]
	content := strings.Join(rest[1:], " ")

	if err := validateName(name); err != nil {
		return Result{}, err
	}

	exeDir, err := exeDirAbs()
	if err != nil {
		return Result{}, err
	}
	voiceDir := filepath.Join(exeDir, "voices", name)

	profile, err := readProfile(filepath.Join(voiceDir, "audio.json"))
	if err != nil {
		return Result{}, fmt.Errorf("读取音色档案失败: %w（请先 generate %s ...）", err, name)
	}
	if len(profile.Templates) == 0 {
		return Result{}, fmt.Errorf("音色档案 %q 没有任何模板", name)
	}
	tpl := pickTemplate(profile.Templates, content)
	audioPath := filepath.Join(voiceDir, tpl.Audio)
	if !fileExistsNonEmpty(audioPath) {
		return Result{}, fmt.Errorf("模板音频缺失: %s", audioPath)
	}
	fmt.Fprintf(out, "选用模板: %s — %s\n", tpl.ID, tpl.SceneDesc)

	// 校验 sherpa-onnx 模型已经下载完成。
	// vocoder 由 Python 端从模型目录里自动找（distill release 已捆绑），不再单独校验。
	modelDir := filepath.Join(exeDir, "models", "sherpa-zipvoice", zipvoiceModelSubdir)
	if !dirHasContents(modelDir) {
		return Result{}, fmt.Errorf("ZipVoice 模型目录缺失: %s（请重跑 install）", modelDir)
	}

	pyScript := filepath.Join(exeDir, "scripts", "zipvoice_clone.py")
	if !fileExistsNonEmpty(pyScript) {
		return Result{}, fmt.Errorf("缺少推理脚本: %s", pyScript)
	}
	pyExe, err := locateVenvPython(exeDir)
	if err != nil {
		return Result{}, err
	}

	outPath := filepath.Join(voiceDir, fmt.Sprintf("fast_%s.wav", time.Now().Format("20060102_150405")))
	pyArgs := []string{pyScript,
		"--text", content,
		"--prompt-audio", audioPath,
		"--prompt-text", tpl.Text,
		"--model-dir", modelDir,
		"--output", outPath,
		"--num-steps", strconv.Itoa(steps),
	}
	fmt.Fprintf(out, "推理参数: num-steps=%d\n", steps)
	cmd := exec.Command(pyExe, pyArgs...)
	cmd.Dir = exeDir
	cmd.Stdout = out
	cmd.Stderr = out
	if runErr := cmd.Run(); runErr != nil {
		return Result{}, fmt.Errorf("zipvoice_clone.py 执行失败: %w", runErr)
	}
	if !fileExistsNonEmpty(outPath) {
		return Result{}, fmt.Errorf("脚本退出但 %s 未生成", outPath)
	}
	return Result{Output: fmt.Sprintf("√ 已生成: %s", outPath)}, nil
}

// locateVenvPython 在 .venv 中找 python 解释器。
func locateVenvPython(exeDir string) (string, error) {
	var candidate string
	if runtime.GOOS == "windows" {
		candidate = filepath.Join(exeDir, ".venv", "Scripts", "python.exe")
	} else {
		candidate = filepath.Join(exeDir, ".venv", "bin", "python")
	}
	if !fileExistsNonEmpty(candidate) {
		return "", fmt.Errorf("未找到 venv python: %s（请先执行 install）", candidate)
	}
	return candidate, nil
}

// parseStepsFlag 从原始 args 里抽出 --steps N（任意位置），返回剩余位置参数与步数。
// 没指定时返回默认 8；非法整数或超出 [4, 32] 报错。
// 仅识别 --steps；如未来要加更多参数，可在此扩展为通用 flag 解析。
func parseStepsFlag(args []string) (rest []string, steps int, err error) {
	steps = 8
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--steps":
			if i+1 >= len(args) {
				return nil, 0, fmt.Errorf("--steps 需要一个整数参数")
			}
			n, perr := strconv.Atoi(args[i+1])
			if perr != nil {
				return nil, 0, fmt.Errorf("--steps 不是合法整数: %q", args[i+1])
			}
			if n < 4 || n > 32 {
				return nil, 0, fmt.Errorf("--steps 应在 [4, 32]，得到 %d", n)
			}
			steps = n
			i++
		case strings.HasPrefix(a, "--steps="):
			n, perr := strconv.Atoi(strings.TrimPrefix(a, "--steps="))
			if perr != nil {
				return nil, 0, fmt.Errorf("--steps 不是合法整数: %q", a)
			}
			if n < 4 || n > 32 {
				return nil, 0, fmt.Errorf("--steps 应在 [4, 32]，得到 %d", n)
			}
			steps = n
		default:
			rest = append(rest, a)
		}
	}
	return rest, steps, nil
}

// dirHasContents 判断 dir 是否存在且包含至少一个文件/子目录。
func dirHasContents(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// ─── helpers ───────────────────────────────────────────────────────────────

// validateName 限制人名只能用安全字符，避免被当成路径分隔符跳出 voices/。
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("人名不能为空")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_' || r == '-':
		default:
			return fmt.Errorf("人名只能包含字母/数字/下划线/连字符，得到 %q", name)
		}
	}
	return nil
}

// exeDirAbs 返回当前可执行文件所在目录的绝对路径。
// 与 install handlers 中保持一致，所有模型/音频路径都相对此目录解析。
func exeDirAbs() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取 exe 路径失败: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// locateVoxcpm 在 .venv 中找 voxcpm 可执行文件。
func locateVoxcpm(exeDir string) (string, error) {
	var candidate string
	if runtime.GOOS == "windows" {
		candidate = filepath.Join(exeDir, ".venv", "Scripts", "voxcpm.exe")
	} else {
		candidate = filepath.Join(exeDir, ".venv", "bin", "voxcpm")
	}
	if !fileExistsNonEmpty(candidate) {
		return "", fmt.Errorf("未找到 voxcpm CLI: %s（请先执行 install）", candidate)
	}
	return candidate, nil
}

func fileExistsNonEmpty(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() > 0
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 JSON 失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写 %s 失败: %w", path, err)
	}
	return nil
}

func readProfile(path string) (VoiceProfile, error) {
	var p VoiceProfile
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return p, nil
}
