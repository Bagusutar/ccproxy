package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		switch a {
		case "--daemon":
			if err := runDaemon(); err != nil {
				fmt.Fprintln(os.Stderr, "daemon:", err)
				os.Exit(1)
			}
			return
		case "--version", "-v":
			fmt.Println("ccproxy", version)
			return
		case "--help", "-h":
			fmt.Print(usage)
			return
		}
	}
	// 开面板顺手清掉上一次升级留下的旧 exe。放在这里而不是 daemon 里：
	// daemon 完全可能就是从那个旧 exe 起来的。
	cleanupInstallLeftovers()
	if err := runGUI(); err != nil {
		fmt.Fprintln(os.Stderr, "gui:", err)
		os.Exit(1)
	}
}

const version = "1.0.0"

const usage = `ccproxy - 让 Claude Code 按模型名自动分流到不同上游

直接双击运行即可打开配置界面。

可选参数:
  --daemon    只运行后台代理,不打开界面(开机自启调用)
  --version   显示版本
`
