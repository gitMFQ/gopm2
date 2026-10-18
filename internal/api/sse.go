package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// sseEscape 将任意文本转成合法的 SSE data 行（每行一个 data: 前缀）。
func sseEscape(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	return strings.ReplaceAll(s, "\n", "\ndata: ")
}

// handleEvents 推送 supervisor 事件流（状态变化等）。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := s.svc.Subscribe()
	defer s.svc.Unsubscribe(ch)

	// 立即发一条注释保持连接
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// 心跳注释行：防止浏览器/代理因空闲断开长连接
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}

// handleMetrics 返回单个应用的即时指标。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	smp, err := s.svc.Metrics(s.appName(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, smp)
}
