package commands

import (
	"fmt"
	"sort"
	"strings"
)

// Result 表示一条命令的执行结果。
type Result struct {
	Output   string // 要打印给用户的内容
	ExitLoop bool   // 是否要求 REPL 退出
}

// ResultExit 是一个哨兵值，命令处理器返回它来请求退出。
var ResultExit = Result{ExitLoop: true}

// Handler 是单条命令的处理函数签名。
type Handler func(args []string) (Result, error)

// Command 描述一条已注册的命令。
type Command struct {
	Name    string
	Help    string
	Handler Handler
}

// Registry 管理所有可用命令。
type Registry struct {
	cmds map[string]Command
}

func NewRegistry() *Registry {
	return &Registry{cmds: make(map[string]Command)}
}

// Register 注册一条命令。
func (r *Registry) Register(c Command) {
	r.cmds[c.Name] = c
}

// Run 查找并执行一条命令。
func (r *Registry) Run(name string, args []string) (Result, error) {
	c, ok := r.cmds[name]
	if !ok {
		return Result{}, fmt.Errorf("未知命令 %q（输入 help 查看可用命令）", name)
	}
	return c.Handler(args)
}

// List 返回按名称排序的命令列表，便于 help 显示。
func (r *Registry) List() []Command {
	out := make([]Command, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// helpText 生成 help 命令的输出。
func (r *Registry) helpText() string {
	var b strings.Builder
	b.WriteString("可用命令:\n")
	for _, c := range r.List() {
		fmt.Fprintf(&b, "  %-10s %s\n", c.Name, c.Help)
	}
	return strings.TrimRight(b.String(), "\n")
}
