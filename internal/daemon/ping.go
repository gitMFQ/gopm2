package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// pingSocket 通过 unix socket 请求 /healthz。
func pingSocket(socketPath string, timeout time.Duration) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Get("http://d/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
}
