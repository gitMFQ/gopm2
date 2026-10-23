package supervisor

import (
	"os"
	"testing"
	"time"

	"github.com/gitMFQ/gopm2/internal/types"
)

// now 是这些单测里的"现在"，让启动时间过滤可预期。
var now = time.Now()

// mkProc 构造一个扫描结果条目。
func mkProc(pid int, start time.Time, argv ...string) procInfo {
	return procInfo{pid: pid, argv: argv, cwd: "/tmp", startAt: start}
}

func TestSignatureMatches(t *testing.T) {
	cases := []struct {
		name string
		spec types.Spec
		argv []string
		want bool
	}{
		{
			name: "直接 exec 形态完全一致",
			spec: types.Spec{Command: "sleep", Args: []string{"60"}},
			argv: []string{"sleep", "60"},
			want: true,
		},
		{
			name: "解释器+脚本+参数（dsh 自重启的真实形态）",
			spec: types.Spec{Command: "dsh", Args: []string{"--profile", "web", "--no-open"}},
			argv: []string{"node", "/usr/bin/dsh", "--profile", "web", "--no-open"},
			want: true,
		},
		{
			name: "解释器+脚本但参数不同 → 不认",
			spec: types.Spec{Command: "dsh", Args: []string{"--profile", "web"}},
			argv: []string{"node", "/usr/bin/dsh", "--profile", "other"},
			want: false,
		},
		{
			name: "参数相同但脚本同名不同路径之外的程序 → 不认",
			spec: types.Spec{Command: "dsh", Args: []string{"--profile", "web"}},
			argv: []string{"node", "/opt/other/node-web", "--profile", "web"},
			want: false,
		},
		{
			name: "参数是前缀但多了尾巴 → 不认（避免把别人的进程认成替身）",
			spec: types.Spec{Command: "sleep", Args: []string{"60"}},
			argv: []string{"sleep", "60", "extra"},
			want: false,
		},
		{
			name: "shell 形态（Args 为空）",
			spec: types.Spec{Command: "node server.js"},
			argv: []string{"sh", "-c", "node server.js"},
			want: true,
		},
		{
			name: "shell 形态但命令不同 → 不认",
			spec: types.Spec{Command: "node server.js"},
			argv: []string{"sh", "-c", "node other.js"},
			want: false,
		},
		{
			name: "空 argv → 不认",
			spec: types.Spec{Command: "sleep", Args: []string{"60"}},
			argv: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := procInfo{pid: 1234, argv: tc.argv, cwd: "/tmp", startAt: now}
			if got := signatureMatches(&tc.spec, info); got != tc.want {
				t.Fatalf("signatureMatches = %v, want %v (argv=%q)", got, tc.want, tc.argv)
			}
		})
	}
}

