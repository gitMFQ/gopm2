package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gitMFQ/gopm2/internal/store"
	"github.com/gitMFQ/gopm2/internal/types"
)

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(filepath.Join(dir, "logs"), st)
}

func waitForState(t *testing.T, s *Supervisor, name string, want types.State, timeout time.Duration) *types.App {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a, err := s.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if a != nil && a.Status.State == want {
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	a, _ := s.Get(name)
	t.Fatalf("app %q did not reach state %q in %v (current: %+v)", name, want, timeout, a)
	return nil
}

func TestStartAndStop(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{Name: "demo", Command: "sleep 60", Restart: types.RestartAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("demo"); err != nil {
		t.Fatal(err)
	}
	a := waitForState(t, s, "demo", types.StateOnline, 5*time.Second)
	pid := a.Status.PID
	if pid <= 0 {
		t.Fatal("pid should be set")
	}

	if err := s.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	a = waitForState(t, s, "demo", types.StateStopped, 5*time.Second)
	if a.Status.PID != 0 {
		t.Fatal("pid should be cleared after stop")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("process %d should be dead after stop", pid)
	}
}

func TestAutoRestartOnKill(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{Name: "victim", Command: "sleep 60", Restart: types.RestartAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("victim"); err != nil {
		t.Fatal(err)
	}
	a := waitForState(t, s, "victim", types.StateOnline, 5*time.Second)
	oldPID := a.Status.PID

	// kill -9 子进程（整个进程组）
	pgid, err := syscall.Getpgid(oldPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// 应在退避（1s）后自动重启，且 pid 变化、restarts+1
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Get("victim")
		if a.Status.State == types.StateOnline && a.Status.PID != oldPID && a.Status.Restarts >= 1 {
			return // 成功
		}
		time.Sleep(100 * time.Millisecond)
	}
	a, _ = s.Get("victim")
	t.Fatalf("app did not auto-restart: %+v", a.Status)
}

func TestNeverRestartPolicy(t *testing.T) {
	s := newTestSupervisor(t)
	// 命令立即失败退出
	_, err := s.Register(types.Spec{Name: "fail", Command: "exit 1", Restart: types.RestartNever})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("fail"); err != nil {
		t.Fatal(err)
	}
	a := waitForState(t, s, "fail", types.StateErrored, 5*time.Second)
	if a.Status.ExitCode != 1 {
		t.Fatalf("exit code should be 1, got %d", a.Status.ExitCode)
	}
	if a.Status.Restarts != 0 {
		t.Fatalf("never policy should not restart, got %d restarts", a.Status.Restarts)
	}
}

func TestOnFailureRestartAndMaxRestarts(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{
		Name:        "flaky",
		Command:     "exit 1",
		Restart:     types.RestartOnFailure,
		MaxRestarts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("flaky"); err != nil {
		t.Fatal(err)
	}
	// 2 次重启（退避 1s+2s）后应停在 errored
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Get("flaky")
		if a.Status.State == types.StateErrored && a.Status.Restarts >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	a, _ := s.Get("flaky")
	t.Fatalf("expected errored with 2 restarts, got %+v", a.Status)
}

func TestRestartCommand(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{Name: "r", Command: "sleep 60", Restart: types.RestartNever})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("r"); err != nil {
		t.Fatal(err)
	}
	a := waitForState(t, s, "r", types.StateOnline, 5*time.Second)
	old := a.Status.PID
	if err := s.Restart("r"); err != nil {
		t.Fatal(err)
	}
	a = waitForState(t, s, "r", types.StateOnline, 5*time.Second)
	if a.Status.PID == old {
		t.Fatal("pid should change after restart")
	}
	if err := syscall.Kill(old, 0); err == nil {
		t.Fatal("old process should be dead")
	}
}

func TestDelete(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{Name: "d", Command: "sleep 60", Restart: types.RestartAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("d"); err != nil {
		t.Fatal(err)
	}
	a := waitForState(t, s, "d", types.StateOnline, 5*time.Second)
	pid := a.Status.PID
	if err := s.Delete("d"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("d"); got != nil {
		t.Fatal("app should be removed from store")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("process should be dead after delete")
	}
}

func TestLogsWritten(t *testing.T) {
	s := newTestSupervisor(t)
	_, err := s.Register(types.Spec{Name: "logger", Command: "echo hello-stdout; echo hello-stderr 1>&2; sleep 5", Restart: types.RestartNever})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("logger"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, "logger", types.StateOnline, 5*time.Second)
	// 等待输出落盘
	time.Sleep(300 * time.Millisecond)
	out, err := os.ReadFile(filepath.Join(s.logsDir, "logger-out.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "hello-stdout\n" {
		t.Fatalf("stdout log = %q", got)
	}
	errLog, err := os.ReadFile(filepath.Join(s.logsDir, "logger-err.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(errLog); got != "hello-stderr\n" {
		t.Fatalf("stderr log = %q", got)
	}
	s.Shutdown()
}

func TestBackoffFor(t *testing.T) {
	cases := map[int]time.Duration{
		0: 1 * time.Second,
		1: 2 * time.Second,
		2: 4 * time.Second,
		3: 8 * time.Second,
		4: 16 * time.Second,
		5: 30 * time.Second,
		6: 30 * time.Second,
	}
	for restarts, want := range cases {
		if got := backoffFor(restarts); got != want {
			t.Fatalf("backoffFor(%d) = %v, want %v", restarts, got, want)
		}
	}
}

func TestMaskEnv(t *testing.T) {
	env := map[string]string{"API_KEY": "secret", "NORMAL": "ok"}
	masked := MaskEnv(env)
	if masked["API_KEY"] != "******" {
		t.Fatal("API_KEY should be masked")
	}
	if masked["NORMAL"] != "ok" {
		t.Fatal("NORMAL should not be masked")
	}
}

// TestSameProcess 验证 C10：pid 复用防护。
func TestSameProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)

	// 启动时间接近当前 → 匹配
	if !sameProcess(pid, time.Now().Add(-100*time.Millisecond)) {
		t.Fatal("fresh process should match recent startedAt")
	}
	// 记录时间远早于真实启动时间（模拟 pid 被复用给了新进程）→ 不匹配
	if sameProcess(pid, time.Now().Add(-10*time.Minute)) {
		t.Fatal("process started now should not match 10-min-old startedAt")
	}
	// startedAt 为零值 → 保守不匹配
	if sameProcess(pid, time.Time{}) {
		t.Fatal("zero startedAt should not match")
	}
	// 不存在的 pid → 不匹配
	if sameProcess(1<<30, time.Now()) {
		t.Fatal("bogus pid should not match")
	}
}

// TestFdNotLeakedAfterExit 验证 C4：进程退出后日志 fd 被关闭。
func TestFdNotLeakedAfterExit(t *testing.T) {
	s := newTestSupervisor(t)
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	base := countFDs()
	// 反复启动/等待退出 10 次，fd 不应持续增长
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("fd%d", i)
		if _, err := s.Register(types.Spec{Name: name, Command: "echo hi", Restart: types.RestartNever}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(name); err != nil {
			t.Fatal(err)
		}
		waitForState(t, s, name, types.StateStopped, 5*time.Second)
	}
	// 等 Wait goroutine 完成关闭
	time.Sleep(500 * time.Millisecond)
	after := countFDs()
	if after > base+6 { // 容忍少量波动（bbolt 等），但不应增长 20 个
		t.Fatalf("fd leak: base=%d after=%d", base, after)
	}
}
