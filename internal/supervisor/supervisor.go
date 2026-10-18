// Package supervisor 负责进程生命周期管理：状态机、自动重启退避、健康检查、指标采样。
package supervisor

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/gitMFQ/gopm2/internal/health"
	"github.com/gitMFQ/gopm2/internal/metrics"
	"github.com/gitMFQ/gopm2/internal/types"
)

// Store 接口抽象持久化，便于测试注入。
type Store interface {
	PutApp(a *types.App) error
	GetApp(name string) (*types.App, error)
	ListApps() ([]*types.App, error)
	DeleteApp(name string) error
}

// uptimeResetThreshold：进程存活超过该时长则重置退避计数。
const uptimeResetThreshold = 60 * time.Second

// Supervisor 管理所有受监督进程。
type Supervisor struct {
	logsDir string
	store   Store

	mu   sync.Mutex
	apps map[string]*appEntry

	// 事件订阅
	subMu sync.Mutex
	subs  map[chan types.Event]struct{}

	// 指标采样器：按 app 名保存，pid 变化时重建（避免按 pid 泄漏）
	samplersMu sync.Mutex
	samplers   map[string]*pidSampler
}

// pidSampler 绑定 pid 与采样器，pid 变化（重启）时重建以重置 CPU 基准。
type pidSampler struct {
	pid    int
	reader *metrics.ProcReader
}

type appEntry struct {
	mu     sync.Mutex
	app    *types.App
	proc   *managedProcess
	cancel context.CancelFunc // 健康检查 goroutine 取消
}

// New 创建 Supervisor。
func New(logsDir string, store Store) *Supervisor {
	return &Supervisor{
		logsDir:  logsDir,
		store:    store,
		apps:     make(map[string]*appEntry),
		subs:     make(map[chan types.Event]struct{}),
		samplers: make(map[string]*pidSampler),
	}
}

// Subscribe 返回事件通道；调用方用完后必须 Unsubscribe。
func (s *Supervisor) Subscribe(bufSize int) chan types.Event {
	if bufSize <= 0 {
		bufSize = 64
	}
	ch := make(chan types.Event, bufSize)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

// Unsubscribe 移除订阅。
func (s *Supervisor) Unsubscribe(ch chan types.Event) {
	s.subMu.Lock()
	delete(s.subs, ch)
	s.subMu.Unlock()
}

func (s *Supervisor) publish(ev types.Event) {
	ev.At = time.Now()
	s.subMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // 订阅者太慢，丢弃
		}
	}
	s.subMu.Unlock()
}

func (s *Supervisor) ensureEntry(a *types.App) *appEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.apps[a.Spec.Name]; ok {
		e.app = a
		return e
	}
	e := &appEntry{app: a}
	s.apps[a.Spec.Name] = e
	return e
}

// setState 更新状态并广播。
func (s *Supervisor) setState(e *appEntry, st types.State) {
	e.app.Status.State = st
	_ = s.store.PutApp(e.app)
	s.publish(types.Event{Type: types.EventStateChange, App: e.app.Spec.Name, Data: string(st)})
}

// Register 将 App 注册进 supervisor（不启动）。重复注册会合并 Spec。
func (s *Supervisor) Register(spec types.Spec) (*types.App, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	existing, err := s.store.GetApp(spec.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing = &types.App{Spec: spec, Status: types.Status{State: types.StateStopped}}
		if err := s.store.PutApp(existing); err != nil {
			return nil, err
		}
	} else {
		existing.Spec = spec
		if existing.Status.State == "" {
			existing.Status.State = types.StateStopped
		}
		if err := s.store.PutApp(existing); err != nil {
			return nil, err
		}
	}
	s.ensureEntry(existing)
	return existing, nil
}

// Start 启动指定进程；已在运行则幂等返回。
func (s *Supervisor) Start(name string) error {
	a, err := s.store.GetApp(name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("app %q not found", name)
	}
	e := s.ensureEntry(a)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.proc != nil && e.app.Status.State == types.StateOnline {
		return nil // 幂等
	}
	return s.startLocked(e)
}

// startLocked 在持有 e.mu 时执行启动。
func (s *Supervisor) startLocked(e *appEntry) error {
	s.setState(e, types.StateStarting)
	proc, err := spawn(&e.app.Spec, s.logsDir)
	if err != nil {
		e.app.Status.LastError = err.Error()
		s.setState(e, types.StateErrored)
		return err
	}
	e.proc = proc
	e.app.Status.PID = proc.pid()
	e.app.Status.StartedAt = time.Now()
	e.app.Status.ExitCode = 0
	e.app.Status.DesiredOn = true
	e.app.Status.LastError = ""
	s.setState(e, types.StateOnline)

	// 监视退出
	go s.watchExit(e, proc)
	// 启动健康检查
	s.startHealthCheck(e)
	return nil
}

