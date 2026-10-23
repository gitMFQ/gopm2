package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gitMFQ/gopm2/internal/types"
)

// writeSelfRestartScript 写一个模拟 dshmarket 自重启的脚本：
// 第一代创建标记、detach 出第二代（刻意换一个 cwd，模拟 dshmarket 用
// dirname(entry) 当 cwd 的真实行为）、稍后以退出码 0 退出；
// 第二代长期驻留，并把每一代的 pid 追加到 <dir>/pids。
// 两代的 argv 完全一致（这一点正是替身识别的基础）。
func writeSelfRestartScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "svc.sh")
	body := `#!/bin/sh
# $1 = run, $2 = 状态目录
echo $$ >> "$2/pids"
if [ -f "$2/spawned" ]; then
  # 第二代：长驻
  while true; do sleep 1; done
fi
: > "$2/spawned"
# 刻意在 / 下拉起替身：cwd 与原进程不同，验证 cwd 不是硬性条件
(cd / && setsid "$0" run "$2" >/dev/null 2>&1 &)
sleep 0.4
exit 0
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAdoptSelfRestartedProcess 覆盖用户报告的场景：应用自己换了替身，
// gopm2 必须接管它而不是再 spawn 第二个（否则两个进程抢同一端口）。
func TestAdoptSelfRestartedProcess(t *testing.T) {
	s := newTestSupervisor(t)
	s.adoptMinUptime = 0 // 测试进程存活时间很短，关掉时长门槛

	dir := t.TempDir()
	script := writeSelfRestartScript(t, dir)

	_, err := s.Register(types.Spec{
		Name:    "svc",
		Command: script,
		Args:    []string{"run", dir},
		Cwd:     dir,
		Restart: types.RestartAlways,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("svc"); err != nil {
		t.Fatal(err)
	}
	first := waitForState(t, s, "svc", types.StateOnline, 5*time.Second)
	firstPID := first.Status.PID

	// 第一代退出、替身出现后应被接管：状态回到 online，PID 变化，标记 Adopted
	deadline := time.Now().Add(20 * time.Second)
	var got *types.App
	for time.Now().Before(deadline) {
		a, err := s.Get("svc")
		if err != nil {
			t.Fatal(err)
		}
		if a.Status.Adopted && a.Status.State == types.StateOnline && a.Status.PID != firstPID {
			got = a
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got == nil {
		a, _ := s.Get("svc")
		t.Fatalf("app was not adopted from its own replacement: %+v", a.Status)
	}
	if got.Status.PID <= 0 {
		t.Fatalf("adopted PID should be set, got %+v", got.Status)
	}

	// 关键断言：没有第三个进程——gopm2 没有自己再拉起一份
	pids := readPIDLines(t, filepath.Join(dir, "pids"))
	if len(pids) != 2 {
		t.Fatalf("expected exactly 2 generations (old + replacement), got %d: %v", len(pids), pids)
	}
	if got.Status.PID != pids[1] {
		t.Fatalf("supervised PID %d should be the replacement %d", got.Status.PID, pids[1])
	}
	// 接管不该被记成一次 gopm2 重启
	if got.Status.Restarts != 0 {
		t.Fatalf("adoption should not count as a restart, got %d", got.Status.Restarts)
	}

	// 停止必须能真正干掉替身（它是 setsid 出来的，按进程组清理）
	adopted := got.Status.PID
	if err := s.Stop("svc"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, "svc", types.StateStopped, 10*time.Second)
	if err := syscall.Kill(adopted, 0); err == nil {
		t.Fatalf("adopted process %d should be dead after stop", adopted)
	}
	a, _ := s.Get("svc")
	if a.Status.Adopted {
		t.Fatal("Adopted must be cleared after stop")
	}
	if a.Status.PID != 0 {
		t.Fatalf("PID must be cleared after stop, got %d", a.Status.PID)
	}
}

// TestAdoptWindowExpiryFallsBackToRestart 验证窗口内没有替身时会回退到自己重启。
func TestAdoptWindowExpiryFallsBackToRestart(t *testing.T) {
	s := newTestSupervisor(t)
	s.adoptMinUptime = 0

	_, err := s.Register(types.Spec{
		Name:    "victim",
		Command: "sleep",
		Args:    []string{"60"},
		Restart: types.RestartAlways,
		// 保留默认接管窗口，但把进程"礼貌终止"（SIGTERM）后不存在替身，
		// 因此必须在窗口用尽后回退到正常重启。
		AdoptWindow: types.Duration(500 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("victim"); err != nil {
		t.Fatal(err)
	}
	first := waitForState(t, s, "victim", types.StateOnline, 5*time.Second)
	firstPID := first.Status.PID

	// 外部 SIGTERM：形态上"像交接"，但没有人接替 → 不能卡死，必须自己重启
	if err := syscall.Kill(firstPID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Get("victim")
		if a.Status.State == types.StateOnline && a.Status.PID != firstPID && a.Status.PID > 0 {
			if a.Status.Adopted {
				t.Fatal("there was no replacement; it must be a normal restart, not an adoption")
			}
			if a.Status.Restarts < 1 {
				t.Fatalf("normal restart should be counted, got %d", a.Status.Restarts)
			}
			s.Stop("victim")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	a, _ := s.Get("victim")
	t.Fatalf("app did not fall back to a normal restart: %+v", a.Status)
}

// TestCrashDoesNotWaitForAdoptWindow 验证崩溃恢复不会被接管窗口拖慢：
// 非零退出码不是"交接"形态，必须走原有退避立即重启。
func TestCrashDoesNotWaitForAdoptWindow(t *testing.T) {
	s := newTestSupervisor(t)
	s.adoptMinUptime = 0

	_, err := s.Register(types.Spec{
		Name:    "crashy",
		Command: "sh",
		Args:    []string{"-c", "exit 1"},
		Restart: types.RestartOnFailure,
		// AdoptWindow 留空 → 默认 10s；崩溃路径不应等它
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := s.Start("crashy"); err != nil {
		t.Fatal(err)
	}

	// 非零退出 → 首次重启应在退避（1s）后很快发生，绝不会等到 10s 窗口用尽
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Get("crashy")
		if a.Status.Restarts >= 1 {
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("crash restart took %v — it must not wait the adopt window", elapsed)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	a, _ := s.Get("crashy")
	t.Fatalf("crash was not restarted: %+v", a.Status)
}

// TestNeverPolicyStillTracksReplacement 验证 restart=never 时也不会"谎报停止"：
// 应用已经把替身拉起来了，gopm2 应当继续守护它（接管不是重启）。
func TestNeverPolicyStillTracksReplacement(t *testing.T) {
	s := newTestSupervisor(t)
	s.adoptMinUptime = 0

	dir := t.TempDir()
	script := writeSelfRestartScript(t, dir)
	_, err := s.Register(types.Spec{
		Name:    "svc",
		Command: script,
		Args:    []string{"run", dir},
		Cwd:     dir,
		Restart: types.RestartNever,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("svc"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, "svc", types.StateOnline, 5*time.Second)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Get("svc")
		if a.Status.Adopted && a.Status.State == types.StateOnline {
			s.Stop("svc")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	a, _ := s.Get("svc")
	t.Fatalf("replacement was left unmanaged under restart=never: %+v", a.Status)
}

// TestAdoptedLogMirror 验证接管进程的输出会被跟随镜像进 gopm2 的日志，
// 使 `gopm2 logs` 在接管之后依然有内容。
func TestAdoptedLogMirror(t *testing.T) {
	s := newTestSupervisor(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "svc.log")

	// 先有一段"历史"，再让子进程持续追加 → 镜像应先补历史、再跟随新增
	if err := os.WriteFile(target, []byte("history-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 0.6; echo new-line; sleep 3")
	cmd.Stdout = mustOpen(t, target)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	pid := cmd.Process.Pid
	time.Sleep(150 * time.Millisecond)

	stop := s.startAdoptedLogMirror("svc", pid)
	defer stop()

	logPath := filepath.Join(s.logsDir, "svc-out.log")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), "history-line") && strings.Contains(string(data), "new-line") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, _ := os.ReadFile(logPath)
	t.Fatalf("mirrored log missing content, got: %q", string(data))
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// readPIDLines 读取 pid 文件里每一代记录的 pid。
func readPIDLines(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []int
	for _, line := range strings.Fields(string(raw)) {
		n := 0
		for _, c := range line {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			out = append(out, n)
		}
	}
	return out
}
