package logutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer 是并发安全的 bytes.Buffer。Follower.Follow 在独立 goroutine 中
// 写入，测试主 goroutine 读取，不加锁会被 -race 判定为数据竞争。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRotatingWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	w, err := NewRotatingWriter(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 写约 5KB，应触发多次轮转
	chunk := bytes.Repeat([]byte("a"), 500)
	for i := 0; i < 10; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	// 主文件不应超过上限太多
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1024 {
		t.Fatalf("main log size %d exceeds max", fi.Size())
	}
	// 至少存在 .1
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated file: %v", err)
	}
	// 不应存在超过 3 个备份
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("should not keep more than 3 backups")
	}
}

func TestSafeLogPath(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := SafeLogPath(dir, "ok-name_1.2"); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"../evil", "a/b", "..", ""} {
		if _, _, err := SafeLogPath(dir, bad); err == nil {
			t.Fatalf("bad name %q accepted", bad)
		}
	}
}

func TestFollower(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewFollower(path).Follow(ctx, &buf) }()

	// Follower 初始跳到末尾，"hello" 不应出现；追加的新内容应出现
	time.Sleep(200 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, "world")
	f.Close()

	deadline := time.After(3 * time.Second)
	for !strings.Contains(buf.String(), "world") {
		select {
		case <-deadline:
			t.Fatalf("follower did not deliver new data, got %q", buf.String())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	// 回归（C1）：Follower 必须从文件末尾开始，历史内容 "hello" 不得出现
	if strings.Contains(buf.String(), "hello") {
		t.Fatalf("follower replayed history from head: %q", buf.String())
	}
	cancel()
	<-done
}
