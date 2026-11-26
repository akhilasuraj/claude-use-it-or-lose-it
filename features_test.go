package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
	state.lastRun, state.nextRun, state.lastErr = time.Time{}, time.Time{}, ""
	state.running, state.cleared = false, map[string]time.Time{}
	state.mu.Unlock()
	stats.mu.Lock()
	stats.counts, stats.served = map[string]int64{}, map[string]int64{}
	stats.lastID, stats.lastAt = "", time.Time{}
	stats.mu.Unlock()
	history.mu.Lock()
	history.accounts, history.path, history.dirty = map[string][]sample{}, "", false
	history.isLoaded, history.pendingLoad = false, false
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
	if n := servedCount("a"); n != 1 {
		t.Fatalf("served = %d; want 1", n)
	}
	// Other providers are ignored.
	raw, _ = json.Marshal(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", ResponseHeaders: h})
	handleUsage(raw)
	if servedCount("a") != 1 {
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
	if math.Abs(slow.NeededPerHour-1) > 1e-9 {
		t.Fatalf("needed pace = %v; want 1", slow.NeededPerHour)
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
	// Recent readings, so loading the file back does not prune them as too old.
	base := time.Now().Add(-time.Hour).Truncate(historyBucket)
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
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	state.mu.Lock()
	state.accounts = map[string]accountState{"a": {ID: "a", Label: "a@example.com", Error: "usage endpoint returned HTTP 401", Problem: problemUnauthorized, FailCount: 2, LoginExpired: true}}
	state.mu.Unlock()
	applyHeaderWindows("a", map[string]usageWindow{"five_hour": {Utilization: 10}}, time.Now())
	got := snapshotAccounts()["a"]
	if got.LoginExpired || got.Error != "" || got.FailCount != 0 || got.Problem != problemNone {
		t.Fatalf("a served request should clear the expired flag, got %+v", got)
	}
}

func TestLoginExpiredAndRoom(t *testing.T) {
	if isLoginExpired(problemUnauthorized, 1, "active", "") {
		t.Fatal("a single HTTP 401 may be a token the proxy is about to renew")
	}
	if !isLoginExpired(problemUnauthorized, 2, "active", "") {
		t.Fatal("HTTP 401 on consecutive checks means the login expired")
	}
	if isLoginExpired(problemRenewing, 3, "active", "") {
		t.Fatal("a token waiting for renewal is not an expired login yet")
	}
	if !isLoginExpired(problemRenewing, renewingChecks, "active", "") {
		t.Fatal("a token that is never renewed means the login expired")
	}
	if !isLoginExpired(problemOther, 1, "error", "refresh token: invalid_grant") {
		t.Fatal("invalid_grant means the login expired")
	}
	if isLoginExpired(problemOther, 5, "active", "") {
		t.Fatal("a rate limit is not an expired login")
	}
	now := time.Now()
	full := accountState{Windows: map[string]usageWindow{"seven_day": {Utilization: 100, ResetsAt: now.Add(time.Hour)}, "five_hour": {}}}
	if hasRoom(full, now) {
		t.Fatal("a full account has no room")
	}
	reset := accountState{Windows: map[string]usageWindow{"seven_day": {Utilization: 3, ResetsAt: now.Add(7 * 24 * time.Hour)}, "five_hour": {}}}
	if !hasRoom(reset, now) {
		t.Fatal("a freshly reset account has room")
	}
}

func TestOnlyAllModelsAndFiveHourLimitsCount(t *testing.T) {
	now := time.Now()
	w := func(u float64, d time.Duration) usageWindow {
		return usageWindow{Utilization: u, ResetsAt: now.Add(d), At: now}
	}
	cands := []pluginapi.SchedulerAuthCandidate{
		{ID: "later", Provider: "claude"},
		{ID: "sooner", Provider: "claude"},
	}
	accounts := map[string]accountState{
		"later": {ID: "later", Windows: map[string]usageWindow{
			"seven_day": w(10, 96*time.Hour), "five_hour": w(0, time.Hour),
			// A model-specific reading resetting earlier must not change the order.
			"seven_day_opus": w(5, 2*time.Hour)}},
		"sooner": {ID: "sooner", Windows: map[string]usageWindow{
			"seven_day": w(50, 24*time.Hour), "five_hour": w(0, time.Hour),
			// A full model-specific reading must not make the account full.
			"seven_day_sonnet": w(100, 24*time.Hour)}},
	}
	// An old config.yaml that still has the removed setting parses, and the setting has no effect.
	cfg, err := parseConfig([]byte("\"Weekly limit to order by\": Opus\nweekly_window: seven_day_sonnet\n"))
	if err != nil {
		t.Fatal(err)
	}
	if id := choose(cands, accounts, cfg, now); id != "sooner" {
		t.Fatalf("the all-models weekly reset must decide, got %q", id)
	}
	if !hasRoom(accounts["sooner"], now) {
		t.Fatal("a full model-specific reading must not block clearing a cooldown")
	}
}

func servedCount(id string) int64 {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	return stats.served[id]
}

func TestClassifyProblem(t *testing.T) {
	cases := []struct {
		err  error
		want checkProblem
	}{
		{nil, problemNone},
		{errNoAccessToken, problemNoToken},
		{errTokenRenewing, problemRenewing},
		{httpStatusError{code: 401}, problemUnauthorized},
		{fmt.Errorf("wrapped: %w", httpStatusError{code: 401}), problemUnauthorized},
		{httpStatusError{code: 500}, problemOther},
		{errors.New("decode credential: bad json"), problemOther},
	}
	for _, c := range cases {
		if got := classifyProblem(c.err); got != c.want {
			t.Errorf("classifyProblem(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestFailureShownAndRetry(t *testing.T) {
	once := accountState{Problem: problemOther, FailCount: 1, FailStreak: 1}
	if once.failureShown() {
		t.Fatal("a single passing failure is not shown")
	}
	if !(accountState{Problem: problemOther, FailCount: 2}).failureShown() {
		t.Fatal("a repeated failure is shown")
	}
	if !(accountState{Problem: problemRenewing, FailCount: 1}).failureShown() {
		t.Fatal("a token being renewed is always shown")
	}
	if !once.retrySoon() {
		t.Fatal("a failed check is retried soon")
	}
	if !(accountState{Problem: problemOther, FailCount: retryChecks, FailStreak: retryChecks}).retrySoon() {
		t.Fatal("the last of the retryChecks retries is still soon")
	}
	if (accountState{Problem: problemOther, FailCount: retryChecks + 1, FailStreak: retryChecks + 1}).retrySoon() {
		t.Fatal("after retryChecks retries the normal interval applies")
	}
	if (accountState{Problem: problemUnauthorized, FailCount: 2, LoginExpired: true}).retrySoon() {
		t.Fatal("an expired login is not retried every two minutes")
	}
}

func TestParseBoolAndInt(t *testing.T) {
	for _, v := range []any{true, "yes", "On", "1", 1, "TRUE"} {
		if b, ok := parseBool(v); !b || !ok {
			t.Errorf("parseBool(%v) = %v, %v; want true", v, b, ok)
		}
	}
	for _, v := range []any{false, "no", "off", "0", 0, "False"} {
		if b, ok := parseBool(v); b || !ok {
			t.Errorf("parseBool(%v) = %v, %v; want false", v, b, ok)
		}
	}
	for _, v := range []any{"maybe", 2, 1.5, nil} {
		if _, ok := parseBool(v); ok {
			t.Errorf("parseBool(%v) accepted an unclear value", v)
		}
	}
	cfg, err := parseConfig([]byte("\"Move full accounts to the bottom\": \"off\"\n\"Watch only, do not change routing\": \"yes\"\n\"Check usage every (minutes)\": \" 12 \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.deprioritizeExhausted() || !cfg.Shadow || cfg.interval() != 12*time.Minute {
		t.Fatalf("parsed %+v", cfg)
	}
	cfg, _ = parseConfig([]byte("\"Move full accounts to the bottom\": \"maybe\"\n"))
	if !cfg.deprioritizeExhausted() {
		t.Fatal("an unclear value keeps the default")
	}
}

func TestPaceNeedsReadingsThatSpanTwoHours(t *testing.T) {
	now := time.Now()
	cur := usageWindow{Utilization: 50, ResetsAt: now.Add(48 * time.Hour), At: now.Add(-3 * time.Hour)}
	// One old reading, which is also the current one: no span, so no pace.
	if p := paceFor([]point{{At: cur.At, Pct: 50}}, cur, now); p.Known {
		t.Fatalf("pace from a single reading: %+v", p)
	}
	// Readings that span one hour, ending three hours ago: still too short.
	short := []point{{At: now.Add(-4 * time.Hour), Pct: 40}, {At: cur.At, Pct: 50}}
	if p := paceFor(short, cur, now); p.Known {
		t.Fatalf("pace from one hour of readings: %+v", p)
	}
	// Two hours of readings: 10 points in 2 hours is 5 per hour, not diluted by the quiet hours since.
	cur.At = now.Add(-time.Hour)
	long := []point{{At: now.Add(-3 * time.Hour), Pct: 40}, {At: cur.At, Pct: 50}}
	if p := paceFor(long, cur, now); !p.Known || math.Abs(p.PerHour-5) > 1e-9 {
		t.Fatalf("pace = %+v; want 5 per hour", p)
	}
}

func TestPageMarksCoolingDownAccounts(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	w := func(d time.Duration) map[string]usageWindow {
		return map[string]usageWindow{"seven_day": {Utilization: 10, ResetsAt: now.Add(d)}, "five_hour": {Utilization: 0}}
	}
	state.mu.Lock()
	state.accounts = map[string]accountState{
		"cool": {ID: "cool", Label: "cool@example.com", Unavailable: true, Windows: w(time.Hour)},
		"used": {ID: "used", Label: "used@example.com", Windows: w(2 * time.Hour)},
		"next": {ID: "next", Label: "next@example.com", Windows: w(3 * time.Hour)},
	}
	state.mu.Unlock()
	page := string(renderPage("", now))
	hero := page[strings.Index(page, `id="inuse-title"`):]
	if !strings.HasPrefix(hero[strings.Index(hero, ">")+1:], "used@example.com") {
		t.Fatalf("the hero must show the account the proxy can use, got %.80s", hero)
	}
	for _, want := range []string{`>In use</span>`, `>Next</span>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	// The label must be on the cooling-down account's own row.
	table := page[strings.Index(page, "<tbody"):]
	row := table[strings.Index(table, "cool@example.com"):]
	row = row[:strings.Index(row, "</tr>")]
	if !strings.Contains(row, ">Paused<") {
		t.Fatal("the cooling-down account's row lacks its label")
	}
}

func TestPaceTextIsRelative(t *testing.T) {
	now := time.Now()
	p := pace{Known: true, PerHour: 10, RunsOutAt: now.Add(90 * time.Minute)}
	head, _ := paceSummary(p, now)
	if !strings.HasPrefix(head, "Runs out in ") {
		t.Fatalf("pace text %q is not relative", head)
	}
}

func TestLateServedRecordClearsProblem(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{"a": {ID: "a", Label: "a@example.com",
		HeadersAt: now, Problem: problemUnauthorized, FailCount: 2, LoginExpired: true}}
	state.mu.Unlock()
	// A record whose reading is older than HeadersAt still proves the login works.
	applyHeaderWindows("a", map[string]usageWindow{"seven_day": {Utilization: 5, At: now.Add(-time.Minute)}}, now.Add(-time.Minute))
	state.mu.RLock()
	acc := state.accounts["a"]
	state.mu.RUnlock()
	if acc.LoginExpired || acc.ServedAt.IsZero() || !acc.HeadersAt.Equal(now) {
		t.Fatalf("late record: %+v", acc)
	}
}

func TestMergeCheckedTrustsTheCheck(t *testing.T) {
	start := time.Now()
	prev := map[string]usageWindow{
		"five_hour":      {Utilization: 40, ResetsAt: start.Add(time.Hour), At: start.Add(-time.Hour)},
		"seven_day":      {Utilization: 90, ResetsAt: start.Add(time.Hour), At: start.Add(-time.Hour)},
		"seven_day_opus": {Utilization: 5, At: start.Add(-time.Hour)},
		"served":         {Utilization: 1, At: start.Add(time.Second)}, // a request during the check
	}
	// After an early reset the check reports the weekly window with no reset time yet.
	checked := map[string]usageWindow{"seven_day": {Utilization: 0, At: start.Add(2 * time.Second)}, "five_hour": {Utilization: 2, At: start.Add(2 * time.Second)}}
	got := mergeChecked(prev, checked, start)
	if _, ok := got["seven_day_opus"]; ok {
		t.Fatal("a window the check no longer reports must go")
	}
	if w := got["seven_day"]; w.Utilization != 0 || !w.ResetsAt.IsZero() {
		t.Fatalf("the old reset time must not survive the check: %+v", w)
	}
	if got["served"].Utilization != 1 || got["five_hour"].Utilization != 2 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := prev["seven_day_opus"]; !ok {
		t.Fatal("the input map was changed")
	}
}

func TestKeepLiveUpdatesDuringRefresh(t *testing.T) {
	start := time.Now()
	served := start.Add(time.Second)
	// As refreshAll builds them: a's check succeeded (fresh readings), b's failed (old ones kept).
	next := map[string]accountState{
		"a": {ID: "a", FetchedAt: start.Add(2 * time.Second), Problem: problemOther, FailCount: 1, LoginExpired: true,
			Windows: map[string]usageWindow{"seven_day": {Utilization: 10, At: start.Add(2 * time.Second)}}},
		"b": {ID: "b", Problem: problemOther, FailCount: 1, FetchedAt: start.Add(-time.Hour),
			Windows: map[string]usageWindow{"seven_day": {Utilization: 30, At: start.Add(-time.Hour)}}},
	}
	current := map[string]accountState{
		"a": {ID: "a", ServedAt: served, HeadersAt: served, Windows: map[string]usageWindow{
			"seven_day":      {Utilization: 9, At: served},
			"seven_day_opus": {Utilization: 5, At: start.Add(-time.Hour)},
		}},
		"b": {ID: "b"},
	}
	keepLiveUpdates(next, current, start)
	a := next["a"]
	if a.LoginExpired || a.Problem != problemNone || !a.ServedAt.Equal(served) {
		t.Fatalf("a served request must clear the problem: %+v", a)
	}
	if _, ok := a.Windows["seven_day_opus"]; ok || a.Windows["seven_day"].Utilization != 10 {
		t.Fatalf("windows %+v", a.Windows)
	}
	if next["b"].Problem != problemOther {
		t.Fatal("an account with no served request keeps its problem")
	}
}

func TestHistoryKeepsResetTimesWhenMergingABucket(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	at := time.Now().Truncate(historyBucket).Add(time.Minute)
	reset := at.Add(48 * time.Hour)
	acc := accountState{ID: "a", Label: "a@example.com", Windows: map[string]usageWindow{
		"seven_day": {Utilization: 10, ResetsAt: reset}, "five_hour": {Utilization: 3, ResetsAt: at.Add(time.Hour)}}}
	history.add(acc, at)
	acc.Windows = map[string]usageWindow{"seven_day": {Utilization: 11, ResetsAt: reset}}
	history.add(acc, at.Add(time.Minute))
	history.mu.Lock()
	defer history.mu.Unlock()
	list := history.accounts[historyKey(acc)]
	if len(list) != 1 || list[0].R["five_hour"] != at.Add(time.Hour).Unix() || list[0].U["seven_day"] != 11 {
		t.Fatalf("merged bucket %+v", list)
	}
}

func TestRetryLimitCountsEveryFailureKind(t *testing.T) {
	// Failures that alternate between kinds still use up the quick retries.
	a := accountState{Problem: problemOther, FailCount: 1, FailStreak: retryChecks + 1}
	if a.retrySoon() {
		t.Fatal("alternating failures must not retry every two minutes forever")
	}
	a.clearProblem()
	if a.FailStreak != 0 {
		t.Fatal("a success resets the streak")
	}
}

func TestRateLimitedRequestShowsCoolingDownAtOnce(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{
		"a": {ID: "a", Label: "a@example.com", Problem: problemUnauthorized, FailCount: 1},
		"b": {ID: "b", Label: "b@example.com"},
	}
	state.mu.Unlock()
	raw, _ := json.Marshal(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: now, Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}})
	handleUsage(raw)
	state.mu.RLock()
	a := state.accounts["a"]
	state.mu.RUnlock()
	if !a.Unavailable {
		t.Fatal("a rate-limited account shows as cooling down at once")
	}
	if a.Problem != problemUnauthorized {
		t.Fatal("a failed request must not clear a failed check")
	}
	if servedCount("a") != 0 {
		t.Fatal("a failed request is not counted as sent")
	}
	// The proxy offers the account again once the cooldown is over.
	markOffered([]pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}})
	state.mu.RLock()
	a = state.accounts["a"]
	state.mu.RUnlock()
	if a.Unavailable {
		t.Fatal("an offered account is no longer cooling down")
	}
}

func TestCooldownIsClearedOnlyAfterAFullLimitReset(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	var calls []string
	fail := false
	orig := resetCooldown
	resetCooldown = func(authIndex string) error {
		calls = append(calls, authIndex)
		if fail {
			return errors.New("host busy")
		}
		return nil
	}
	t.Cleanup(func() { resetCooldown = orig })
	now := time.Now()
	start := now.Add(-time.Second)
	room := accountState{ID: "a", AuthIndex: "1", Unavailable: true, FetchedAt: now, Windows: map[string]usageWindow{
		allModelsWindow: {Utilization: 41, ResetsAt: now.Add(48 * time.Hour)}, fiveHourWindow: {Utilization: 9, ResetsAt: now.Add(time.Hour)}}}

	// A cooldown the readings never explained (they showed room all along) is left alone.
	state.clearStaleCooldown(room, false, start, now)
	if len(calls) != 0 {
		t.Fatal("an unexplained cooldown must be left to the proxy")
	}
	// A failed reset is not throttled, so the next check tries again.
	fail = true
	state.clearStaleCooldown(room, true, start, now)
	fail = false
	state.clearStaleCooldown(room, true, start, now)
	if len(calls) != 2 {
		t.Fatalf("calls = %v; a failed reset must be retried", calls)
	}
	// After a full limit reset early it is cleared, then not again within cooldownClearEvery.
	state.clearStaleCooldown(room, true, start, now.Add(time.Minute))
	if len(calls) != 2 {
		t.Fatalf("calls = %v; cleared again too soon", calls)
	}
}

func TestRateLimitHeadersMarkTheLimitFull(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{"a": {ID: "a", Label: "a@example.com"}}
	state.mu.Unlock()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.97")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.2")
	raw, _ := json.Marshal(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: now, Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: h})
	handleUsage(raw)
	acc := snapshotAccounts()["a"]
	if !acc.Unavailable || !acc.exhausted(now) || acc.Windows[fiveHourWindow].Utilization != 20 {
		t.Fatalf("a refusal on the weekly limit marks it full and cooling down: %+v", acc)
	}
}

func TestRefreshKeepsCooldownSeenDuringIt(t *testing.T) {
	start := time.Now()
	// As refreshAll builds them: availability from the credential list read at the start.
	next := map[string]accountState{"a": {ID: "a", AvailabilityAt: start}, "b": {ID: "b", Unavailable: true, AvailabilityAt: start}}
	current := map[string]accountState{
		"a": {ID: "a", Unavailable: true, AvailabilityAt: start.Add(time.Second), CooldownModel: "m"}, // refused during the refresh
		"b": {ID: "b", AvailabilityAt: start.Add(time.Second)},                                        // offered during the refresh
	}
	keepLiveUpdates(next, current, start)
	if !next["a"].Unavailable || next["a"].CooldownModel != "m" || next["b"].Unavailable {
		t.Fatalf("updates seen during the refresh were lost: %+v", next)
	}
}

func TestRenewingKeepsQuickRetriesAfterOtherFailures(t *testing.T) {
	a := accountState{Problem: problemRenewing, FailCount: 3, FailStreak: retryChecks + 2}
	if !a.retrySoon() {
		t.Fatal("a token being renewed is retried soon until it counts as expired")
	}
	a.FailCount = renewingChecks + 1
	if a.retrySoon() {
		t.Fatal("the quick retries stop once renewing has run its course")
	}
}

func TestLateRecordIsKeptWhenTheCheckFailed(t *testing.T) {
	start := time.Now()
	old := start.Add(-time.Hour)
	// The check failed, so next carries the old readings; a record from just before the
	// refresh started arrived while it ran.
	next := map[string]accountState{"a": {ID: "a", FetchedAt: old, HeadersAt: old, Problem: problemOther, FailCount: 1,
		Windows: map[string]usageWindow{"seven_day": {Utilization: 30, At: old}}}}
	current := map[string]accountState{"a": {ID: "a", FetchedAt: old, HeadersAt: start.Add(-time.Second), ServedAt: start.Add(5 * time.Second),
		Windows: map[string]usageWindow{"seven_day": {Utilization: 35, At: start.Add(-time.Second)}}}}
	keepLiveUpdates(next, current, start)
	if got := next["a"].Windows["seven_day"].Utilization; got != 35 {
		t.Fatalf("utilization = %v; the newer reading must win when the check failed", got)
	}
}

func TestLateRefusalDoesNotUndoANewerPick(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	now := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{"a": {ID: "a", AvailabilityAt: now}} // offered by a pick just now
	state.mu.Unlock()
	markRateLimited("a", "m", nil, now.Add(-10*time.Second)) // the refusal happened before that pick
	if snapshotAccounts()["a"].Unavailable {
		t.Fatal("a refusal older than the last pick must not mark the account cooling down")
	}
}

func TestStaleResetIsNotBorrowed(t *testing.T) {
	now := time.Now()
	old := map[string]usageWindow{"five_hour": {Utilization: 50, ResetsAt: now.Add(-time.Minute), At: now.Add(-2 * time.Hour)}}
	fresh := map[string]usageWindow{"five_hour": {Utilization: 100, At: now}}
	if got := mergeWindows(old, fresh)["five_hour"]; !got.ResetsAt.IsZero() {
		t.Fatalf("a reset that had already passed was borrowed: %+v", got)
	}
}

func TestRefusalBeforeAPickArrivingAfterItIsIgnored(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	refused := time.Now()
	state.mu.Lock()
	state.accounts = map[string]accountState{"a": {ID: "a"}}
	state.mu.Unlock()
	// The cooldown ended and a pick offered the account before the refusal's record came in.
	markOffered([]pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude"}})
	markRateLimited("a", "m", nil, refused)
	if snapshotAccounts()["a"].Unavailable {
		t.Fatal("a refusal from before the last pick must not pause the account")
	}
}
