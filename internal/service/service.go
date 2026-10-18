// Package service 是 CLI 与 Web 共用的唯一业务层，组合 supervisor 与 store。
package service

import (
	"encoding/json"
	"fmt"

	"github.com/gitMFQ/gopm2/internal/logutil"
	"github.com/gitMFQ/gopm2/internal/metrics"
	"github.com/gitMFQ/gopm2/internal/supervisor"
	"github.com/gitMFQ/gopm2/internal/types"
)

// Store 是 service 需要的持久化能力（store.Store 的超集接口便于测试）。
type Store interface {
	supervisor.Store
	SaveSnapshot(apps []*types.App) error
	LoadSnapshot() ([]*types.Spec, error)
}

// Service 业务入口。
type Service struct {
	sup   *supervisor.Supervisor
	store Store
}

// New 创建 Service。
func New(sup *supervisor.Supervisor, store Store) *Service {
	return &Service{sup: sup, store: store}
}

// AddApp 注册（创建/更新）一个应用。
func (s *Service) AddApp(spec types.Spec) (*types.App, error) {
	return s.sup.Register(spec)
}

// Start 启动应用（幂等）。
func (s *Service) Start(name string) error { return s.sup.Start(name) }

// Stop 停止应用。
func (s *Service) Stop(name string) error { return s.sup.Stop(name) }

// Restart 重启应用。
func (s *Service) Restart(name string) error { return s.sup.Restart(name) }

// Delete 删除应用。
func (s *Service) Delete(name string) error { return s.sup.Delete(name) }

// List 列出应用。指标由后台 MetricsLoop 周期采样写入 store，这里直接返回缓存值，
// 避免每次请求都触发"读 /proc + bbolt 写事务"。
func (s *Service) List() ([]*types.App, error) {
	return s.sup.List()
}

// Get 获取单个应用。
func (s *Service) Get(name string) (*types.App, error) {
	return s.sup.Get(name)
}

// Metrics 采样指定应用指标。
func (s *Service) Metrics(name string) (*metrics.Sample, error) {
	return s.sup.SampleMetrics(name)
}

// LogsTail 读取应用日志尾部（stream: out|err）。
func (s *Service) LogsTail(logsDir, name, stream string, maxBytes int64) ([]byte, error) {
	outPath, errPath, err := logutil.SafeLogPath(logsDir, name)
	if err != nil {
		return nil, err
	}
	p := outPath
	if stream == "err" {
		p = errPath
	}
	return logutil.ReadTail(p, maxBytes)
}

// Save 将当前应用列表固化为 snapshot。
func (s *Service) Save() (int, error) {
	apps, err := s.sup.List()
	if err != nil {
		return 0, err
	}
	if err := s.store.SaveSnapshot(apps); err != nil {
		return 0, err
	}
	n := 0
	for _, a := range apps {
		if a.Status.DesiredOn {
			n++
		}
	}
	return n, nil
}

// Resurrect 从 snapshot 恢复应用并启动。
func (s *Service) Resurrect() (started []string, errs []error, err error) {
	specs, err := s.store.LoadSnapshot()
	if err != nil {
		return nil, nil, err
	}
	started, errs = s.sup.Resurrect(specs)
	return started, errs, nil
}

// Apply 幂等应用一组 Spec：新增/更新；removeMissing=true 时删除配置中不存在的应用。
// spec 未变化的已有应用不会被重启（幂等）；spec 变化或期望运行但已停止的才会被操作。
func (s *Service) Apply(specs []*types.Spec, removeMissing bool) (changed []string, err error) {
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		// 先规范化输入，保证与 store 中已规范化的 spec 可比较
		if err := spec.Normalize(); err != nil {
			return nil, err
		}
		seen[spec.Name] = true
		existing, err := s.sup.Get(spec.Name)
		if err != nil {
			return nil, err
		}
		app, err := s.sup.Register(*spec)
		if err != nil {
			return nil, fmt.Errorf("apply %q: %w", spec.Name, err)
		}
		// 新应用：直接启动
		if existing == nil {
			changed = append(changed, spec.Name)
			if err := s.sup.Start(spec.Name); err != nil {
				return changed, fmt.Errorf("start %q: %w", spec.Name, err)
			}
			continue
		}
		// 已存在：spec 未变化则完全不动（幂等）；变化了才重启
		if specEqual(&existing.Spec, spec) {
			// 状态收敛：期望运行但没在跑（如 errored/stopped）则拉起
			if app.Status.DesiredOn && app.Status.State != types.StateOnline &&
				app.Status.State != types.StateStarting && app.Status.State != types.StateRestarting {
				changed = append(changed, spec.Name)
				if err := s.sup.Start(spec.Name); err != nil {
					return changed, fmt.Errorf("start %q: %w", spec.Name, err)
				}
			}
			continue
		}
		changed = append(changed, spec.Name)
		if app.Status.State == types.StateOnline || app.Status.DesiredOn {
			if err := s.sup.Restart(spec.Name); err != nil {
				return changed, fmt.Errorf("restart %q: %w", spec.Name, err)
			}
		}
	}
	if removeMissing {
		apps, err := s.sup.List()
		if err != nil {
			return changed, err
		}
		for _, a := range apps {
			if !seen[a.Spec.Name] {
				if err := s.sup.Delete(a.Spec.Name); err != nil {
					return changed, fmt.Errorf("delete %q: %w", a.Spec.Name, err)
				}
				changed = append(changed, "-"+a.Spec.Name)
			}
		}
	}
	return changed, nil
}

// Export 导出全部应用 Spec 为可备份结构。
func (s *Service) Export() ([]*types.Spec, error) {
	apps, err := s.sup.List()
	if err != nil {
		return nil, err
	}
	out := make([]*types.Spec, 0, len(apps))
	for _, a := range apps {
		spec := a.Spec
		out = append(out, &spec)
	}
	return out, nil
}

// Import 导入 Spec 列表（等价于 Apply 不删除缺失项）。
func (s *Service) Import(specs []*types.Spec) ([]string, error) {
	return s.Apply(specs, false)
}

// Subscribe 订阅事件流。
func (s *Service) Subscribe() chan types.Event { return s.sup.Subscribe(64) }

// Unsubscribe 取消订阅。
func (s *Service) Unsubscribe(ch chan types.Event) { s.sup.Unsubscribe(ch) }

// specEqual 判断两个已规范化的 Spec 是否语义相同。
// 通过 JSON 序列化比较（encoding/json 对 map 键排序输出，结果稳定）。
func specEqual(a, b *types.Spec) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ja) == string(jb)
}
