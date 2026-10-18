// gopm2d 是 gopm2 的常驻守护进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gitMFQ/gopm2/internal/api"
	"github.com/gitMFQ/gopm2/internal/daemon"
	"github.com/gitMFQ/gopm2/internal/service"
	"github.com/gitMFQ/gopm2/internal/store"
	"github.com/gitMFQ/gopm2/internal/supervisor"
)

// defaultTCPAddr 是 Web 面板默认监听地址（仅本机）。
const defaultTCPAddr = "127.0.0.1:9615"

// version 由发布流水线通过 -ldflags "-X main.version=..." 注入。
var version = "0.1.0-dev"

func main() {
	defTCP := defaultTCPAddr
	if env := os.Getenv("GOPM2_TCP_ADDR"); env != "" {
		defTCP = env
	}
	var (
		dataDir     = flag.String("data-dir", "", "数据目录（默认 ~/.gopm2）")
		tcpAddr     = flag.String("tcp", defTCP, "Web 面板监听地址（空=不开启；可用 GOPM2_TCP_ADDR 覆盖默认）")
		watchdog    = flag.Bool("watchdog", false, "以 watchdog 模式运行（内部使用）")
		parentPID   = flag.Int("parent-pid", 0, "watchdog 模式：被监视的 daemon pid")
		showVersion = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("gopm2d", version)
		return
	}

	paths := daemon.DefaultPaths()
	if *dataDir != "" {
		paths = daemon.PathsFor(*dataDir)
	}

	if *watchdog {
		if err := daemon.RunWatchdog(paths, *parentPID, os.Args[0]); err != nil {
			log.Fatalf("watchdog: %v", err)
		}
		return
	}

	if err := run(paths, *tcpAddr); err != nil {
		log.Fatalf("gopm2d: %v", err)
	}
}

func run(paths daemon.Paths, tcpAddr string) error {
	// 单实例锁
	lock, err := daemon.AcquireLock(paths.PidFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := lock.WritePID(os.Getpid()); err != nil {
		return err
	}

	st, err := store.Open(paths.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	sup := supervisor.New(paths.LogsDir, st)
	svc := service.New(sup, st)
	auth := api.NewAuth(st)
	srv := api.New(svc, paths, auth)

	// 首次启动：生成管理员密码并打印 + 落盘一次
	if !auth.HasPassword() {
		pw, err := auth.InitPassword()
		if err != nil {
			return fmt.Errorf("init password: %w", err)
		}
		log.Printf("====================================================")
		log.Printf("  Web 面板初始密码: %s", pw)
		log.Printf("  （仅显示/保存这一次，请妥善保管）")
		log.Printf("====================================================")
		// 密码明文仅写入一次到数据目录，权限 0600，便于用户找回
		pwFile := filepath.Join(paths.DataDir, "initial-password")
		if err := os.WriteFile(pwFile, []byte("Web 面板初始密码: "+pw+"\n（此后不再显示，如丢失请删除 auth 数据重新初始化）\n"), 0o600); err != nil {
			log.Printf("write initial-password: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)

	// Unix socket（CLI 通道）——最先启动，保证 daemon ensure 的 socket 就绪等待
	// 不被后续的 reconcile/resurrect 拖慢
	go func() {
		log.Printf("listening on unix://%s", paths.Socket)
		if err := srv.ListenUnix(ctx); err != nil {
			errCh <- fmt.Errorf("unix listener: %w", err)
		}
	}()

	// TCP（Web 面板）：预绑定端口，被占用时降级为仅 CLI 模式，不影响 daemon 核心功能
	if tcpAddr != "" {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			log.Printf("警告: Web 面板无法监听 %s (%v)，面板不可用，CLI 通道正常", tcpAddr, err)
		} else {
			go func() {
				log.Printf("web panel on http://%s", tcpAddr)
				if err := srv.ServeTCP(ctx, ln); err != nil {
					errCh <- fmt.Errorf("tcp listener: %w", err)
				}
			}()
		}
	}

	// 清理 daemon 重启前的残留子进程（无法接管，按进程组清理）
	if err := sup.Reconcile(); err != nil {
		log.Printf("reconcile: %v", err)
	}

	// 启动时自动 resurrect：恢复 snapshot 中期望运行的进程
	started, errs, err := svc.Resurrect()
	if err != nil {
		log.Printf("resurrect: %v", err)
	}
	for _, e := range errs {
		log.Printf("resurrect: %v", e)
	}
	if len(started) > 0 {
		log.Printf("resurrected: %v", started)
	}

	// 后台周期采样指标（CPU/RSS），List/Get API 只读缓存，避免每请求写盘
	sup.MetricsLoop(ctx, 5*time.Second)

	// 启动 watchdog（若尚未运行），并收割我们 fork 的 watchdog 退出后的僵尸
	var reaper *daemon.WatchdogReaper
	if wdPID, err := daemon.EnsureWatchdog(paths, os.Args[0]); err != nil {
		log.Printf("watchdog: %v", err)
	} else if wdPID > 0 && isOwnChild(wdPID) {
		reaper = daemon.StartWatchdogReaper(wdPID)
	}

	// 信号处理
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("got signal %v, shutting down", sig)
	case err := <-errCh:
		log.Printf("listener error: %v", err)
	}

	cancel()
	sup.Shutdown()
	os.Remove(paths.Socket)
	os.Remove(paths.PidFile) // 删除 pidfile：watchdog 据此判断"主动退出"而非"被杀"
	if reaper != nil {
		reaper.Stop()
	}
	return nil
}

// isOwnChild 判断 pid 是否为当前进程的直接子进程（读 /proc/<pid>/stat 的 ppid 字段）。
// 只有本次 fork 出来的 watchdog 才由本 daemon 收割；旧 daemon 留下的 watchdog 归其原父进程（或 init）。
func isOwnChild(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// comm 字段可能含空格/括号，从最后一个 ')' 之后解析
	i := -1
	for j := len(data) - 1; j >= 0; j-- {
		if data[j] == ')' {
			i = j
			break
		}
	}
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(data[i+2:]))
	if len(fields) < 2 {
		return false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return false
	}
	return ppid == os.Getpid()
}
