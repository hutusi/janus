package server

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{time.Second, "1s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m 0s"},
		{83 * time.Second, "1m 23s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{time.Hour, "1h 0m 0s"},
		{time.Hour + 5*time.Second, "1h 0m 5s"},
		{2*time.Hour + 5*time.Minute + 3*time.Second, "2h 5m 3s"},
		{-5 * time.Second, "0s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestDurationFunc(t *testing.T) {
	fn := templateFuncs["duration"].(func(start, end time.Time) string)
	start := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

	if got := fn(time.Time{}, time.Time{}); got != "—" {
		t.Errorf("never-started duration = %q, want —", got)
	}
	if got := fn(start, start.Add(83*time.Second)); got != "1m 23s" {
		t.Errorf("finished duration = %q, want 1m 23s", got)
	}
	if got := fn(start, start.Add(-time.Second)); got != "0s" {
		t.Errorf("end-before-start duration = %q, want 0s", got)
	}
	// Started but unfinished: snapshotted against now, so anything but the
	// zero-time and clamp sentinels.
	if got := fn(time.Now().Add(-90*time.Second), time.Time{}); got == "—" || got == "0s" {
		t.Errorf("in-progress duration = %q, want a live elapsed value", got)
	}
}

func TestMillisFunc(t *testing.T) {
	fn := templateFuncs["millis"].(func(t time.Time) int64)
	// A .9s fraction in a non-UTC zone: the sub-second part must survive
	// (whole-second anchors wobble the ticker by ±1s) and the zone must not
	// matter — epoch milliseconds are absolute.
	in := time.Date(2026, 7, 22, 17, 15, 0, 900e6, time.FixedZone("CST", 8*3600))
	got := fn(in)
	if got != in.UnixMilli() || got%1000 != 900 {
		t.Errorf("millis = %d, want %d with the 900ms fraction intact", got, in.UnixMilli())
	}
}
