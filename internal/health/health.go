// Package health 实现 HTTP/TCP/exec 三种健康检查。
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"

	"github.com/gitMFQ/gopm2/internal/types"
)

// Checker 执行一次健康检查。
type Checker interface {
	Check(ctx context.Context) error
}

// New 根据 HealthSpec 构造 Checker。
func New(hs types.HealthSpec) Checker {
	switch hs.Type {
	case types.HealthHTTP:
		return &httpChecker{url: hs.Target}
	case types.HealthTCP:
		return &tcpChecker{addr: hs.Target}
	case types.HealthExec:
		return &execChecker{cmd: hs.Target}
	default:
		return &noopChecker{}
	}
}

type noopChecker struct{}

func (c *noopChecker) Check(ctx context.Context) error { return nil }

type httpChecker struct{ url string }

func (c *httpChecker) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("http health: status %d", resp.StatusCode)
}

type tcpChecker struct{ addr string }

func (c *tcpChecker) Check(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

type execChecker struct{ cmd string }

func (c *execChecker) Check(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", c.cmd)
	return cmd.Run()
}
