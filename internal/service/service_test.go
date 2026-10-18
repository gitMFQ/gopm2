package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gitMFQ/gopm2/internal/store"
	"github.com/gitMFQ/gopm2/internal/supervisor"
	"github.com/gitMFQ/gopm2/internal/types"
)

func newTestService(t *testing.T) (*Service, *supervisor.Supervisor) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sup := supervisor.New(filepath.Join(dir, "logs"), st)
	return New(sup, st), sup
}

func online(t *testing.T, svc *Service, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := svc.Get(name)
		if a != nil && a.Status.State == types.StateOnline && a.Status.PID > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("app %q not online", name)
}

// TestApplyIdempotent 验证 C6：spec 未变时重复 apply 不重启进程。
func TestApplyIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	spec := &types.Spec{Name: "idem", Command: "sleep", Args: []string{"300"}, Restart: types.RestartNever}

	changed, err := svc.Apply([]*types.Spec{spec}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("first apply should report 1 change, got %v", changed)
	}
	online(t, svc, "idem")
	first, _ := svc.Get("idem")
	pid1, r1 := first.Status.PID, first.Status.Restarts

	// 相同 spec 再 apply：不应变化、不应重启
	changed2, err := svc.Apply([]*types.Spec{spec}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed2) != 0 {
		t.Fatalf("idempotent apply should report 0 change, got %v", changed2)
	}
	second, _ := svc.Get("idem")
	if second.Status.PID != pid1 {
		t.Fatalf("pid changed on idempotent apply: %d -> %d", pid1, second.Status.PID)
	}
	if second.Status.Restarts != r1 {
		t.Fatalf("restarts changed on idempotent apply: %d -> %d", r1, second.Status.Restarts)
	}
	svc.Stop("idem")
}

// TestApplySpecChangeRestarts 验证：spec 变化时 apply 会重启进程。
func TestApplySpecChangeRestarts(t *testing.T) {
	svc, _ := newTestService(t)
	spec := &types.Spec{Name: "chg", Command: "sleep", Args: []string{"300"}, Restart: types.RestartNever}
	if _, err := svc.Apply([]*types.Spec{spec}, false); err != nil {
		t.Fatal(err)
	}
	online(t, svc, "chg")
	first, _ := svc.Get("chg")
	pid1 := first.Status.PID

	// 改 cwd（spec 变化）再 apply：应重启，pid 变化
	spec2 := &types.Spec{Name: "chg", Command: "sleep", Args: []string{"300"}, Restart: types.RestartNever, Cwd: "/tmp"}
	changed, err := svc.Apply([]*types.Spec{spec2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("spec-change apply should report 1 change, got %v", changed)
	}
	online(t, svc, "chg")
	second, _ := svc.Get("chg")
	if second.Status.PID == pid1 {
		t.Fatal("pid should change after spec-change apply")
	}
	svc.Stop("chg")
}

// TestSpecEqual 验证 spec 比较逻辑。
func TestSpecEqual(t *testing.T) {
	a := &types.Spec{Name: "x", Command: "sleep", Args: []string{"1"}, Restart: types.RestartAlways, Env: map[string]string{"A": "1"}}
	b := &types.Spec{Name: "x", Command: "sleep", Args: []string{"1"}, Restart: types.RestartAlways, Env: map[string]string{"A": "1"}}
	if !specEqual(a, b) {
		t.Fatal("identical specs should be equal")
	}
	c := &types.Spec{Name: "x", Command: "sleep", Args: []string{"1"}, Restart: types.RestartNever, Env: map[string]string{"A": "1"}}
	if specEqual(a, c) {
		t.Fatal("different restart policy should not be equal")
	}
	d := &types.Spec{Name: "x", Command: "sleep", Args: []string{"1"}, Restart: types.RestartAlways, Env: map[string]string{"A": "2"}}
	if specEqual(a, d) {
		t.Fatal("different env should not be equal")
	}
}
