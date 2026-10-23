package supervisor

// 本文件负责"应用自换替身"的识别：当受管进程被外部信号杀死后，它的重启可能
// 已经由应用自己完成了（典型例子：dsh 的 dshmarket 插件点重启 → 先 detach 一个
// helper，helper 等端口释放后拉起新的 dsh，再 SIGTERM 老进程）。
//
// 这类替身进程不在 gopm2 的进程组成员关系里（它 setsid 了），也不在
// exec.Cmd 的句柄表里。识别它的唯一可靠依据是**命令行签名**：argv 与 Spec
// 一致、cwd 一致、且启动时间晚于老进程的退出。命中即接管，避免 gopm2 再拉一个
// 造成双开抢端口。
//
// 识别是保守的：签名不一致、cwd 不一致、或启动时间早于退出事件，都不接管。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitMFQ/gopm2/internal/metrics"
	"github.com/gitMFQ/gopm2/internal/types"
)

// procInfo 是从 /proc 读到的一个进程的最小画像。
type procInfo struct {
	pid     int
	argv    []string
	cwd     string // 读不到时为空
	startAt time.Time
}

// procScanFunc 扫描系统进程，便于测试注入。
type procScanFunc func() ([]procInfo, error)

// scanProcs 读取 /proc 下所有可见进程的 argv、cwd 与启动时间。
// 权限不足（读不到别人的 /proc/<pid>/cmdline）时跳过该进程，不报错。
func scanProcs() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		info, ok := readProcInfo(pid)
		if !ok {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// readProcInfo 读取单个进程画像；进程已消失或不可读时返回 ok=false。
func readProcInfo(pid int) (procInfo, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(raw) == 0 {
		return procInfo{}, false
	}
	argv := splitNul(raw)
	if len(argv) == 0 {
		return procInfo{}, false
	}
	startAt, err := metrics.ProcessStartTime(pid)
	if err != nil {
		return procInfo{}, false
	}
	info := procInfo{pid: pid, argv: argv, startAt: startAt}
	if cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
		info.cwd = cwd
	}
	return info, true
}

