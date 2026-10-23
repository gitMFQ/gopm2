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

	// procScan 是进程扫描函数，默认 scanProcs；测试可注入。
	procScan procScanFunc

	// adoptMinUptime 是"值得等替身"的最短存活时间，默认 defaultAdoptMinUptime；
	// 测试可调小。
	adoptMinUptime time.Duration
}

// defaultAdoptMinUptime 见 watchExit 里的说明。
const defaultAdoptMinUptime = 5 * time.Second

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
		logsDir:        logsDir,
		store:          store,
		apps:           make(map[string]*appEntry),
		subs:           make(map[chan types.Event]struct{}),
		samplers:       make(map[string]*pidSampler),
		procScan:       scanProcs,
		adoptMinUptime: defaultAdoptMinUptime,
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

// ensureEntry 返回 app 对应的 entry，不存在则新建。
//
// 关键：e.app 指针只能在这把锁的保护下替换——supervisor 里每一处读 e.app 的
// 代码（startLocked / watchExit / setState / startHealthCheck …）都持 e.mu。
// 早先这里在 s.mu 下直接写 e.app，正好与上面那些读形成数据竞争（CI 的
// `go test -race` 会抓到）。这里先放掉 s.mu 再取 e.mu，既修正竞争，也避免
// 形成 "s.mu → e.mu" 的嵌套加锁顺序（Delete/Shutdown 都是先 e.mu 后 s.mu）。
func (s *Supervisor) ensureEntry(a *types.App) *appEntry {
	s.mu.Lock()
	e, ok := s.apps[a.Spec.Name]
	if !ok {
		e = &appEntry{app: a}
		s.apps[a.Spec.Name] = e
		s.mu.Unlock()
		return e
	}
	s.mu.Unlock()

	e.mu.Lock()
	e.app = a
	e.mu.Unlock()
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
	e.app.Status.Adopted = false
	e.app.Status.LastError = ""
	s.setState(e, types.StateOnline)

	// 监视退出
	go s.watchExit(e, proc)
	// 启动健康检查
	s.startHealthCheck(e)
	return nil
}

