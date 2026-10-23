package supervisor

// 接管来的进程，日志往往不在 gopm2 的日志文件里：它是应用自己（或它的 helper）
// 起的，stdout/stderr 指向应用自选的路径——dsh 的 dshmarket helper 就写到
// /tmp/dsh-market-restart-<stamp>.out.log。若不处理，接管之后 `gopm2 logs` 会
// 停在老进程的最后一行，用户看不到新进程的任何输出。
//
// 这里尽力把 fd 1/2 指向的常规文件跟随复制进 gopm2 自己的日志，让 `gopm2 logs`
// 继续可用。做不到时（fd 是管道、终端、/dev/null，或权限不足）只是不镜像，
// 不影响守护本身。

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/gitMFQ/gopm2/internal/logutil"
)

// startAdoptedLogMirror 尽力镜像 pid 的 stdout/stderr 到 name 的日志文件。
// 返回的 stop 用于在进程退出后停止镜像并回收 goroutine。
func (s *Supervisor) startAdoptedLogMirror(name string, pid int) (stop func()) {
	outPath, errPath, err := logutil.SafeLogPath(s.logsDir, name)
	if err != nil {
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// 同一目标文件只跟一次：out 与 err 指向同一个文件时避免双份内容
	seen := map[string]bool{}
	found := make([]string, 0, 2)
	for _, fd := range []int{1, 2} {
		target := fdTarget(pid, fd)
		if target == "" || seen[target] {
			continue
		}
		// 输出已经直接落在我们的日志里（应用保留了继承来的 fd）→ 不需要镜像，
		// 镜像反而会把同一内容写两遍
		if target == outPath || target == errPath {
			seen[target] = true
			continue
		}
		seen[target] = true
		found = append(found, target)

		dst := outPath
		if fd == 2 {
			dst = errPath
		}
		w, err := logutil.NewRotatingWriter(dst, 0, 0)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(target string, w *logutil.RotatingWriter) {
			defer wg.Done()
			defer w.Close()
			// 先补一段历史（最多 16KB），让接管的进程不是"日志从半句话开始"；
			// 然后从当前末尾跟随新增内容，两段衔接处不重叠。
			if tail, err := logutil.ReadTail(target, 16*1024); err == nil && len(tail) > 0 {
				_, _ = w.Write(tail)
			}
			_ = logutil.NewFollower(target).Follow(ctx, w)
		}(target, w)
	}

	if len(found) > 0 {
		// 留一行线索：新进程的输出原本落在哪里，出问题时好追
		note := fmt.Sprintf("[gopm2] 已接管应用自重启产生的进程 pid=%d，其原有输出位于: %v\n", pid, found)
		appendLogLine(outPath, note)
	}

	return func() {
		cancel()
		wg.Wait()
	}
}

// fdTarget 返回 pid 的 fd 指向的常规文件路径；不是常规文件时返回空串。
// 管道/套接字/终端拿到的是 "pipe:[123]" 这类伪路径，Stat 会失败，正好排除。
func fdTarget(pid, fd int) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, fd))
	if err != nil {
		return ""
	}
	fi, err := os.Stat(link)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return link
}

// appendLogLine 往日志文件追加一行（失败静默：只是少一行线索）。
func appendLogLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}
