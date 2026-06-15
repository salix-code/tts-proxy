package install

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// Rule 是 install.json 中的一条规则。
// 不同的 Type 会派发到不同的 Handler，未来按需要追加字段即可。
// Raw 保留了规则的原始 JSON，供 Handler 自行解析其特有字段。
//
// OS / SkipOS 是所有规则共用的可选字段，用于按操作系统跳过：
//   "os":      ["windows", "linux"]   // 仅在这些系统执行
//   "skip_os": ["darwin"]             // 在这些系统跳过
// 取值即 Go 的 GOOS：windows / linux / darwin / freebsd ...
// 别名: "mac" / "macos" → darwin, "win" → windows。
type Rule struct {
	Type   string   `json:"type"`
	Name   string   `json:"name,omitempty"`
	OS     []string `json:"os,omitempty"`
	SkipOS []string `json:"skip_os,omitempty"`
	// Fatal 为 true 时，本规则失败将直接终止安装，不再询问用户是否继续。
	Fatal bool `json:"fatal,omitempty"`
	Raw   json.RawMessage
}

// UnmarshalJSON 在解析常规字段的同时把整段 JSON 存到 Raw 里。
func (r *Rule) UnmarshalJSON(data []byte) error {
	type alias Rule
	aux := &struct{ *alias }{alias: (*alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	r.Raw = append([]byte(nil), data...)
	return nil
}

// shouldSkip 根据 OS / SkipOS 决定是否跳过当前规则；返回 (skip, reason)。
func (r *Rule) shouldSkip(currentOS string) (bool, string) {
	if len(r.SkipOS) > 0 {
		for _, s := range r.SkipOS {
			if matchOS(s, currentOS) {
				return true, fmt.Sprintf("当前系统 %s 命中 skip_os", currentOS)
			}
		}
	}
	if len(r.OS) > 0 {
		for _, s := range r.OS {
			if matchOS(s, currentOS) {
				return false, ""
			}
		}
		return true, fmt.Sprintf("当前系统 %s 不在 os 白名单 %v 中", currentOS, r.OS)
	}
	return false, ""
}

// matchOS 把别名归一化后比较。
func matchOS(name, current string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mac", "macos", "osx":
		return current == "darwin"
	case "win":
		return current == "windows"
	default:
		return strings.EqualFold(name, current)
	}
}

// Config 是 install.json 的根结构。
type Config struct {
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Rules       []Rule `json:"rules"`
}

// Load 从指定路径读取并解析 install.json。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return &cfg, nil
}

// Handler 是单条规则的处理函数。
// 返回向用户展示的字符串和错误。
// ctx 提供 Confirm（询问 [Y/N]）与 Out（实时输出）等交互能力，
// 当 handler 想在内部完成「失败 → 询问 → 自动修复」流程时使用；不需要时可忽略。
type Handler func(rule Rule, ctx *HandlerContext) (string, error)

// HandlerContext 把 Runner 的交互能力暴露给 Handler。
// 暂时只包含两项最常用的：用户确认与实时输出。
type HandlerContext struct {
	Confirm Confirm
	Out     io.Writer
}

// Confirm 在某条规则失败后由 Runner 调用，让上层决定继续还是结束。
// 实参 prompt 形如「是否已经处理好？继续下一步? [Y/N]:」。
// 返回 true 表示继续后续规则，false 表示中止。
type Confirm func(prompt string) (bool, error)

// Runner 维护「规则类型 -> 处理器」映射。
type Runner struct {
	handlers map[string]Handler
}

func NewRunner() *Runner {
	return &Runner{handlers: make(map[string]Handler)}
}

// Register 注册某种规则类型的处理器。重复注册同名类型会覆盖。
func (r *Runner) Register(typ string, h Handler) {
	r.handlers[typ] = h
}

// Run 顺序执行配置中的所有规则，输出实时写到 out。
// 当某一条规则失败时调用 confirm 询问用户是否继续；confirm 为 nil 等同于「失败即中止」。
// 返回值是「是否所有已执行规则均通过」与遇到的第一个致命错误（如 confirm 自身报错）。
func (r *Runner) Run(cfg *Config, out io.Writer, confirm Confirm) (allOK bool, err error) {
	return r.runWithOS(cfg, runtime.GOOS, out, confirm)
}

func (r *Runner) runWithOS(cfg *Config, currentOS string, out io.Writer, confirm Confirm) (bool, error) {
	if out == nil {
		out = io.Discard
	}

	fmt.Fprintf(out, "进入安装模式 (version=%s, rules=%d)\n", cfg.Version, len(cfg.Rules))
	if cfg.Description != "" {
		fmt.Fprintf(out, "描述: %s\n", cfg.Description)
	}
	if len(cfg.Rules) == 0 {
		fmt.Fprintln(out, "(install.json 中暂无规则，待添加)")
		return true, nil
	}

	allOK := true
	for i, rule := range cfg.Rules {
		idx := i + 1
		label := rule.Type
		if rule.Name != "" {
			label = fmt.Sprintf("%s (%s)", rule.Type, rule.Name)
		}

		if skip, reason := rule.shouldSkip(currentOS); skip {
			fmt.Fprintf(out, "[%d] 跳过 %s — %s\n", idx, label, reason)
			continue
		}

		h, ok := r.handlers[rule.Type]
		if !ok {
			fmt.Fprintf(out, "[%d] 跳过未知规则类型: %s\n", idx, label)
			continue
		}

		fmt.Fprintf(out, "[%d] 执行 %s\n", idx, label)
		ctx := &HandlerContext{Confirm: confirm, Out: out}
		text, runErr := h(rule, ctx)
		writeIndented(out, text)

		if runErr == nil {
			continue
		}

		// 失败：打印错误细节 + 让用户决定是否继续
		allOK = false
		fmt.Fprintf(out, "    !! 失败: %v\n", runErr)
		fmt.Fprintf(out, "    步骤 [%d] %s 未通过；请按上面的提示处理后再回到这里。\n", idx, label)

		if rule.Fatal {
			fmt.Fprintf(out, "    步骤 [%d] %s 是必须项 (fatal=true)，安装中止。\n", idx, label)
			return allOK, nil
		}

		if confirm == nil {
			fmt.Fprintln(out, "    （未提供交互回调，默认中止）")
			return allOK, nil
		}

		prompt := fmt.Sprintf("步骤 [%d] %s 是否已安装/处理完成? 继续下一步? [Y/N]: ", idx, label)
		ok, askErr := confirm(prompt)
		if askErr != nil {
			return allOK, fmt.Errorf("读取用户确认失败: %w", askErr)
		}
		if !ok {
			fmt.Fprintln(out, "    用户选择结束安装。")
			return allOK, nil
		}
		fmt.Fprintln(out, "    用户选择继续，进入下一步。")
	}

	return allOK, nil
}

// writeIndented 把规则处理器的多行输出每行加 4 空格缩进后写出。
func writeIndented(w io.Writer, text string) {
	if text == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
