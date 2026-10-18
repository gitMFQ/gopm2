package daemon

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// watchdogInterval 是 watchdog 探测父进程的时间间隔。
const watchdogInterval = 2 * time.Second

// WatchdogReaper 周期性收割"已退出的 watchdog 子进程"，防止僵尸堆积。
// 重要：只能 Wait4 指定 pid——用 Wait4(-1) 会抢走业务子进程的退出状态，
// 导致 exec.Cmd.Wait 报 ECHILD、退出码丢失。
type WatchdogReaper struct {
	wdPID int
	stop  chan struct{}
	done  chan struct{}
}

// StartWatchdogReaper 启动针对单个 watchdog pid 的收割循环。
func StartWatchdogReaper(wdPID int) *WatchdogReaper {
	r := &WatchdogReaper{wdPID: wdPID, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				// 退出前做最后一次非阻塞收割
				var st unix.WaitStatus
				unix.Wait4(r.wdPID, &st, unix.WNOHANG, nil)
				return
			case <-ticker.C:
				var st unix.WaitStatus
				pid, err := unix.Wait4(r.wdPID, &st, unix.WNOHANG, nil)
				if err != nil || pid > 0 {
					// 已收割或进程不存在：停止收割循环
					return
				}
			}
		}
	}()
	return r
}

// Stop 停止收割循环并等待退出。
func (r *WatchdogReaper) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

// EnsureWatchdog 若 watchdog 未运行则 fork 一个（daemon 启动时调用）。
// 返回 watchdog 的 pid（已在运行返回既有 pid；fork 失败返回 0 和错误）。
func EnsureWatchdog(p Paths, daemonBin string) (int, error) {
	wdPID := ReadPIDFile(p.WDPidFile)
	if wdPID > 0 && ProcessAlive(wdPID, "--watchdog") {
		// 已在运行——但注意：它不是本进程的子进程，无法收割，由旧 daemon 负责
		return wdPID, nil
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()
	attr := &os.ProcAttr{
		Files: []*os.File{devNull, devNull, devNull},
		Sys:   &syscall.SysProcAttr{Setsid: true}, // 脱离 daemon 的会话与进程组
	}
	proc, err := os.StartProcess(daemonBin,
		[]string{daemonBin, "--watchdog", "--parent-pid", fmt.Sprintf("%d", os.Getpid()), "--data-dir", p.DataDir},
		attr)
	if err != nil {
		return 0, fmt.Errorf("spawn watchdog: %w", err)
	}
	pid := proc.Pid
	if err := proc.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}

// RunWatchdog 是 watchdog 主循环：探测 daemon，挂掉则拉起。
// 该函数不返回（除非致命错误），由 gopm2d --watchdog 调用。
func RunWatchdog(p Paths, parentPID int, daemonBin string) error {
	if parentPID <= 0 {
		return fmt.Errorf("--parent-pid required")
	}
	// 写自己的 pidfile
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p.WDPidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(p.WDPidFile)

	for {
		time.Sleep(watchdogInterval)
		// pidfile 不存在 → daemon 主动退出（优雅退出会删 pidfile）→ watchdog 随之退出
		filePID := ReadPIDFile(p.PidFile)
		if filePID == 0 {
			return nil
		}
		if ProcessAlive(filePID, "gopm2d") {
			continue
		}
		// daemon 被杀（pidfile 残留但进程死了）：拉起
		// EnsureDaemon 内部有 flock 单实例保护，天然防惊群/重复拉起
		_, _ = EnsureDaemon(p, daemonBin)
	}
}
