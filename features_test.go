package main

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestWindowsFromHeaders(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1795600800")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.873")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1795900000")
	got := windowsFromHeaders(h, at)
	if w := got["five_hour"]; w.Utilization != 42 || w.ResetsAt.Unix() != 1795600800 || !w.At.Equal(at) {
		t.Fatalf("five_hour = %+v", w)
	}
	if w := got["seven_day"]; math.Abs(w.Utilization-87.3) > 1e-9 || w.ResetsAt.Unix() != 1795900000 {
		t.Fatalf("seven_day = %+v", w)
	}
	if len(windowsFromHeaders(http.Header{"Content-Type": {"x"}}, at)) != 0 {
		t.Fatal("headers without rate limits must give no windows")
	}
}

func TestMergeWindowsKeepsNewerAndReset(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	reset := t0.Add(48 * time.Hour)
	old := map[string]usageWindow{
		"seven_day":      {Utilization: 50, ResetsAt: reset, At: t0},
		"seven_day_opus": {Utilization: 5, ResetsAt: reset, At: t0},
	}
	fresh := map[string]usageWindow{"seven_day": {Utilization: 55, At: t0.Add(time.Minute)}}
	got := mergeWindows(old, fresh)
	if got["seven_day"].Utilization != 55 || !got["seven_day"].ResetsAt.Equal(reset) {
		t.Fatalf("seven_day = %+v; want newer reading with known reset", got["seven_day"])
	}
	if got["seven_day_opus"].Utilization != 5 {
		t.Fatal("windows the headers lack must be kept")
	}
	stale := map[string]usageWindow{"seven_day": {Utilization: 1, At: t0.Add(-time.Hour)}}
	if mergeWindows(got, stale)["seven_day"].Utilization != 55 {
		t.Fatal("an older reading must not replace a newer one")
	}
	if old["seven_day"].Utilization != 50 {
		t.Fatal("mergeWindows mutated its input")
	}
}

func resetGlobals(t *testing.T) {
	t.Helper()
	state.mu.Lock()
	state.accounts = map[string]accountState{}
	state.mu.Unlock()
	stats.mu.Lock()
	stats.counts, stats.served = map[string]int64{}, map[string]int64{}
	stats.mu.Unlock()
	history.mu.Lock()
	history.accounts, history.path, history.dirty = map[string][]sample{}, "", false
	history.mu.Unlock()
	currentConfig.Store(pluginConfig{})
}

func TestHandleUsageUpdatesAccountFromHeaders(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts["a"] = accountState{ID: "a", Label: "a@example.com", Error: "usage endpoint returned HTTP 429",
		Windows: map[string]usageWindow{"seven_day": {Utilization: 10, ResetsAt: now.Add(time.Hour), At: now.Add(-time.Hour)}}}
	state.mu.Unlock()

	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.25")
	raw, _ := json.Marshal(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: now, ResponseHeaders: h})
	handleUsage(raw)

	acc := snapshotAccounts()["a"]
	if acc.Windows["seven_day"].Utilization != 25 || acc.Error != "" || acc.HeadersAt.IsZero() {
		t.Fatalf("account after header update = %+v", acc)
	}
	if stats.served["a"] != 1 {
		t.Fatalf("served = %d; want 1", stats.served["a"])
	}
	// Other providers are ignored.
	raw, _ = json.Marshal(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", ResponseHeaders: h})
	handleUsage(raw)
	if stats.served["a"] != 1 {
		t.Fatal("non-Claude usage was counted")
	}
}

func TestShadowModeCountsButDoesNotRoute(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts["a"] = accountState{ID: "a", Windows: map[string]usageWindow{"seven_day": {ResetsAt: now.Add(time.Hour)}}}
	state.mu.Unlock()
	currentConfig.Store(pluginConfig{Shadow: true})

	req, _ := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{cand("a", 0)}})
	raw, err := pick(req)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Handled || env.Result.AuthID != "" {
		t.Fatalf("shadow pick = %+v; want unhandled", env.Result)
	}
	if stats.counts["a"] != 1 {
		t.Fatalf("would-pick count = %d; want 1", stats.counts["a"])
	}
}