// splitNul 把 cmdline（NUL 分隔）切成 argv，去掉结尾空串。
func splitNul(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// expectedArgv 返回 Spec 对应的命令行（不含解释器前缀）。
//
// 与 buildCmd 保持同一套解释规则：
//   - Args 非空 → [Command, Args...]（直接 exec，不经 shell）；
//   - Args 为空 → ["sh", "-c", Command]。
//
// 但"自重启"的应用往往用原始 argv 重新 exec 自己（dsh 就是这样：它记住
// process.argv 再 spawn），此时看到的 argv 是 [node, /usr/bin/dsh, --profile,
// web, ...]，既不是 sh -c 也不是裸 Command。因此匹配走 signatureMatches，
// 它额外接受"解释器 + 脚本 + 完全相同参数"这一形态。
func expectedArgv(spec *types.Spec) []string {
	if len(spec.Args) > 0 {
		return append([]string{spec.Command}, spec.Args...)
	}
	return []string{"sh", "-c", spec.Command}
}

// signatureMatches 判断一个进程画像是否就是该 Spec 的替身。
// 这是接管的主判据；cwd 与启动时间由调用方另行处理（cwd 只作排序偏好）。
//
// 判定顺序（任一成立即算匹配）：
//  1. argv 与 expectedArgv 完全一致（[Command Args...]，或 ["sh","-c",Command]）；
//  2. argv 形如 [<解释器>, <脚本>, <与 Spec.Args 完全相同的参数...>]，且脚本
//     的 basename 等于 Command 的 basename（覆盖 node /usr/bin/dsh ... 这类
//     应用级重启用原始 argv 重新 exec 自己的形态）。
func signatureMatches(spec *types.Spec, info procInfo) bool {
	if len(info.argv) == 0 {
		return false
	}
	want := expectedArgv(spec)
	if argvEqual(info.argv, want) {
		return true
	}
	// 解释器 + 脚本 + 参数：只在 Args 非空（直接 exec 形态）时套用，避免把
	// "sh -c 'foo bar'" 这种整串命令误配到任意同名脚本上。
	if len(spec.Args) > 0 && len(info.argv) >= 2 {
		script := info.argv[1]
		if filepath.Base(script) == filepath.Base(spec.Command) &&
			argvEqual(info.argv[2:], spec.Args) {
			return true
		}
	}
	return false
}

// argvEqual 逐项比较两个 argv。
func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameCwd 判断进程 cwd 是否与 Spec 一致。
// Spec.Cwd 为空表示继承 daemon 的 cwd；此时只有当进程 cwd 与 daemon 相同时才算一致。
// 注意这是**偏好**而非门槛：helper 可能在别的 cwd 拉起替身（见 findReplacement）。
func sameCwd(spec *types.Spec, info procInfo) bool {
	want := spec.Cwd
	if want == "" {
		if wd, err := os.Getwd(); err == nil {
			want = wd
		}
	}
	if want == "" || info.cwd == "" {
		// 信息不全时不做否定判断：命令行签名已经足够强
		return true
	}
	absWant, err1 := filepath.Abs(want)
	absGot, err2 := filepath.Abs(info.cwd)
	if err1 != nil || err2 != nil {
		return want == info.cwd
	}
	// 容器里 cwd 可能经挂载点映射，保守起见再比一次 EvalSymlinks 后的结果
	if absWant == absGot {
		return true
	}
	realWant, err1 := filepath.EvalSymlinks(absWant)
	realGot, err2 := filepath.EvalSymlinks(absGot)
	return err1 == nil && err2 == nil && realWant == realGot
}

// excludeSet 是绝不能接管的 pid 集合（自身、父进程、以及本 daemon 已受管的所有进程）。
type excludeSet map[int]struct{}

// add 加入若干 pid（忽略非正数）。
func (x excludeSet) add(pids ...int) {
	for _, pid := range pids {
		if pid > 0 {
			x[pid] = struct{}{}
		}
	}
}

// buildExcludeSet 收集自身与父进程，排除误接管。
func buildExcludeSet() excludeSet {
	set := excludeSet{}
	set.add(os.Getpid(), os.Getppid())
	return set
}

// findReplacement 在扫描结果里找出该 Spec 的替身进程，返回启动时间最新的那个。
//
// since 是"替身必须比它晚"的时间基准（老进程的启动时刻）：只有比被替换的进程
// 更新的进程才算替身，这样既覆盖"s"先起新进程再杀老的"交接，也覆盖"先杀老的
// 再起新的"。更早的进程（例如上一代遗留的孤儿）不会被接管，避免认错人。
//
// cwd 只作**排序偏好**，不作硬性条件：应用自己的重启 helper 未必在同一个 cwd
// 里拉起替身。dshmarket 就是反例——它按 `dirname(process.argv[1])` 取 cwd，
// 于是替身跑在 `/usr/bin`，而原进程跑在 `/root`；把 cwd 当硬条件会直接错过它。
// 命令行签名（含完整参数）才是主判据，cwd 一致只用来在多个候选中优先选。
//
// 认错人的主要风险是"另一个 app 的进程命令行一模一样"（例如两个 sleep 60）。
// 调用方必须把当前所有受管进程的 pid 放进 exclude，否则会把邻居的进程抢过来。
func findReplacement(spec *types.Spec, since time.Time, scan procScanFunc, exclude excludeSet) (int, bool) {
	procs, err := scan()
	if err != nil {
		return 0, false
	}
	best := 0
	var bestStart time.Time
	bestCwd := false
	for _, info := range procs {
		if _, skip := exclude[info.pid]; skip {
			continue
		}
		if !signatureMatches(spec, info) {
			continue
		}
		// 启动时间必须不早于 since（放宽 1s：/proc 启动时间精度是 jiffies，
		// 且时钟采样点与退出事件之间有毫秒级抖动）。
		if !since.IsZero() && info.startAt.Before(since.Add(-time.Second)) {
			continue
		}
		cwdOK := sameCwd(spec, info)
		better := best == 0 ||
			(cwdOK && !bestCwd) || // cwd 一致者优先
			(cwdOK == bestCwd && info.startAt.After(bestStart)) // 否则取更新的
		if better {
			best, bestStart, bestCwd = info.pid, info.startAt, cwdOK
		}
	}
	return best, best != 0
}
