package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitMFQ/gopm2/internal/logutil"
	"github.com/gitMFQ/gopm2/internal/metrics"
	"github.com/gitMFQ/gopm2/internal/types"
)

// adoptPollInterval 是接管进程的存活轮询间隔。
// 非子进程拿不到 SIGCHLD，也没有 Wait 句柄，只能轮询 /proc。
const adoptPollInterval = 500 * time.Millisecond

// managedProcess 封装一个被监督的进程。
//
// 有两种形态：
//   - 自己 fork 的（cmd != nil）：有 exec.Cmd 与 Wait 句柄，退出码/信号精确可得；
//   - 接管的（adopted == true）：进程不是我们 fork 的（应用"自重启"留下的替身），
//     没有 Wait 句柄，只能轮询 /proc 判断存活，退出原因不可知。
//
// 两者对外接口一致：done 送一次退出结果，waitDone 关闭表示进程已不复存在。
type managedProcess struct {
	spec *types.Spec

	cmd    *exec.Cmd // 接管的进程为 nil
	pidVal int

	adopted  bool
	outLog   *logutil.RotatingWriter // 接管的进程为 nil：日志 fd 不在我们手里
	errLog   *logutil.RotatingWriter
	stopOnce sync.Once

	done     chan exitResult // 退出后写一次（由 watchExit 消费）
	waitDone chan struct{}   // 进程不复存在即关闭（供 stop 等待，不与 done 竞争）
	stopCh   chan struct{}   // 标记主动停止，避免退出事件触发重启
}

type exitResult struct {
	code   int
	signal syscall.Signal // 被信号杀死时的信号；正常退出为 0
	err    error
}

// buildCmd 根据 Spec 构造 exec.Cmd：独立进程组 + 日志重定向。
func buildCmd(spec *types.Spec, outW, errW *logutil.RotatingWriter) *exec.Cmd {
	var cmd *exec.Cmd
	if len(spec.Args) > 0 {
		cmd = exec.Command(spec.Command, spec.Args...)
	} else {
		cmd = exec.Command("sh", "-c", spec.Command)
	}
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	if len(spec.Env) > 0 {
		env := os.Environ()
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	// Setpgid：子进程独立进程组，便于整组 kill；Setsid 留给 watchdog。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// spawn 启动进程，返回 managedProcess。失败时返回 error。
func spawn(spec *types.Spec, logsDir string) (*managedProcess, error) {
	outPath, errPath, err := logutil.SafeLogPath(logsDir, spec.Name)
	if err != nil {
		return nil, err
	}
	outW, err := logutil.NewRotatingWriter(outPath, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("open stdout log: %w", err)
	}
	errW, err := logutil.NewRotatingWriter(errPath, 0, 0)
	if err != nil {
		outW.Close()
		return nil, fmt.Errorf("open stderr log: %w", err)
	}
	mp := &managedProcess{
		spec:     spec,
		outLog:   outW,
		errLog:   errW,
		done:     make(chan exitResult, 1),
		waitDone: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	mp.cmd = buildCmd(spec, outW, errW)
	if err := mp.cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		return nil, fmt.Errorf("start %q: %w", spec.Name, err)
	}
	mp.pidVal = mp.cmd.Process.Pid
	go func() {
		err := mp.cmd.Wait()
		code := 0
		var sig syscall.Signal
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		if ps := mp.cmd.ProcessState; ps != nil {
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig = ws.Signal()
			}
		}
		// Wait 返回后 stdout/stderr 拷贝 goroutine 已结束，可安全关闭日志 fd（防泄漏）
		mp.outLog.Close()
		mp.errLog.Close()
		mp.done <- exitResult{code: code, signal: sig, err: err}
		close(mp.waitDone)
	}()
	return mp, nil
}

// adopt 把一个已经存在、但不是我们 fork 的进程纳入守护。
// 没有 Wait 句柄，因此用轮询 /proc 的方式感知退出；退出码不可知，记为 -1。
// startedAt 是它的真实启动时刻，轮询时用它防 pid 复用（pid 被回收后另作他用时
// 启动时间会对不上，据此判定它已不是原来那个进程）。
func adopt(spec *types.Spec, pid int, startedAt time.Time) *managedProcess {
	mp := &managedProcess{
		spec:     spec,
		pidVal:   pid,
		adopted:  true,
		done:     make(chan exitResult, 1),
		waitDone: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	go func() {
		ticker := time.NewTicker(adoptPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			if !metrics.PidAlive(pid) {
				break
			}
			// startedAt 未知（读不到 /proc）时不做身份校验：此时 PidAlive 就是唯一依据，
			// 拿零值去比会把一个活着的进程误判成已退出。
			if !startedAt.IsZero() && !sameProcess(pid, startedAt) {
				break
			}
		}
		mp.done <- exitResult{code: -1}
		close(mp.waitDone)
	}()
	return mp
}

// pid 返回进程 pid（未启动返回 0）。
func (mp *managedProcess) pid() int {
	if mp.pidVal > 0 {
		return mp.pidVal
	}
	if mp.cmd == nil || mp.cmd.Process == nil {
		return 0
	}
	return mp.cmd.Process.Pid
}

// markStop 幂等地标记"主动停止"，避免重复 close 造成 panic。
func (mp *managedProcess) markStop() {
	mp.stopOnce.Do(func() { close(mp.stopCh) })
}

// stop 先向整个进程组发 SIGTERM，超时后 SIGKILL。等待进程不复存在。
//
// 对接管的进程这一点尤其重要：它是 setsid 出来的独立会话首领（pgid == pid），
// 只有按进程组发信号才能连带清掉它派生的子进程，否则端口会被野进程继续占着。
func (mp *managedProcess) stop(timeout time.Duration) {
	pid := mp.pid()
	if pid <= 0 {
		return
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		// 进程可能已退出；接管的进程靠轮询确认，等它自己收尾
		if mp.adopted {
			<-mp.waitDone
		}
		return
	}
	// 安全兜底：绝不向"我们自己所在的进程组"发整组信号（-1 会打掉 daemon 自己）。
	// 受管进程都通过 Setpgid 有独立进程组，正常不会走到这里，纯属防御。
	if pgid == syscall.Getpgrp() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	} else {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-mp.waitDone:
		return
	case <-timer.C:
		if pgid == syscall.Getpgrp() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		} else {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		<-mp.waitDone // 等回收，避免僵尸/残留
	}
}

// backoffFor 按重启次数计算指数退避：1s,2s,4s... 最大 30s。
func backoffFor(restarts int) time.Duration {
	if restarts < 0 {
		restarts = 0
	}
	d := time.Second << uint(restarts)
	if d > 30*time.Second || d <= 0 {
		d = 30 * time.Second
	}
	return d
}

// envListForDisplay 将环境变量脱敏后输出（值含 KEY/SECRET/TOKEN/PASSWORD 的键打码）。
func maskEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		up := strings.ToUpper(k)
		if strings.Contains(up, "KEY") || strings.Contains(up, "SECRET") ||
			strings.Contains(up, "TOKEN") || strings.Contains(up, "PASSWORD") {
			out[k] = "******"
		} else {
			out[k] = v
		}
	}
	return out
}