// watchExit 等待进程退出并决策是否重启。
func (s *Supervisor) watchExit(e *appEntry, proc *managedProcess) {
	res := <-proc.done

	e.mu.Lock()
	defer e.mu.Unlock()

	// 若已经被新进程替换（restart 场景），忽略旧退出事件
	if e.proc != proc {
		return
	}
	e.proc = nil
	e.app.Status.PID = 0
	e.app.Status.ExitCode = res.code
	e.app.Status.StoppedAt = time.Now()
	s.stopHealthCheck(e)

	// 主动停止：watchExit 由 stopLocked 处理状态，此处只在非停止路径执行
	select {
	case <-proc.stopCh:
		// stopLocked 已处理
		return
	default:
	}

	// 异常退出，按策略重启
	spec := &e.app.Spec
	shouldRestart := false
	switch spec.Restart {
	case types.RestartAlways:
		shouldRestart = true
	case types.RestartOnFailure:
		shouldRestart = res.code != 0
	case types.RestartNever:
		shouldRestart = false
	}
	if !e.app.Status.DesiredOn {
		shouldRestart = false
	}
	// max_restarts 限制
	if shouldRestart && spec.MaxRestarts > 0 && e.app.Status.Restarts >= spec.MaxRestarts {
		shouldRestart = false
		e.app.Status.LastError = fmt.Sprintf("reached max_restarts=%d", spec.MaxRestarts)
	}

	if !shouldRestart {
		if res.code == 0 {
			s.setState(e, types.StateStopped)
		} else {
			s.setState(e, types.StateErrored)
		}
		return
	}

	// 计算退避
	restarts := e.app.Status.Restarts
	// 存活时间足够长，重置计数
	if time.Since(e.app.Status.StartedAt) > uptimeResetThreshold {
		restarts = 0
	}
	backoff := backoffFor(restarts)
	s.setState(e, types.StateRestarting)

	// 释放锁后睡眠再重启，避免阻塞其他操作
	go func() {
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		// 等待期间可能被 stop/delete 取消
		e.mu.Lock()
		desired := e.app.Status.DesiredOn && e.proc == nil
		e.mu.Unlock()
		if !desired {
			return
		}
		<-timer.C
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.app.Status.DesiredOn || e.proc != nil {
			return
		}
		e.app.Status.Restarts++
		if err := s.startLocked(e); err != nil {
			// startLocked 内部已置 errored
			return
		}
	}()
}

// Stop 停止进程，等待退出。
func (s *Supervisor) Stop(name string) error {
	a, err := s.store.GetApp(name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("app %q not found", name)
	}
	e := s.ensureEntry(a)
	e.mu.Lock()
	defer e.mu.Unlock()
	return s.stopLocked(e)
}

func (s *Supervisor) stopLocked(e *appEntry) error {
	e.app.Status.DesiredOn = false
	s.stopHealthCheck(e)
	if e.proc == nil {
		s.setState(e, types.StateStopped)
		return nil
	}
	s.setState(e, types.StateStopping)
	proc := e.proc
	close(proc.stopCh) // 标记主动停止
	proc.stop(e.app.Spec.StopTimeout.Std())
	e.proc = nil
	e.app.Status.PID = 0
	e.app.Status.StoppedAt = time.Now()
	s.setState(e, types.StateStopped)
	return nil
}

// Restart 重启：先停再启。
func (s *Supervisor) Restart(name string) error {
	a, err := s.store.GetApp(name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("app %q not found", name)
	}
	e := s.ensureEntry(a)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := s.stopLocked(e); err != nil {
		return err
	}
	// 显式重启（用户手动 / 健康检查触发）计入重启次数
	e.app.Status.Restarts++
	return s.startLocked(e)
}

// Delete 停止并从 store 移除。
func (s *Supervisor) Delete(name string) error {
	a, err := s.store.GetApp(name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("app %q not found", name)
	}
	e := s.ensureEntry(a)
	e.mu.Lock()
	if err := s.stopLocked(e); err != nil {
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	s.mu.Lock()
	delete(s.apps, name)
	s.mu.Unlock()
	// 清理该 app 的指标采样器，避免长期运行泄漏
	s.samplersMu.Lock()
	delete(s.samplers, name)
	s.samplersMu.Unlock()
	return s.store.DeleteApp(name)
}

// List 返回全部 App。
func (s *Supervisor) List() ([]*types.App, error) {
	return s.store.ListApps()
}

// Get 返回单个 App。
func (s *Supervisor) Get(name string) (*types.App, error) {
	return s.store.GetApp(name)
}

// SampleMetrics 采样一次指标并更新 store 中的缓存字段。
// 正常情况下由 MetricsLoop 周期调用；API 按需调用亦可。
func (s *Supervisor) SampleMetrics(name string) (*metrics.Sample, error) {
	a, err := s.store.GetApp(name)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("app %q not found", name)
	}
	if a.Status.State != types.StateOnline || a.Status.PID <= 0 {
		return &metrics.Sample{}, nil
	}
	pid := a.Status.PID
	s.samplersMu.Lock()
	ps, ok := s.samplers[name]
	if !ok || ps.pid != pid {
		// 首次或进程重启（pid 变化）：重建采样器，重置 CPU 基准
		ps = &pidSampler{pid: pid, reader: metrics.NewProcReader()}
		s.samplers[name] = ps
	}
	s.samplersMu.Unlock()
	smp, err := ps.reader.Sample(pid, a.Status.StartedAt)
	if err != nil {
		return nil, err
	}
	a.Status.CPUPercent = smp.CPUPercent
	a.Status.RSSBytes = smp.RSSBytes
	_ = s.store.PutApp(a)
	return smp, nil
}

