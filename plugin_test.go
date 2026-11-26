package main

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func cand(id string, prio int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Priority: prio}
}

func TestChooseSoonestWeeklyReset(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	accounts := map[string]accountState{
		"a": {Windows: map[string]usageWindow{"seven_day": {Utilization: 10, ResetsAt: now.Add(5 * 24 * time.Hour)}}},
		"b": {Windows: map[string]usageWindow{"seven_day": {Utilization: 50, ResetsAt: now.Add(2 * time.Hour)}}},
		"c": {Windows: map[string]usageWindow{"seven_day": {Utilization: 0, ResetsAt: now.Add(3 * 24 * time.Hour)}}},
	}
	// b has the lowest priority but resets soonest: it must still win.
	id := choose([]pluginapi.SchedulerAuthCandidate{cand("a", 10), cand("b", -27), cand("c", 0)}, accounts, pluginConfig{}, now)
	if id != "b" {
		t.Fatalf("got %q, want b", id)
	}
}

func TestRankPutsFullAndExpiredLast(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	accounts := map[string]accountState{
		"full":  {Windows: map[string]usageWindow{"seven_day": {Utilization: 100, ResetsAt: now.Add(time.Hour)}}},
		"five":  {Windows: map[string]usageWindow{"seven_day": {Utilization: 20, ResetsAt: now.Add(2 * time.Hour)}, "five_hour": {Utilization: 100, ResetsAt: now.Add(time.Hour)}}},
		"later": {Windows: map[string]usageWindow{"seven_day": {Utilization: 20, ResetsAt: now.Add(4 * 24 * time.Hour)}}},
		// An expired login makes its readings meaningless, even a reset that comes first.
		"err": {LoginExpired: true, Windows: map[string]usageWindow{"seven_day": {Utilization: 1, ResetsAt: now.Add(time.Minute)}}},
	}
	cands := []pluginapi.SchedulerAuthCandidate{cand("full", 0), cand("five", 0), cand("later", 0), cand("err", 99)}
	ranked := rankCandidates(cands, accounts, pluginConfig{}, now)
	got := []string{}
	for _, r := range ranked {
		got = append(got, r.id)
	}
	// An expired login goes last, after the full accounts, since it cannot serve requests.
	want := []string{"later", "full", "five", "err"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	off := false
	id := choose(cands, accounts, pluginConfig{DeprioritizeExhausted: &off}, now)
	if id != "full" {
		t.Fatalf("with deprioritize off got %q, want full", id)
	}
}

func TestChooseFallsBack(t *testing.T) {
	now := time.Now()
	if id := choose([]pluginapi.SchedulerAuthCandidate{cand("a", 0)}, map[string]accountState{}, pluginConfig{}, now); id != "" {
		t.Fatalf("no data: got %q, want fallback", id)
	}
	mixed := []pluginapi.SchedulerAuthCandidate{cand("a", 0), {ID: "x", Provider: "codex"}}
	accounts := map[string]accountState{"a": {Windows: map[string]usageWindow{"seven_day": {ResetsAt: now.Add(time.Hour)}}}}
	if id := choose(mixed, accounts, pluginConfig{}, now); id != "" {
		t.Fatalf("mixed providers: got %q, want fallback", id)
	}
	stale := map[string]accountState{"a": {Windows: map[string]usageWindow{"seven_day": {ResetsAt: now.Add(-time.Minute)}}}}
	if id := choose([]pluginapi.SchedulerAuthCandidate{cand("a", 0)}, stale, pluginConfig{}, now); id != "" {
		t.Fatalf("past reset: got %q, want fallback", id)
	}
}

func TestParseUsage(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-11-23T15:00:00.123456+00:00"},"seven_day":{"utilization":40,"resets_at":"2026-11-27T09:00:00+00:00"},"seven_day_opus":null,"extra_usage":{"is_enabled":false}}`)
	w, err := parseUsage(body)
	if err != nil {
		t.Fatal(err)
	}
	if w["seven_day"].Utilization != 40 || w["seven_day"].ResetsAt.Day() != 27 {
		t.Fatalf("seven_day = %+v", w["seven_day"])
	}
	if w["five_hour"].ResetsAt.IsZero() {
		t.Fatalf("five_hour reset not parsed")
	}
	if _, ok := w["seven_day_opus"]; ok {
		t.Fatalf("null window should be skipped")
	}
	if _, ok := w["extra_usage"]; ok {
		t.Fatalf("non-window object should be skipped")
	}
}

func TestPageRenders(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })
	page := string(renderPage("hi", time.Now()))
	for _, want := range []string{`<p class="notice">hi</p>`, "No enabled Claude accounts found yet."} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
}

