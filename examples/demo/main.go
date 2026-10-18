// gopm2 demo：周期性打印日志，支持 --crash-after 触发出错退出，便于测试自动重启。
// 用法: demo [--crash-after N] [--exit-on-stdin] [--interval 1s]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var (
		crashAfter  = flag.Duration("crash-after", 0, "启动后多久主动以非零码退出（测试自动重启）")
		exitOnStdin = flag.Bool("exit-on-stdin", false, "从 stdin 读到 'crash\\n' 时以非零码退出")
		interval    = flag.Duration("interval", 1*time.Second, "打日志间隔")
	)
	flag.Parse()

	start := time.Now()
	counter := 0
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	// 可选读取 stdin 触发崩溃
	if *exitOnStdin {
		go func() {
			s := bufio.NewScanner(os.Stdin)
			for s.Scan() {
				if s.Text() == "crash" {
					fmt.Fprintln(os.Stderr, "demo: stdin told to crash, exiting 1")
					os.Exit(1)
				}
			}
		}()
	}

	fmt.Println("demo started pid=", os.Getpid(), "at", start.Format(time.RFC3339))
	for {
		counter++
		fmt.Printf("tick %d uptime=%s mem=ok\n", counter, time.Since(start).Round(time.Second))
		if *crashAfter > 0 && time.Since(start) >= *crashAfter {
			fmt.Fprintln(os.Stderr, "demo: crash-after reached, exiting 2")
			os.Exit(2)
		}
		<-tick.C
	}
}