// watchExit 等待进程退出并决策：先看应用是否自己换了个替身（接管），
// 再按重启策略决定是否自己补一个。
func (s *Supervisor) watchExit(e *appEntry, proc *managedProcess) {
	res := <-proc.done

	e.mu.Lock()

	// 若已经被新进程替换（restart 场景），忽略旧退出事件
	if e.proc != proc {
		e.mu.Unlock()
		return
	}
	e.proc = nil
	e.app.Status.PID = 0
	e.app.Status.Adopted = false
	e.app.Status.ExitCode = res.code
	e.app.Status.StoppedAt = time.Now()
	s.stopHealthCheck(e)

	// 主动停止：watchExit 由 stopLocked 处理状态，此处只在非停止路径执行
	select {
	case <-proc.stopCh:
		e.mu.Unlock()
		return
	default:
	}

	// 快照决策所需字段；后续在无锁 goroutine 里用（再读 e.app 会与并发 Register 竞争）
	spec := e.app.Spec
	wasAdopted := proc.adopted
	prevStart := e.app.Status.StartedAt
	uptime := time.Since(e.app.Status.StartedAt)
	desired := e.app.Status.DesiredOn
	restarts := e.app.Status.Restarts

	// 异常退出，按策略重启
	shouldRestart := false
	switch spec.Restart {
	case types.RestartAlways:
		shouldRestart = true
	case types.RestartOnFailure:
		shouldRestart = res.code != 0
	case types.RestartNever:
		shouldRestart = false
	}
	if !desired {
		shouldRestart = false
	}
	// max_restarts 限制
	if shouldRestart && spec.MaxRestarts > 0 && restarts >= spec.MaxRestarts {
		shouldRestart = false
		e.app.Status.LastError = fmt.Sprintf("reached max_restarts=%d", spec.MaxRestarts)
	}

	window := spec.AdoptWindow.Std()
	if window == 0 {
		// 兼容升级前写入的旧 Spec（adopt_window 未归一化，读出来是零值）。
		// 这里补默认值，避免老库里的应用静默失去接管能力。
		window = types.DefaultAdoptWindow
	}
	// 是否值得等一个"替身"：应用自重启几乎都表现为"优雅退出（码 0）"或
	// "被终止信号结束"（dsh 捕获 SIGTERM 后以码 0 退出，所以码 0 是主信号）。
	// 再叠一条"活得够久"的门槛：短命令（echo hi / 秒级批处理循环）退出后不应
	// 白等一个窗口——那种形态几乎不可能带替身，而快速重启对它是刚需。
	plausible := window >= 0 && (wasAdopted || (handoffPlausible(res) && uptime >= s.adoptMinUptime))

	if !shouldRestart {
		if res.code == 0 {
			s.setState(e, types.StateStopped)
		} else {
			s.setState(e, types.StateErrored)
		}
		e.mu.Unlock()

		// 不重启不等于"它真的停了"：自重启的应用可能已经留下替身在跑。
		// 这里只做一次即时扫描（不等待），命中则继续守护它，避免状态谎报。
		if plausible && window > 0 {
			s.adoptReplacement(e, proc, spec, prevStart, 0)
		}
		return
	}

	// 计算退避
	if uptime > uptimeResetThreshold {
		restarts = 0 // 存活时间足够长，重置计数
	}
	backoff := backoffFor(restarts)
	s.setState(e, types.StateRestarting)
	e.mu.Unlock()

	// 释放锁后睡眠再重启，避免阻塞其他操作
	go func() {
		// 接管窗口：先看应用是不是自己把替身拉起来了（dshmarket 自重启 dsh
		// 就是这样）。命中即接管，绝不再 spawn 第二个，否则两个进程抢同一端口。
		if plausible {
			if s.adoptReplacement(e, proc, spec, prevStart, window) {
				return
			}
		}
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		// 等待期间可能被 stop/delete 取消
		e.mu.Lock()
		wanted := e.app.Status.DesiredOn && e.proc == nil
		e.mu.Unlock()
		if !wanted {
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

// handoffPlausible 判断这次退出是否像"应用自己的重启交接"。
//
// 判据不是"被信号杀死"：dsh 这类应用会捕获 SIGTERM，优雅收尾后以退出码 0 退出，
// 从 WaitStatus 里看不到信号。所以码 0 与"被礼貌信号终结"都算可疑形态。
func handoffPlausible(res exitResult) bool {
	if res.code == 0 {
		return true
	}
	return politelyTerminated(res.signal)
}

// adoptReplacement 查找应用自己拉起的替身进程并接管。
//
// 返回 true 表示已经接管（调用方不要再 spawn）；false 表示没找到，调用方可以
// 安全地自己重启。window 的语义：
//   - < 0：接管被禁用，直接返回 false；
//   - == 0：只做一次即时扫描（供"不该重启但可能已被替身接管"的路径用）；
//   - > 0：即时扫描后继续轮询，直到命中或窗口用尽。
//
// 命中即返回，所以真实延迟通常只有替身出现所需的那点时间（dsh 场景约 1s），
// 窗口只是最长等待。
func (s *Supervisor) adoptReplacement(e *appEntry, dead *managedProcess, spec types.Spec, prevStart time.Time, window time.Duration) bool {
	if window < 0 {
		return false
	}
	// 主动 stop/restart 由 stopLocked 负责，stopCh 已关闭，不在此路径
	select {
	case <-dead.stopCh:
		return false
	default:
	}

	// 基准：替身必须比"被替换的那个进程"更新。用它自己的启动时刻而不是退出
	// 时刻，这样"s先起新的再杀老的"与"先杀老的再起新的"两种交接都覆盖。
	since := procStartOf(prevStart, dead)
	deadline := time.Now().Add(window)
	scan := s.procScan
	if scan == nil {
		scan = scanProcs
	}
	for {
		// 每轮重算排除集：窗口期间别的 app 可能新起了进程，不能把它的进程抢来
		if pid, ok := findReplacement(&spec, since, scan, s.excludeSet()); ok {
			return s.adoptPID(e, spec, pid)
		}
		if !sleepUntil(deadline, adoptPollInterval) {
			return false
		}
	}
}

// adoptPID 在持锁状态下把一个替身进程纳入守护，成功返回 true。
func (s *Supervisor) adoptPID(e *appEntry, spec types.Spec, pid int) bool {
	startAt := procStartAt(pid)
	statusStart := startAt
	if statusStart.IsZero() {
		statusStart = time.Now()
	}

	e.mu.Lock()
	// 窗口期间意图可能已变（stop/delete）或已有新进程接位
	if e.proc != nil || !e.app.Status.DesiredOn {
		e.mu.Unlock()
		return true
	}
	proc := adopt(&spec, pid, startAt)
	e.proc = proc
	e.app.Status.PID = pid
	e.app.Status.Adopted = true
	e.app.Status.StartedAt = statusStart
	e.app.Status.ExitCode = 0
	e.app.Status.LastError = ""
	s.setState(e, types.StateOnline)
	s.startHealthCheck(e)
	e.mu.Unlock()

	// 接管进程的 stdout/stderr 不在我们手里，尽力跟随镜像进日志，
	// 让 `gopm2 logs` 在接管之后依然能看到输出。进程退出后停止镜像。
	mirrorStop := s.startAdoptedLogMirror(spec.Name, pid)
	go func() {
		<-proc.waitDone
		mirrorStop()
	}()

	go s.watchExit(e, proc)
	return true
}

// excludeSet 返回当前不得接管的 pid 集合：自身、父进程，以及本 daemon 正在
// 守护的所有进程（含接管来的）。否则两个命令行相同的 app 会互相抢进程。
func (s *Supervisor) excludeSet() excludeSet {
	set := buildExcludeSet()
	s.mu.Lock()
	entries := make([]*appEntry, 0, len(s.apps))
	for _, e := range s.apps {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if e.proc != nil {
			set.add(e.proc.pid())
		}
		e.mu.Unlock()
	}
	return set
}

// procStartOf 返回受管进程的启动时刻，用作"替身必须比它更新"的基准。
//
// 进程可能已经死了，读不到 /proc；因此优先用 watchExit 在退出瞬间快照下来的
// StartedAt（由调用方传入）。两者都不可用时返回零值，表示不做时间过滤。
func procStartOf(recorded time.Time, proc *managedProcess) time.Time {
	if !recorded.IsZero() {
		return recorded
	}
	if proc.adopted {
		// 接管的进程：pid 可能已被回收，读到的未必是原来那个
		return time.Time{}
	}
	if t, err := metrics.ProcessStartTime(proc.pid()); err == nil {
		return t
	}
	return time.Time{}
}

// politelyTerminated 判断退出是否属于"被终止信号结束"（而非崩溃）。
func politelyTerminated(sig syscall.Signal) bool {
	switch sig {
	case syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT:
		return true
	default:
		return false
	}
}

// procStartAt 读取进程真实启动时间；读不到时返回零值（调用方对零值不做身份校验）。
func procStartAt(pid int) time.Time {
	if t, err := metrics.ProcessStartTime(pid); err == nil {
		return t
	}
	return time.Time{}
}

// sleepUntil 睡一个步长；超过 deadline 时返回 false 表示窗口用尽。
func sleepUntil(deadline time.Time, step time.Duration) bool {
	remain := time.Until(deadline)
	if remain <= 0 {
		return false
	}
	if remain < step {
		step = remain
	}
	time.Sleep(step)
	return true
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
	proc.markStop() // 标记主动停止（幂等，避免重复 close panic）
	proc.stop(e.app.Spec.StopTimeout.Std())
	e.proc = nil
	e.app.Status.PID = 0
	e.app.Status.Adopted = false
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
