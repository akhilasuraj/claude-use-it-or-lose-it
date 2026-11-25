package main

import (
	"os"
	"testing"
	"time"
)

// TestWritePreview renders the status page with sample data when RS_PREVIEW is set to an
// output path. Used only for visual checks: RS_PREVIEW=out.html go test -run TestWritePreview
func TestWritePreview(t *testing.T) {
	out := os.Getenv("RS_PREVIEW")
	if out == "" {
		t.Skip("RS_PREVIEW not set")
	}
	now := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{
		"a": {ID: "a", Label: "alex@example.com", Priority: 0, FetchedAt: now.Add(-9 * time.Second), Windows: map[string]usageWindow{
			"seven_day": {Utilization: 33, ResetsAt: now.Add(57 * time.Hour)},
			"five_hour": {Utilization: 59, ResetsAt: now.Add(48 * time.Minute)}}},
		"b": {ID: "b", Label: "sam.longer-address@example.org", Priority: -17, FetchedAt: now.Add(-8 * time.Second), Windows: map[string]usageWindow{
			"seven_day": {Utilization: 76, ResetsAt: now.Add(83 * time.Hour)},
			"five_hour": {Utilization: 0}}},
		"c": {ID: "c", Label: "jo@example.net", Priority: -27, FetchedAt: now.Add(-8 * time.Second), Windows: map[string]usageWindow{
			"seven_day": {Utilization: 100, ResetsAt: now.Add(30 * time.Hour)},
			"five_hour": {Utilization: 94, ResetsAt: now.Add(3 * time.Hour)}}},
		"d": {ID: "d", Label: "kim@example.com", Priority: 5, Error: "usage endpoint returned HTTP 401"},
	}
	state.lastRun = now.Add(-3 * time.Minute)
	state.nextRun = now.Add(27 * time.Minute)
	state.mu.Unlock()
	stats.record("a", "")
	stats.record("a", "")
	if err := os.WriteFile(out, renderPage("", now), 0o644); err != nil {
		t.Fatal(err)
	}
}