func TestParseConfigShadowAndHistoryDir(t *testing.T) {
	cfg, err := parseConfig([]byte("\"Watch only, do not change routing\": true\n\"Folder for usage history\": /data/uiol\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Shadow || cfg.historyPath() != filepath.Join("/data/uiol", historyFile) {
		t.Fatalf("parsed %+v, path %s", cfg, cfg.historyPath())
	}
}

func TestPaceProjection(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cur := usageWindow{Utilization: 40, ResetsAt: now.Add(60 * time.Hour)}
	// 30 -> 40 over 10 hours: 1 point per hour, 60 hours left -> would reach 100.
	fast := paceFor([]point{{At: now.Add(-10 * time.Hour), Pct: 30}, {At: now, Pct: 40}}, cur, now)
	if !fast.Known || math.Abs(fast.PerHour-1) > 1e-9 || fast.Projected != 100 || fast.Unused != 0 || fast.RunsOutAt.IsZero() {
		t.Fatalf("fast pace = %+v", fast)
	}
	// 38 -> 40 over 10 hours: 0.2 per hour -> 52 at reset, 48 unused.
	slow := paceFor([]point{{At: now.Add(-10 * time.Hour), Pct: 38}}, cur, now)
	if math.Abs(slow.Projected-52) > 1e-9 || math.Abs(slow.Unused-48) > 1e-9 || !slow.RunsOutAt.IsZero() {
		t.Fatalf("slow pace = %+v", slow)
	}
	if math.Abs(slow.NeedPerHr-1) > 1e-9 {
		t.Fatalf("needed pace = %v; want 1", slow.NeedPerHr)
	}
	if paceFor([]point{{At: now.Add(-time.Hour), Pct: 39}}, cur, now).Known {
		t.Fatal("an hour of data must not give a rate")
	}
}

func TestHistoryBucketsAndPersistence(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	path := filepath.Join(t.TempDir(), "sub", historyFile)
	history.load(path)
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	reset := base.Add(50 * time.Hour)
	acc := func(pct float64) accountState {
		return accountState{Label: "Me@Example.com", Windows: map[string]usageWindow{"seven_day": {Utilization: pct, ResetsAt: reset}}}
	}
	history.add(acc(10), base)
	history.add(acc(11), base.Add(3*time.Minute))  // same bucket: replaces
	history.add(acc(15), base.Add(12*time.Minute)) // next bucket: appends
	if got := history.accounts["me@example.com"]; len(got) != 2 || got[0].U["seven_day"] != 11 {
		t.Fatalf("samples = %+v", got)
	}
	history.save()

	history.mu.Lock()
	history.accounts = map[string][]sample{}
	history.mu.Unlock()
	history.load(path)
	points := history.cycle("me@example.com", "seven_day", reset)
	if len(points) != 2 || points[1].Pct != 15 {
		t.Fatalf("reloaded cycle = %+v", points)
	}
	if len(history.cycle("me@example.com", "seven_day", reset.Add(7*24*time.Hour))) != 0 {
		t.Fatal("readings from another cycle leaked in")
	}
}

func TestTokenStale(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 32, 9, 0, time.FixedZone("", 5*3600+1800))
	if !tokenStale("2026-11-26T21:32:00+05:30", now) {
		t.Fatal("an expired token is stale")
	}
	if !tokenStale("2026-11-26T21:32:30+05:30", now) {
		t.Fatal("a token expiring within the slack is stale")
	}
	if tokenStale("2026-11-27T05:32:16+05:30", now) {
		t.Fatal("a fresh token is not stale")
	}
	if tokenStale("", now) || tokenStale("garbage", now) {
		t.Fatal("an unknown expiry counts as fresh")
	}
}

func TestHeadersClearLoginExpired(t *testing.T) {
	state.mu.Lock()
	saved := state.accounts
	state.accounts = map[string]accountState{"a": {ID: "a", Label: "a@example.com", Error: "usage endpoint returned HTTP 401", FailCount: 2, LoginExpired: true}}
	state.mu.Unlock()
	defer func() { state.mu.Lock(); state.accounts = saved; state.mu.Unlock() }()
	applyHeaderWindows("a", map[string]usageWindow{"five_hour": {Utilization: 10}}, time.Now())
	got := snapshotAccounts()["a"]
	if got.LoginExpired || got.Error != "" || got.FailCount != 0 {
		t.Fatalf("a served request should clear the expired flag, got %+v", got)
	}
}

func TestLoginExpiredAndRoom(t *testing.T) {
	if isLoginExpired("usage endpoint returned HTTP 401", 1, "active", "") {
		t.Fatal("a single HTTP 401 may be a token the proxy is about to renew")
	}
	if !isLoginExpired("usage endpoint returned HTTP 401", 2, "active", "") {
		t.Fatal("HTTP 401 on consecutive checks means the login expired")
	}
	if isLoginExpired(errTokenRenewing.Error(), 3, "active", "") {
		t.Fatal("a token waiting for renewal is not an expired login yet")
	}
	if !isLoginExpired(errTokenRenewing.Error(), renewingChecks, "active", "") {
		t.Fatal("a token that is never renewed means the login expired")
	}
	if !isLoginExpired("x", 1, "error", "refresh token: invalid_grant") {
		t.Fatal("invalid_grant means the login expired")
	}
	if isLoginExpired("usage endpoint returned HTTP 429", 5, "active", "") {
		t.Fatal("a rate limit is not an expired login")
	}
	now := time.Now()
	full := accountState{Windows: map[string]usageWindow{"seven_day": {Utilization: 100, ResetsAt: now.Add(time.Hour)}, "five_hour": {}}}
	if hasRoom(full, "seven_day", now) {
		t.Fatal("a full account has no room")
	}
	reset := accountState{Windows: map[string]usageWindow{"seven_day": {Utilization: 3, ResetsAt: now.Add(7 * 24 * time.Hour)}, "five_hour": {}}}
	if !hasRoom(reset, "seven_day", now) {
		t.Fatal("a freshly reset account has room")
	}
}