func TestThemeBlocksPresent(t *testing.T) {
	for _, want := range []string{
		`:root:not([data-theme])`,
		`:root[data-theme="dark"]{color-scheme:dark;`,
		`:root[data-theme="light"]{color-scheme:light}`,
		`:root[data-theme="white"]{color-scheme:light;`,
	} {
		if !strings.Contains(pageStyles, want) {
			t.Errorf("pageStyles missing %q", want)
		}
	}
	if strings.Contains(pageStyles, "/*DARK*/") {
		t.Error("dark placeholder was not replaced")
	}
}

// The host HTML-escapes plugin metadata and the panel renders it as text, so characters like
// an apostrophe would show up as "&#39;".
func TestConfigFieldDescriptionsArePlain(t *testing.T) {
	meta := registration()["metadata"].(pluginapi.Metadata)
	for _, f := range meta.ConfigFields {
		if strings.ContainsAny(f.Description, `'"&<>`) {
			t.Errorf("%s description contains a character the host will escape: %q", f.Name, f.Description)
		}
	}
	if strings.ContainsAny(meta.Logo, `'"&<>`) {
		t.Error("logo contains a character the host will escape")
	}
}

func TestParseConfigLabelledAndLegacyKeys(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\n\"Check usage every (minutes)\": 45\n\"Weekly limit to order by\": Opus\n\"Move full accounts to the bottom\": false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.interval() != 45*time.Minute || cfg.deprioritizeExhausted() {
		t.Fatalf("labelled keys parsed as %+v", cfg)
	}
	legacy, err := parseConfig([]byte("refresh_interval_minutes: 10\nweekly_window: seven_day_sonnet\ndeprioritize_exhausted: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.interval() != 10*time.Minute || legacy.deprioritizeExhausted() {
		t.Fatalf("legacy keys parsed as %+v", legacy)
	}
	empty, _ := parseConfig([]byte("enabled: true\n"))
	if empty.interval() != defaultInterval*time.Minute || !empty.deprioritizeExhausted() {
		t.Fatalf("defaults parsed as %+v", empty)
	}
}

func TestConfigFieldNamesAreReadable(t *testing.T) {
	meta := registration()["metadata"].(pluginapi.Metadata)
	for _, f := range meta.ConfigFields {
		if strings.Contains(f.Name, "_") {
			t.Errorf("field %q shows as a raw key", f.Name)
		}
	}
}

func TestNoDataRanksAheadOfFull(t *testing.T) {
	now := time.Now()
	accounts := map[string]accountState{
		"full": {Windows: map[string]usageWindow{"seven_day": {Utilization: 100, ResetsAt: now.Add(time.Hour)}}},
		"new":  {},
	}
	ranked := rankCandidates([]pluginapi.SchedulerAuthCandidate{cand("full", 0), cand("new", 0)}, accounts, pluginConfig{}, now)
	if ranked[0].id != "new" {
		t.Fatalf("an account without data must come before a full one, got %v", ranked)
	}
	// With no data first, the plugin hands the choice to the normal routing.
	if id := choose([]pluginapi.SchedulerAuthCandidate{cand("full", 0), cand("new", 0)}, accounts, pluginConfig{}, now); id != "" {
		t.Fatalf("choose = %q; want the normal routing", id)
	}
}
