package store

import (
	"testing"

	"github.com/gitMFQ/gopm2/internal/types"
)

func TestStoreAppCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	app := &types.App{
		Spec:   types.Spec{Name: "demo", Command: "sleep 100", Restart: types.RestartAlways},
		Status: types.Status{State: types.StateOnline, PID: 1234, DesiredOn: true},
	}
	if err := s.PutApp(app); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Spec.Command != "sleep 100" || got.Status.PID != 1234 {
		t.Fatalf("unexpected app: %+v", got)
	}

	list, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 app, got %d", len(list))
	}

	if err := s.DeleteApp("demo"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	apps := []*types.App{
		{Spec: types.Spec{Name: "a", Command: "sleep 1"}, Status: types.Status{DesiredOn: true}},
		{Spec: types.Spec{Name: "b", Command: "sleep 2"}, Status: types.Status{DesiredOn: false}},
	}
	if err := s.SaveSnapshot(apps); err != nil {
		t.Fatal(err)
	}
	specs, err := s.LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "a" {
		t.Fatalf("snapshot should only contain desired-on app 'a', got %+v", specs)
	}
}

func TestStoreReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutApp(&types.App{Spec: types.Spec{Name: "x", Command: "true"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetApp("x")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("app should survive reopen")
	}
}
