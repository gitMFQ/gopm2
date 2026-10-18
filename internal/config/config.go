// Package config 加载 TOM 配置文件并转换为 types.Spec。
package config

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gitMFQ/gopm2/internal/types"
)

// File 对应 gopm2.toml 的顶层结构。
type File struct {
	Version  int        `toml:"version"`
	Defaults Defaults   `toml:"defaults"`
	Apps     []AppEntry `toml:"apps"`
}

type Defaults struct {
	RestartPolicy string `toml:"restart_policy"`
	MaxRestarts   int    `toml:"max_restarts"`
}

// AppEntry 对应一个 [[apps]] 节。
type AppEntry struct {
	Name           string            `toml:"name"`
	Command        string            `toml:"command"`
	Args           []string          `toml:"args"`
	Cwd            string            `toml:"cwd"`
	Env            map[string]string `toml:"env"`
	RestartPolicy  string            `toml:"restart_policy"`
	MaxRestarts    *int              `toml:"max_restarts"`
	HealthCmd      string            `toml:"health_cmd"`      // exec 检查
	HealthHTTP     string            `toml:"health_http"`     // http 检查
	HealthTCP      string            `toml:"health_tcp"`      // tcp 检查
	HealthInterval string            `toml:"health_interval"` // 例如 "10s"
}

// LoadFile 解析 TOML 配置文件为 Spec 列表（应用 defaults）。
func LoadFile(path string) ([]*types.Spec, error) {
	var f File
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(f.Apps) == 0 {
		return nil, fmt.Errorf("%s: no [[apps]] defined", path)
	}
	baseDir := filepath.Dir(path)
	var specs []*types.Spec
	for _, a := range f.Apps {
		spec := &types.Spec{
			Name:        a.Name,
			Command:     a.Command,
			Args:        a.Args,
			Cwd:         a.Cwd,
			Env:         a.Env,
			MaxRestarts: f.Defaults.MaxRestarts,
		}
		// 相对 cwd 相对配置文件所在目录解析
		if spec.Cwd != "" && !filepath.IsAbs(spec.Cwd) {
			spec.Cwd = filepath.Join(baseDir, spec.Cwd)
		}
		policy := a.RestartPolicy
		if policy == "" {
			policy = f.Defaults.RestartPolicy
		}
		if policy != "" {
			spec.Restart = types.RestartPolicy(policy)
		}
		if a.MaxRestarts != nil {
			spec.MaxRestarts = *a.MaxRestarts
		}
		// 健康检查：三选一
		var interval time.Duration
		if a.HealthInterval != "" {
			d, err := time.ParseDuration(a.HealthInterval)
			if err != nil {
				return nil, fmt.Errorf("app %q: invalid health_interval: %w", a.Name, err)
			}
			interval = d
		}
		switch {
		case a.HealthCmd != "":
			spec.Health = types.HealthSpec{Type: types.HealthExec, Target: a.HealthCmd, Interval: types.Duration(interval)}
		case a.HealthHTTP != "":
			spec.Health = types.HealthSpec{Type: types.HealthHTTP, Target: a.HealthHTTP, Interval: types.Duration(interval)}
		case a.HealthTCP != "":
			spec.Health = types.HealthSpec{Type: types.HealthTCP, Target: a.HealthTCP, Interval: types.Duration(interval)}
		}
		if err := spec.Normalize(); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