// MetricsLoop 后台周期采样所有 online 应用的指标。
// 这样 List/Get API 只读 store 缓存，避免每次请求都触发"读 /proc + bbolt 写事务"。
func (s *Supervisor) MetricsLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			apps, err := s.store.ListApps()
			if err != nil {
				continue
			}
			for _, a := range apps {
				if a.Status.State == types.StateOnline && a.Status.PID > 0 {
					_, _ = s.SampleMetrics(a.Spec.Name)
				}
			}
		}
	}()
}

// Reconcile daemon 启动时调用：清理残留子进程。
// daemon 重启后无法重建旧进程的 Wait 句柄与日志 writer，
// 因此对 store 中 state=online/restarting/stopping 的 app：
// 若旧 pid 仍存活且确为原进程（比对启动时间防 pid 复用），按进程组 SIGKILL 清理；
// 无论是否存活都标记为 stopped（由调用方决定是否 resurrect）。
func (s *Supervisor) Reconcile() error {
	apps, err := s.store.ListApps()
	if err != nil {
		return err
	}
	for _, a := range apps {
		st := &a.Status
		if st.State != types.StateOnline && st.State != types.StateRestarting && st.State != types.StateStopping {
			if st.State == "" {
				st.State = types.StateStopped
				_ = s.store.PutApp(a)
			}
			continue
		}
		if st.PID > 0 && metrics.PidAlive(st.PID) && sameProcess(st.PID, st.StartedAt) {
			// 确认是我们之前启动的那个进程（非 pid 复用），杀整个进程组清理残留
			if pgid, err := syscall.Getpgid(st.PID); err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		st.PID = 0
		st.State = types.StateStopped
		st.StoppedAt = time.Now()
		_ = s.store.PutApp(a)
	}
	return nil
}

// sameProcess 判断 pid 对应的进程是否就是在 startedAt 附近启动的那个（防 pid 复用误杀）。
// /proc 的启动时间精度是 jiffies（10ms），与记录的 StartedAt 存在微小偏差，这里放宽到 2 秒容差。
func sameProcess(pid int, startedAt time.Time) bool {
	if startedAt.IsZero() {
		// 没有记录启动时间则无法校验，保守起见不动手（宁可漏杀也不误杀）
		return false
	}
	realStart, err := metrics.ProcessStartTime(pid)
	if err != nil {
		return false
	}
	diff := realStart.Sub(startedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff < 2*time.Second
}

// Resurrect 恢复 snapshot 中记录的进程（daemon 启动时调用）。
func (s *Supervisor) Resurrect(specs []*types.Spec) (started []string, errs []error) {
	for _, spec := range specs {
		if _, err := s.Register(*spec); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.Start(spec.Name); err != nil {
			errs = append(errs, fmt.Errorf("start %q: %w", spec.Name, err))
			continue
		}
		started = append(started, spec.Name)
	}
	return started, errs
}

// startHealthCheck 为 entry 启动健康检查 goroutine。
func (s *Supervisor) startHealthCheck(e *appEntry) {
	hs := e.app.Spec.Health
	if hs.Type == types.HealthNone {
		return
	}
	checker := health.New(hs)
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	name := e.app.Spec.Name
	go func() {
		ticker := time.NewTicker(hs.Interval.Std())
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// 仅 online 状态检查
			e.mu.Lock()
			online := e.app.Status.State == types.StateOnline
			e.mu.Unlock()
			if !online {
				continue
			}
			cctx, ccancel := context.WithTimeout(ctx, hs.Timeout.Std())
			err := checker.Check(cctx)
			ccancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures >= hs.Failures {
				s.publish(types.Event{Type: types.EventStateChange, App: name, Data: "health-failed"})
				_ = s.Restart(name)
				return
			}
		}
	}()
}

func (s *Supervisor) stopHealthCheck(e *appEntry) {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
}

// Shutdown 停止全部进程（daemon 退出时调用）。
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	entries := make([]*appEntry, 0, len(s.apps))
	for _, e := range s.apps {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *appEntry) {
			defer wg.Done()
			e.mu.Lock()
			defer e.mu.Unlock()
			_ = s.stopLocked(e)
		}(e)
	}
	wg.Wait()
}

// MaskEnv 导出脱敏工具供 API 使用。
func MaskEnv(env map[string]string) map[string]string { return maskEnv(env) }
