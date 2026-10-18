// Package logutil 提供按大小轮转的日志写入器和日志文件实时跟踪器。
package logutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultMaxSize 单个日志文件默认最大 10MB。
	DefaultMaxSize = 10 * 1024 * 1024
	// DefaultMaxBackups 默认保留 5 个轮转文件。
	DefaultMaxBackups = 5
)

// RotatingWriter 实现 io.Writer，写入超过 MaxSize 时轮转：
// name.log -> name.log.1 -> ... -> name.log.N（最老的被丢弃）。
type RotatingWriter struct {
	path       string
	maxSize    int64
	maxBackups int

	mu   sync.Mutex
	file *os.File
	size int64
}

// NewRotatingWriter 创建轮转写入器。maxSize/maxBackups <= 0 时使用默认值。
func NewRotatingWriter(path string, maxSize int64, maxBackups int) (*RotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if maxBackups <= 0 {
		maxBackups = DefaultMaxBackups
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &RotatingWriter{path: path, maxSize: maxSize, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = fi.Size()
	return nil
}

// Write 追加写入，必要时先轮转。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxSize && w.size > 0 {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked 关闭当前文件并做 N 级 rename。
func (w *RotatingWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	// 丢弃最老的
	oldest := fmt.Sprintf("%s.%d", w.path, w.maxBackups)
	os.Remove(oldest)
	for i := w.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

// Close 关闭底层文件。
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// Path 返回当前日志文件路径。
func (w *RotatingWriter) Path() string { return w.path }

// SafeLogPath 校验 name 生成的日志路径不会逃逸出 logsDir，返回 out/err 两个路径。
func SafeLogPath(logsDir, name string) (outPath, errPath string, err error) {
	if name == "" || name == "." || name == ".." {
		return "", "", fmt.Errorf("invalid app name %q", name)
	}
	for _, c := range name {
		if c == '/' || c == '\\' || c == 0 {
			return "", "", fmt.Errorf("invalid app name %q: contains path separator", name)
		}
	}
	base, err := filepath.Abs(logsDir)
	if err != nil {
		return "", "", err
	}
	out := filepath.Join(base, name+"-out.log")
	errF := filepath.Join(base, name+"-err.log")
	if filepath.Dir(out) != base || filepath.Dir(errF) != base {
		return "", "", fmt.Errorf("path escapes logs dir")
	}
	return out, errF, nil
}

// ReadTail 读取文件最后 maxBytes 字节（用于初次展示日志）。
func ReadTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
