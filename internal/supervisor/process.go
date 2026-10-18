package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/gitMFQ/gopm2/internal/logutil"
	"github.com/gitMFQ/gopm2/internal/types"
)

// managedProcess 封装一个被监督的子进程。
type managedProcess struct {
	spec   *types.Spec
	cmd    *exec.Cmd
	outLog *logutil.RotatingWriter
	errLog *logutil.RotatingWriter

	done     chan exitResult // Wait 结束后写一次（由 watchExit 消费）
	waitDone chan struct{}   // Wait 返回即关闭（供 stop 等待，不与 done 竞争）
	stopCh   chan struct{}   // 标记主动停止，避免退出事件触发重启
}

type exitResult struct {
	code int
	err  error
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
	go func() {
		err := mp.cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		// Wait 返回后 stdout/stderr 拷贝 goroutine 已结束，可安全关闭日志 fd（防泄漏）
		mp.outLog.Close()
		mp.errLog.Close()
		mp.done <- exitResult{code: code, err: err}
		close(mp.waitDone)
	}()
	return mp, nil
}

// pid 返回进程 pid（未启动返回 0）。
func (mp *managedProcess) pid() int {
	if mp.cmd == nil || mp.cmd.Process == nil {
		return 0
	}
	return mp.cmd.Process.Pid
}

// stop 先向整个进程组发 SIGTERM，超时后 SIGKILL。等待 Wait 完成。
func (mp *managedProcess) stop(timeout time.Duration) {
	if mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(mp.pid())
	if err != nil {
		// 进程可能已退出
		return
	}
	// 负 pid：向整个进程组发信号
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-mp.waitDone:
		return
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-mp.waitDone // 等 Wait 回收，避免僵尸
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
