package main

import (
	"os"
	"testing"
	"time"
)

// TestWritePreview renders the status page with sample data when UIOL_PREVIEW is set to an
// output path. Used only for visual checks: UIOL_PREVIEW=out.html go test -run TestWritePreview
func TestWritePreview(t *testing.T) {
	out := os.Getenv("UIOL_PREVIEW")
	if out == "" {
		t.Skip("UIOL_PREVIEW not set")
	}
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
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
		"d": {ID: "d", Label: "kim@example.com", Priority: 5, Error: "usage endpoint returned HTTP 401", Problem: problemUnauthorized, FailCount: 2},
	}
	state.lastRun = now.Add(-3 * time.Minute)
	state.nextRun = now.Add(27 * time.Minute)
	state.mu.Unlock()
	stats.record("a")
	stats.record("a")
	stats.recordServed("a")
	stats.recordServed("a")
	stats.recordServed("a")
	state.mu.Lock()
	d := state.accounts["d"]
	d.LoginExpired = true
	state.accounts["d"] = d
	state.mu.Unlock()
	// Synthetic history: a daytime-heavy pattern over the current cycle of each account.
	history.mu.Lock()
	history.accounts = map[string][]sample{}
	history.mu.Unlock()
	for _, acc := range snapshotAccounts() {
		w, ok := acc.Windows["seven_day"]
		if !ok || w.ResetsAt.IsZero() {
			continue
		}
		start := w.ResetsAt.Add(-7 * 24 * time.Hour)
		elapsed := now.Sub(start)
		for t := start; t.Before(now); t = t.Add(30 * time.Minute) {
			f := float64(t.Sub(start)) / float64(elapsed)
			pct := w.Utilization * f * f * (3 - 2*f)
			a := acc
			a.Windows = map[string]usageWindow{"seven_day": {Utilization: pct, ResetsAt: w.ResetsAt}}
			history.add(a, t)
		}
	}
	if os.Getenv("UIOL_SHADOW") != "" {
		currentConfig.Store(pluginConfig{Shadow: true})
	}
	if err := os.WriteFile(out, renderPage("", now), 0o644); err != nil {
		t.Fatal(err)
	}
}
