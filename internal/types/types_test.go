package types

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	// Marshal：输出秒（浮点），不是纳秒
	d := Duration(10 * time.Second)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "10" {
		t.Fatalf("marshal 10s = %s, want 10", b)
	}

	d2 := Duration(1500 * time.Millisecond)
	b2, _ := json.Marshal(d2)
	if string(b2) != "1.5" {
		t.Fatalf("marshal 1.5s = %s, want 1.5", b2)
	}

	// Unmarshal：数字（秒）
	var d3 Duration
	if err := json.Unmarshal([]byte("2.5"), &d3); err != nil {
		t.Fatal(err)
	}
	if d3.Std() != 2500*time.Millisecond {
		t.Fatalf("unmarshal 2.5 = %v", d3.Std())
	}

	// Unmarshal：字符串 duration
	var d4 Duration
	if err := json.Unmarshal([]byte(`"30s"`), &d4); err != nil {
		t.Fatal(err)
	}
	if d4.Std() != 30*time.Second {
		t.Fatalf("unmarshal 30s = %v", d4.Std())
	}

	// 非法值
	var d5 Duration
	if err := json.Unmarshal([]byte(`"abc"`), &d5); err == nil {
		t.Fatal("expected error for invalid duration string")
	}
}

func TestSpecJSONRoundTrip(t *testing.T) {
	spec := Spec{
		Name:        "x",
		Command:     "sleep",
		Args:        []string{"1"},
		Restart:     RestartAlways,
		MaxRestarts: 5,
		StopTimeout: Duration(10 * time.Second),
		Health: HealthSpec{
			Type:     HealthExec,
			Target:   "true",
			Interval: Duration(5 * time.Second),
			Timeout:  Duration(2 * time.Second),
			Failures: 3,
		},
		Env: map[string]string{"A": "1"},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got Spec
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.StopTimeout != spec.StopTimeout || got.Health.Interval != spec.Health.Interval {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, spec)
	}
	if got.Env["A"] != "1" || got.MaxRestarts != 5 {
		t.Fatalf("roundtrip lost fields: %+v", got)
	}
}
