package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"tts-proxy/internal/commands"
)

func main() {
	registry := commands.NewRegistry()
	reader := bufio.NewReader(os.Stdin)
	commands.RegisterBuiltins(registry, reader, os.Stdout)

	fmt.Println("tts-proxy interactive shell")
	fmt.Println("输入 help 查看可用命令，输入 exit 退出。")

	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF (Ctrl+D / Ctrl+Z) -> 平稳退出
			fmt.Println()
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		name, args := parts[0], parts[1:]

		result, err := registry.Run(name, args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			continue
		}

		if result.Output != "" {
			fmt.Println(result.Output)
		}

		if result.ExitLoop {
			fmt.Println("再见!")
			return
		}
	}
}
