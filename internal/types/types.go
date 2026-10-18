// Package types 定义 gopm2 的核心数据结构：进程规格、运行时状态、状态机与事件。
package types

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// State 是进程状态机的状态。
type State string

const (
	StateStopped    State = "stopped"
	StateStarting   State = "starting"
	StateOnline     State = "online"
	StateStopping   State = "stopping"
	StateErrored    State = "errored"
	StateRestarting State = "restarting"
)

// RestartPolicy 重启策略。
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

// ParseRestartPolicy 校验并解析重启策略字符串。
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	switch RestartPolicy(s) {
	case RestartAlways, RestartOnFailure, RestartNever:
		return RestartPolicy(s), nil
	default:
		return "", fmt.Errorf("invalid restart policy %q (want always|on-failure|never)", s)
	}
}

// Duration 是 time.Duration 的别名类型，JSON 序列化为秒（浮点）而非纳秒，
// 便于 API 消费与人工阅读；反序列化同时接受数字（秒）与字符串（如 "10s"）。
type Duration time.Duration

// Std 转换为标准 time.Duration。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Seconds 返回秒数。
func (d Duration) Seconds() float64 { return time.Duration(d).Seconds() }

// MarshalJSON 以秒为单位输出。
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(time.Duration(d).Seconds(), 'f', -1, 64)), nil
}

// UnmarshalJSON 接受数字（秒）或 duration 字符串（如 "10s"）。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		*d = Duration(time.Duration(value * float64(time.Second)))
		return nil
	case string:
		td, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(td)
		return nil
	default:
		return fmt.Errorf("invalid duration value: %v", v)
	}
}

// HealthType 健康检查类型。
type HealthType string

const (
	HealthNone HealthType = ""
	HealthHTTP HealthType = "http"
	HealthTCP  HealthType = "tcp"
	HealthExec HealthType = "exec"
)

// HealthSpec 描述一次健康检查的配置。
type HealthSpec struct {
	Type     HealthType `json:"type" toml:"-"`     // http | tcp | exec
	Target   string     `json:"target" toml:"-"`   // URL / host:port / 命令
	Interval Duration   `json:"interval" toml:"-"` // 检查间隔，默认 10s
	Timeout  Duration   `json:"timeout" toml:"-"`  // 单次超时，默认 5s
	Failures int        `json:"failures" toml:"-"` // 连续失败多少次触发重启，默认 3
}

// Spec 是用户对进程的声明式描述（持久化）。
type Spec struct {
	Name        string            `json:"name" toml:"name"`
	Command     string            `json:"command" toml:"command"`     // 完整命令字符串（sh -c）或 exec argv[0]
	Args        []string          `json:"args,omitempty" toml:"args"` // 非空时与 Command 一起直接 exec（不经 shell）
	Cwd         string            `json:"cwd,omitempty" toml:"cwd"`
	Env         map[string]string `json:"env,omitempty" toml:"env"`
	Restart     RestartPolicy     `json:"restart" toml:"restart_policy"`
	MaxRestarts int               `json:"max_restarts" toml:"max_restarts"` // 0 = 无限
	StopTimeout Duration          `json:"stop_timeout" toml:"-"`            // SIGTERM 后等待多久再 SIGKILL，默认 10s
	Health      HealthSpec        `json:"health,omitempty" toml:"-"`
}

// Normalize 填充默认值并做基本校验。
func (s *Spec) Normalize() error {
	if s.Name == "" {
		return fmt.Errorf("app name is required")
	}
	if s.Command == "" {
		return fmt.Errorf("app %q: command is required", s.Name)
	}
	if s.Restart == "" {
		s.Restart = RestartOnFailure
	}
	if _, err := ParseRestartPolicy(string(s.Restart)); err != nil {
		return err
	}
	if s.StopTimeout <= 0 {
		s.StopTimeout = Duration(10 * time.Second)
	}
	if s.Health.Type != HealthNone {
		if s.Health.Target == "" {
			return fmt.Errorf("app %q: health target is required", s.Name)
		}
		if s.Health.Interval <= 0 {
			s.Health.Interval = Duration(10 * time.Second)
		}
		if s.Health.Timeout <= 0 {
			s.Health.Timeout = Duration(5 * time.Second)
		}
		if s.Health.Failures <= 0 {
			s.Health.Failures = 3
		}
	}
	return nil
}

// Status 是进程的运行时状态（持久化关键字段，便于 daemon 重启后展示）。
type Status struct {
	State      State     `json:"state"`
	PID        int       `json:"pid"`
	Restarts   int       `json:"restarts"`   // 累计重启次数（不含首次启动）
	ExitCode   int       `json:"exit_code"`  // 最近一次退出码；运行中为 0
	StartedAt  time.Time `json:"started_at"` // 最近一次启动时间
	StoppedAt  time.Time `json:"stopped_at"` // 最近一次停止时间
	DesiredOn  bool      `json:"desired_on"` // 期望运行（resurrect 依据）
	LastError  string    `json:"last_error,omitempty"`
	CPUPercent float64   `json:"cpu_percent"` // 最近一次采样
	RSSBytes   int64     `json:"rss_bytes"`   // 最近一次采样
}

// App 是 Spec + Status 的聚合，API 返回值。
type App struct {
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
}

// EventType 事件类型，用于 SSE 广播。
type EventType string

// EventStateChange 是状态变化事件（含 health-failed 通知）。
const EventStateChange EventType = "state"

// Event 是 supervisor 广播给订阅者的事件。
type Event struct {
	Type EventType `json:"type"`
	App  string    `json:"app"`
	Data string    `json:"data,omitempty"`
	At   time.Time `json:"at"`
}
