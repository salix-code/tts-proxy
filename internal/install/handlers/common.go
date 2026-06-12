package handlers

import (
	"fmt"
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
