package api

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gitMFQ/gopm2/internal/logutil"
)

// handleLogs 返回日志尾部，或 stream=true 时用 SSE 持续推送。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := s.appName(r)
	stream := r.URL.Query().Get("stream") // out|err，默认 out
	if r.URL.Query().Get("follow") == "true" {
		s.followLogsSSE(w, r, name, stream)
		return
	}
	data, err := s.svc.LogsTail(s.paths.LogsDir, name, stream, 64*1024)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// followLogsSSE 通过 SSE 实时推送日志。
func (s *Server) followLogsSSE(w http.ResponseWriter, r *http.Request, name, stream string) {
	outPath, errPath, err := logutil.SafeLogPath(s.paths.LogsDir, name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p := outPath
	if stream == "err" {
		p = errPath
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 先发送尾部历史
	tail, _ := logutil.ReadTail(p, 16*1024)
	if len(tail) > 0 {
		fmt.Fprintf(w, "data: %s\n\n", sseEscape(string(tail)))
		flusher.Flush()
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_ = logutil.NewFollower(p).Follow(r.Context(), pw)
	}()
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			fmt.Fprintf(w, "data: %s\n\n", sseEscape(string(buf[:n])))
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}
