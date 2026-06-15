package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// InstallHint 描述某个依赖未满足时如何提示用户。
// 目前被 check_command 与 check_python_package 复用。
type InstallHint struct {
	Intro    string           `json:"intro,omitempty"`
	Commands []InstallCommand `json:"commands,omitempty"`
	Docs     string           `json:"docs,omitempty"`
}

// InstallCommand 是一条建议的安装命令。
type InstallCommand struct {
	Label string `json:"label,omitempty"`
	Cmd   string `json:"cmd"`
}

// String 渲染成可读文本，便于追加到错误信息后面。
// fallback 在 Intro 为空时使用，例如 "请先安装 torch:".
func (h *InstallHint) String(fallback string) string {
	if h == nil {
		return ""
	}
	var b strings.Builder
	switch {
	case h.Intro != "":
		fmt.Fprintf(&b, "\n%s", h.Intro)
	case fallback != "":
		fmt.Fprintf(&b, "\n%s", fallback)
	}
	for _, c := range h.Commands {
		if c.Label != "" {
			fmt.Fprintf(&b, "\n  [%s] %s", c.Label, c.Cmd)
		} else {
			fmt.Fprintf(&b, "\n  %s", c.Cmd)
		}
	}
	if h.Docs != "" {
		fmt.Fprintf(&b, "\n  参考: %s", h.Docs)
	}
	return b.String()
}

// StringList 既能从 JSON 字符串解析（向后兼容旧的 "import_as": "torch"），
// 也能从 JSON 数组解析（新的 "import_as": ["torch", "voxcpm"]）。
type StringList []string

// UnmarshalJSON 接受单字符串或字符串数组。
func (s *StringList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	switch data[0] {
	case '"':
		var single string
		if err := json.Unmarshal(data, &single); err != nil {
			return err
		}
		if single == "" {
			*s = nil
			return nil
		}
		*s = StringList{single}
		return nil
	case '[':
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*s = StringList(list)
		return nil
	default:
		return fmt.Errorf("StringList 仅接受字符串或字符串数组，得到: %s", string(data))
	}
}

// utf8Env 返回带 UTF-8 输出强制项的环境变量（os.Environ() + PYTHONUTF8 / PYTHONIOENCODING）。
// 在 Windows GBK 控制台下，避免 Python 子进程报错时输出 "锟斤拷" 之类的乱码。
// extra 中的条目会附加在末尾、覆盖前面的同名项。
func utf8Env(extra ...string) []string {
	env := append(os.Environ(),
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
	)
	return append(env, extra...)
}

