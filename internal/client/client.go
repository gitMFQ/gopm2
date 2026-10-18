// Package client 是 CLI 侧的 HTTP-over-Unix-socket 客户端。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gitMFQ/gopm2/internal/daemon"
	"github.com/gitMFQ/gopm2/internal/types"
)

// Client 调用 daemon API。
type Client struct {
	hc   *http.Client
	base string
}

// New 创建走 Unix socket 的客户端。
func New(paths daemon.Paths) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", paths.Socket)
		},
		// SSE 长连接不能被整体超时掐断，故 client 不设 Timeout；
		// 短时请求的超时由 do() 内 per-request context 控制。
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://d"}
}

// doShort 执行带 30s 超时的短请求。
func (c *Client) doShort(method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.doCtx(ctx, method, path, body, out)
}

func (c *Client) do(method, path string, body any, out any) error {
	return c.doShort(method, path, body, out)
}

func (c *Client) doCtx(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w (try `gopm2 daemon ensure`)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateApp 创建应用并可选立即启动。
func (c *Client) CreateApp(spec types.Spec, autostart bool) (*types.App, error) {
	req := map[string]any{
		"name":         spec.Name,
		"command":      spec.Command,
		"args":         spec.Args,
		"cwd":          spec.Cwd,
		"env":          spec.Env,
		"restart":      string(spec.Restart),
		"max_restarts": spec.MaxRestarts,
		"autostart":    autostart,
	}
	if spec.Health.Type != "" {
		req["health_type"] = string(spec.Health.Type)
		req["health_target"] = spec.Health.Target
		req["health_interval_sec"] = spec.Health.Interval.Seconds()
	}
	var app types.App
	if err := c.do(http.MethodPost, "/api/apps", req, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// List 返回全部应用。
func (c *Client) List() ([]*types.App, error) {
	var apps []*types.App
	if err := c.do(http.MethodGet, "/api/apps", nil, &apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// Get 返回单个应用。
func (c *Client) Get(name string) (*types.App, error) {
	var app types.App
	if err := c.do(http.MethodGet, "/api/apps/"+url.PathEscape(name), nil, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// Start / Stop / Restart / Delete。
func (c *Client) Start(name string) error {
	return c.do(http.MethodPost, "/api/apps/"+url.PathEscape(name)+"/start", nil, nil)
}
func (c *Client) Stop(name string) error {
	return c.do(http.MethodPost, "/api/apps/"+url.PathEscape(name)+"/stop", nil, nil)
}
func (c *Client) Restart(name string) error {
	return c.do(http.MethodPost, "/api/apps/"+url.PathEscape(name)+"/restart", nil, nil)
}
func (c *Client) Delete(name string) error {
	return c.do(http.MethodDelete, "/api/apps/"+url.PathEscape(name), nil, nil)
}

// Save 保存快照。
func (c *Client) Save() (int, error) {
	var out struct {
		Saved int `json:"saved"`
	}
	if err := c.do(http.MethodPost, "/api/save", nil, &out); err != nil {
		return 0, err
	}
	return out.Saved, nil
}

// Resurrect 恢复快照。
func (c *Client) Resurrect() ([]string, []string, error) {
	var out struct {
		Started []string `json:"started"`
		Errors  []string `json:"errors"`
	}
	if err := c.do(http.MethodPost, "/api/resurrect", nil, &out); err != nil {
		return nil, nil, err
	}
	return out.Started, out.Errors, nil
}

// Apply 应用配置。
func (c *Client) Apply(specs []*types.Spec, removeMissing bool) ([]string, error) {
	var out struct {
		Changed []string `json:"changed"`
	}
	q := ""
	if removeMissing {
		q = "?remove_missing=true"
	}
	if err := c.do(http.MethodPost, "/api/apply"+q, specs, &out); err != nil {
		return nil, err
	}
	return out.Changed, nil
}

// Export 导出全部 Spec。
func (c *Client) Export() ([]*types.Spec, error) {
	var specs []*types.Spec
	if err := c.do(http.MethodGet, "/api/export", nil, &specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// Import 导入 Spec。
func (c *Client) Import(specs []*types.Spec) ([]string, error) {
	var out struct {
		Changed []string `json:"changed"`
	}
	if err := c.do(http.MethodPost, "/api/import", specs, &out); err != nil {
		return nil, err
	}
	return out.Changed, nil
}

// LogsTail 返回日志尾部。
func (c *Client) LogsTail(name, stream string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/apps/"+url.PathEscape(name)+"/logs?stream="+stream, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// FollowLogs 流式读取 SSE 日志，写入 w，直到 ctx 取消。
func (c *Client) FollowLogs(ctx context.Context, name, stream string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/apps/"+url.PathEscape(name)+"/logs?follow=true&stream="+stream, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("follow logs: %s", resp.Status)
	}
	// 简化解析：逐行读，去掉 data: 前缀
	buf := make([]byte, 4096)
	var pending []byte
	flushData := func() {
		// pending 中含若干 "data: ...\n" 行与空行
		for len(pending) > 0 {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := pending[:i]
			pending = pending[i+1:]
			if bytes.HasPrefix(line, []byte("data: ")) {
				w.Write(line[6:])
				w.Write([]byte("\n"))
			}
		}
	}
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			flushData()
		}
		if err != nil {
			return err
		}
	}
}
