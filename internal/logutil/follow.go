package logutil

import (
	"context"
	"io"
	"os"
	"time"
)

// Follower 以 tail -f 语义跟踪一个文件，容忍轮转（inode 变化时重新打开）。
type Follower struct {
	path      string
	pollEvery time.Duration
}

// NewFollower 创建跟踪器。
func NewFollower(path string) *Follower {
	return &Follower{path: path, pollEvery: 500 * time.Millisecond}
}

// Follow 从文件末尾开始持续读取新增内容，写入 w，直到 ctx 取消。
// 文件不存在时等待其出现；发生轮转（inode 变化）时自动切换到新文件从头读。
func (f *Follower) Follow(ctx context.Context, w io.Writer) error {
	var (
		file      *os.File
		offset    int64
		firstOpen = true
	)
	open := func() error {
		nf, err := os.Open(f.path)
		if err != nil {
			return err
		}
		fi, err := nf.Stat()
		if err != nil {
			nf.Close()
			return err
		}
		if firstOpen {
			// 首次打开：定位到文件末尾，只推送新增内容
			// （历史由调用方通过 ReadTail 单独发送，避免重复）
			offset = fi.Size()
			firstOpen = false
		} else if offset > fi.Size() {
			// 轮转后新文件更小：从头读
			offset = 0
		}
		if _, err := nf.Seek(offset, io.SeekStart); err != nil {
			nf.Close()
			return err
		}
		file = nf
		return nil
	}
	defer func() {
		if file != nil {
			file.Close()
		}
	}()

	// 初次打开：定位到末尾
	for file == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := open(); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.pollEvery):
		}
	}

	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buf)
		if n > 0 {
			offset += int64(n)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		// EOF：检查是否发生轮转（路径对应 inode 变了）
		curFi, statErr := os.Stat(f.path)
		openFi, openErr := file.Stat()
		if statErr == nil && openErr == nil && !os.SameFile(curFi, openFi) {
			file.Close()
			file = nil
			offset = 0
			for file == nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := open(); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(f.pollEvery):
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.pollEvery):
		}
	}
}
