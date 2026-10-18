package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchdogReaperReapsZombie 验证 C3 修复：
// StartWatchdogReaper 能收割指定 pid 的僵尸子进程（且不误伤其他进程）。
func TestWatchdogReaperReapsZombie(t *testing.T) {
	// 启动一个立即退出的子进程，不调用 Wait，让它变成僵尸
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := cmd.Process.Pid

	// 等它变成僵尸
	deadline := time.Now().Add(5 * time.Second)
	zombie := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/proc/" + itoa(pid) + "/stat")
		if err != nil {
			break
		}
		if i := lastParenIdx(data); i >= 0 && i+2 < len(data) && data[i+2] == 'Z' {
			zombie = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !zombie {
		t.Fatal("child did not become zombie (test setup issue)")
	}

	reaper := StartWatchdogReaper(pid)
	defer reaper.Stop()

	// 用 /proc/<pid> 是否消失判断收割完成（僵尸仍有 /proc 条目；被收割后消失）。
	// 注意：不能用 Wait4 轮询——那会把僵尸抢走，与 reaper 竞争。
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + itoa(pid)); os.IsNotExist(err) {
			return // 已被 reaper 收割
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("reaper did not reap zombie within 15s")
}

func lastParenIdx(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ')' {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestStopDaemonCleansStalePidFile 验证 C11 修复：
// daemon 已死但 pidfile 残留时，StopDaemon 应清理 pidfile 与 socket，
// 使 watchdog 不会误判"被杀"而重新拉起。
func TestStopDaemonCleansStalePidFile(t *testing.T) {
	dir := t.TempDir()
	p := PathsFor(dir)

	// 写一个指向不存在进程的 pidfile（99999999 几乎不可能存在）
	if err := os.WriteFile(p.PidFile, []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StopDaemon(p, 2*time.Second); err != nil {
		t.Fatalf("StopDaemon on stale pid: %v", err)
	}
	if _, err := os.Stat(p.PidFile); !os.IsNotExist(err) {
		t.Fatal("stale pidfile should be removed")
	}
	if _, err := os.Stat(p.Socket); !os.IsNotExist(err) {
		t.Fatal("stale socket should be removed")
	}
}

// TestProcessAliveCmdlineMatch 验证 cmdline 匹配防 pid 误判。
func TestProcessAliveCmdlineMatch(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pid := cmd.Process.Pid

	// Start() 返回时子进程可能尚未完成 execve，/proc/<pid>/cmdline 会短暂
	// 仍是父进程（测试二进制）的内容。轮询等待 exec 完成，避免竞态误判。
	deadline := time.Now().Add(5 * time.Second)
	for !ProcessAlive(pid, "sleep") {
		if time.Now().After(deadline) {
			t.Fatal("sleep process never matched 'sleep' within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if ProcessAlive(pid, "gopm2d-nonexistent") {
		t.Fatal("sleep should not match 'gopm2d-nonexistent'")
	}
	if ProcessAlive(0, "anything") || ProcessAlive(-1, "anything") {
		t.Fatal("invalid pid should not be alive")
	}
}

// TestAcquireLockExclusive 验证 flock 单实例。
func TestAcquireLockExclusive(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "test.pid")
	l1, err := AcquireLock(pf)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	if err := l1.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if got := ReadPIDFile(pf); got != os.Getpid() {
		t.Fatalf("pidfile = %d, want %d", got, os.Getpid())
	}
	// 第二次加锁应失败
	if _, err := AcquireLock(pf); err == nil {
		t.Fatal("second AcquireLock should fail while first holds it")
	}
}
