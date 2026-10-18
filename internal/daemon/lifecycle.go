// Package daemon 管理 gopm2d 的生命周期：pidfile、单实例文件锁、ensure 幂等拉起。
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Paths 集中管理数据目录下各类文件路径。
type Paths struct {
	DataDir   string
	Socket    string
	PidFile   string
	WDPidFile string
	LogsDir   string
}

// DefaultPaths 返回默认数据目录（~/.gopm2 或 GOPM2_DATA_DIR）。
func DefaultPaths() Paths {
	dir := os.Getenv("GOPM2_DATA_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".gopm2")
	}
	return PathsFor(dir)
}

// PathsFor 基于指定数据目录构造 Paths。
func PathsFor(dataDir string) Paths {
	return Paths{
		DataDir:   dataDir,
		Socket:    filepath.Join(dataDir, "gopm2.sock"),
		PidFile:   filepath.Join(dataDir, "gopm2d.pid"),
		WDPidFile: filepath.Join(dataDir, "gopm2d-watchdog.pid"),
		LogsDir:   filepath.Join(dataDir, "logs"),
	}
}

// InstanceLock 持有 pidfile 的 flock，保证单实例。
type InstanceLock struct {
	file *os.File
}

// AcquireLock 打开 pidfile 并尝试排他 flock；已有实例持锁时返回错误。
func AcquireLock(pidFile string) (*InstanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another gopm2d instance is running (lock: %s)", pidFile)
	}
	return &InstanceLock{file: f}, nil
}

// WritePID 将 pid 写入 pidfile（需已持锁）。
func (l *InstanceLock) WritePID(pid int) error {
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return err
	}
	_, err := fmt.Fprintf(l.file, "%d\n", pid)
	return err
}

// Release 释放锁并关闭文件。
func (l *InstanceLock) Release() {
	if l.file != nil {
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.file.Close()
	}
}

// ReadPIDFile 读取 pidfile；不存在或内容非法返回 0。
func ReadPIDFile(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// ProcessAlive 校验 pid 存活且 cmdline 包含 match（防 pid 复用误判）。
func ProcessAlive(pid int, match string) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		return false
	}
	return strings.Contains(strings.ReplaceAll(string(data), "\x00", " "), match)
}

// DaemonRunning 判断 daemon 是否在运行（pidfile + cmdline 双重确认）。
func DaemonRunning(p Paths) bool {
	pid := ReadPIDFile(p.PidFile)
	return ProcessAlive(pid, "gopm2d")
}

// Ping 通过 unix socket 做健康检查：GET /healthz。
func Ping(p Paths, timeout time.Duration) error {
	return pingSocket(p.Socket, timeout)
}

// EnsureDaemon 幂等确保 daemon 运行：已运行则直接返回；否则后台拉起并等待 socket 就绪。
// daemonBin 为 gopm2d 二进制路径。返回 (是否新拉起, error)。
func EnsureDaemon(p Paths, daemonBin string) (bool, error) {
	if DaemonRunning(p) {
		if err := Ping(p, 2*time.Second); err == nil {
			return false, nil
		}
		// pidfile 指向活进程但 socket 不通：当作损坏，继续拉起会撞锁，直接报错
		return false, fmt.Errorf("daemon pid alive but socket not responding; consider killing pid %d", ReadPIDFile(p.PidFile))
	}
	// 清理陈旧 socket
	os.Remove(p.Socket)
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return false, err
	}
	logPath := filepath.Join(p.LogsDir, "gopm2d.log")
	if err := os.MkdirAll(p.LogsDir, 0o755); err != nil {
		return false, err
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer lf.Close()

	devNull, _ := os.Open(os.DevNull)
	defer devNull.Close()

	attr := &os.ProcAttr{
		Files: []*os.File{devNull, lf, lf},
		Sys:   &syscall.SysProcAttr{Setsid: true}, // 脱离当前会话，CLI 退出不影响 daemon
	}
	proc, err := os.StartProcess(daemonBin, []string{daemonBin, "--data-dir", p.DataDir}, attr)
	if err != nil {
		return false, fmt.Errorf("spawn gopm2d: %w", err)
	}
	spawnedPID := proc.Pid
	// 不等待，立即释放
	if err := proc.Release(); err != nil {
		return false, err
	}
	// 等待 socket 就绪
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := Ping(p, 500*time.Millisecond); err == nil {
			return true, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 失败兜底：若 daemon 进程已退出，附上退出信息提示（如 TCP 端口冲突等致命错误）
	hint := ""
	if !ProcessAlive(spawnedPID, "gopm2d") {
		hint = fmt.Sprintf("; daemon 进程 %d 已退出，常见原因：端口被占用、数据目录无权限", spawnedPID)
	}
	return true, fmt.Errorf("daemon spawned (pid %d) but socket not ready in 15s; check %s%s", spawnedPID, logPath, hint)
}

// StopDaemon 优雅停止 daemon：读 pidfile 发 SIGTERM，等待退出。
// daemon 已死时清理残留的 pidfile/socket——否则 watchdog 会把
// "pidfile 存在但进程死" 误判为"被杀"而重新拉起，破坏 stop 语义。
func StopDaemon(p Paths, timeout time.Duration) error {
	pid := ReadPIDFile(p.PidFile)
	if pid <= 0 || !ProcessAlive(pid, "gopm2d") {
		// 已停止：清理残留文件，让 watchdog 也退出（它看到 pidfile 不存在即退出）
		os.Remove(p.PidFile)
		os.Remove(p.Socket)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid, "gopm2d") {
			// daemon 优雅退出时会自己删 pidfile/socket；这里兜底清理
			os.Remove(p.PidFile)
			os.Remove(p.Socket)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon pid %d did not exit in %v", pid, timeout)
}
