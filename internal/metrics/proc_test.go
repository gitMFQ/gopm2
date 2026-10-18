package metrics

import (
	"os/exec"
	"testing"
	"time"
)

func TestPidAlive(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	if !PidAlive(cmd.Process.Pid) {
		t.Fatal("expected sleep process to be alive")
	}
	if PidAlive(1 << 30) {
		t.Fatal("bogus pid should not be alive")
	}
}

func TestSample(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	pid := cmd.Process.Pid
	r := NewProcReader()
	time.Sleep(50 * time.Millisecond)
	smp, err := r.Sample(pid, time.Now().Add(-50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if smp.RSSBytes <= 0 {
		t.Fatalf("RSS should be > 0, got %d", smp.RSSBytes)
	}
	if smp.CPUPercent < 0 {
		t.Fatalf("CPU should be >= 0, got %f", smp.CPUPercent)
	}
	st, err := ProcessStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(st) > time.Minute {
		t.Fatalf("start time looks wrong: %v", st)
	}
}
