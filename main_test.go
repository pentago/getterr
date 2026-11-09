package main

import (
	"testing"
	"time"
)

func TestParsePace(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"6h": 6 * time.Hour, "1d": day, "2w": 14 * day, "1m": 30 * day,
	} {
		if got, err := parsePace(in); err != nil || got != want {
			t.Errorf("parsePace(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "h", "0h", "-1d", "1y", "1.5h", "1H", "99999999999m"} {
		if _, err := parsePace(in); err == nil {
			t.Errorf("parsePace(%q) succeeded; want error", in)
		}
	}
}