func TestFindReplacement(t *testing.T) {
	spec := &types.Spec{Name: "dsh", Command: "dsh", Args: []string{"--profile", "web"}}
	since := now.Add(-time.Minute)
	// spec.Cwd 为空时 sameCwd 会与 daemon 的 cwd 比较；用真实 cwd 让候选项通过校验
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	scan := func(procs ...procInfo) procScanFunc {
		return func() ([]procInfo, error) { return procs, nil }
	}

	t.Run("命中签名一致且启动更晚的进程", func(t *testing.T) {
		procs := []procInfo{
			mkProc(101, now.Add(-time.Hour), "sleep", "60"),
			mkProc(102, now.Add(10*time.Second), "node", "/usr/bin/dsh", "--profile", "web"),
		}
		for i := range procs {
			procs[i].cwd = here
		}
		pid, ok := findReplacement(spec, since, scan(procs...), excludeSet{})
		if !ok || pid != 102 {
			t.Fatalf("want pid 102, got pid=%d ok=%v", pid, ok)
		}
	})

	t.Run("排除集里的 pid 不接管", func(t *testing.T) {
		procs := []procInfo{mkProc(102, now.Add(10*time.Second), "node", "/usr/bin/dsh", "--profile", "web")}
		procs[0].cwd = here
		ex := excludeSet{}
		ex.add(102)
		if pid, ok := findReplacement(spec, since, scan(procs...), ex); ok {
			t.Fatalf("excluded pid should not be adopted, got %d", pid)
		}
	})

	t.Run("启动时间早于基准的进程不接管", func(t *testing.T) {
		procs := []procInfo{mkProc(102, now.Add(-2*time.Minute), "node", "/usr/bin/dsh", "--profile", "web")}
		procs[0].cwd = here
		if pid, ok := findReplacement(spec, since, scan(procs...), excludeSet{}); ok {
			t.Fatalf("stale process should not be adopted, got %d", pid)
		}
	})

	t.Run("cwd 不一致仍接管（签名一致才是主判据）", func(t *testing.T) {
		// 真实反例：dshmarket 的 helper 按 dirname(entry) 取 cwd，替身跑在
		// /usr/bin，而原进程跑在 /root —— 不能因为 cwd 不同就漏掉替身。
		procs := []procInfo{mkProc(102, now.Add(10*time.Second), "node", "/usr/bin/dsh", "--profile", "web")}
		procs[0].cwd = "/usr/bin"
		pid, ok := findReplacement(spec, since, scan(procs...), excludeSet{})
		if !ok || pid != 102 {
			t.Fatalf("cwd mismatch must not block adoption, got pid=%d ok=%v", pid, ok)
		}
	})

	t.Run("多个候选时 cwd 一致者优先", func(t *testing.T) {
		procs := []procInfo{
			mkProc(102, now.Add(9*time.Second), "node", "/usr/bin/dsh", "--profile", "web"), // cwd 不一致
			mkProc(103, now.Add(5*time.Second), "node", "/usr/bin/dsh", "--profile", "web"), // cwd 一致
		}
		procs[0].cwd = "/usr/bin"
		procs[1].cwd = here
		pid, ok := findReplacement(spec, since, scan(procs...), excludeSet{})
		if !ok || pid != 103 {
			t.Fatalf("cwd-matching candidate should win, got pid=%d ok=%v", pid, ok)
		}
	})

	t.Run("多个候选时取最新的一个", func(t *testing.T) {
		procs := []procInfo{
			mkProc(102, now.Add(5*time.Second), "node", "/usr/bin/dsh", "--profile", "web"),
			mkProc(103, now.Add(9*time.Second), "node", "/usr/bin/dsh", "--profile", "web"),
		}
		for i := range procs {
			procs[i].cwd = here
		}
		pid, ok := findReplacement(spec, since, scan(procs...), excludeSet{})
		if !ok || pid != 103 {
			t.Fatalf("want newest pid 103, got pid=%d ok=%v", pid, ok)
		}
	})
}

func TestSameCwd(t *testing.T) {
	spec := &types.Spec{Command: "x", Cwd: "/tmp"}
	if !sameCwd(spec, procInfo{cwd: "/tmp"}) {
		t.Fatal("same cwd should match")
	}
	if sameCwd(spec, procInfo{cwd: "/var"}) {
		t.Fatal("different cwd should not match")
	}
	// cwd 信息缺失时不做否定判断（命令行签名已足够强）
	if !sameCwd(spec, procInfo{cwd: ""}) {
		t.Fatal("missing cwd should be treated as unknown, not mismatch")
	}
}

// TestAdoptWindowSemantics 验证窗口取值的语义不因 Normalize 而失真：
// 正数保留、零值变默认、负数保持负数（负数=关闭接管）。
func TestAdoptWindowSemantics(t *testing.T) {
	// 显式设置的正数必须原样保留
	s := types.Spec{Name: "a", Command: "sleep 1", AdoptWindow: types.Duration(3 * time.Second)}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s.AdoptWindow.Std() != 3*time.Second {
		t.Fatalf("explicit window lost: %v", s.AdoptWindow.Std())
	}

	// 零值 → 默认窗口
	d := types.Spec{Name: "b", Command: "sleep 1"}
	if err := d.Normalize(); err != nil {
		t.Fatal(err)
	}
	if d.AdoptWindow.Std() != types.DefaultAdoptWindow {
		t.Fatalf("zero should become default, got %v", d.AdoptWindow.Std())
	}

	// 负数 → 保持负数，接管被禁用
	n := types.Spec{Name: "c", Command: "sleep 1", AdoptWindow: types.Duration(-1)}
	if err := n.Normalize(); err != nil {
		t.Fatal(err)
	}
	if n.AdoptWindow.Std() >= 0 {
		t.Fatalf("negative (disabled) must stay negative, got %v", n.AdoptWindow.Std())
	}
}
