package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every Claude response carries Anthropic's rate-limit headers, so each request served by an
// account also tells us that account's current usage, for free. These updates keep the order
// current between the timed usage checks.
var headerWindows = []struct {
	window string // usage API window key
	prefix string // Anthropic-Ratelimit-Unified-<prefix>-Utilization / -Reset
}{
	{"five_hour", "5h"},
	{"seven_day", "7d"},
}

// handleUsage receives one usage record from the host. It must stay cheap: the host delivers
// records from a queue and waits for each call.
func handleUsage(raw []byte) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(rec.Provider), claudeProvider) || rec.AuthID == "" {
		return
	}
	at := rec.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	if rec.Latency > 0 {
		at = at.Add(rec.Latency)
	}
	stats.recordServed(rec.AuthID)
	windows := windowsFromHeaders(rec.ResponseHeaders, at)
	if len(windows) == 0 {
		return
	}
	applyHeaderWindows(rec.AuthID, windows, at)
}

// windowsFromHeaders reads the unified 5-hour and 7-day windows from Anthropic response
// headers. Utilization is sent as a fraction (0.42) and reset times as Unix seconds.
func windowsFromHeaders(h http.Header, at time.Time) map[string]usageWindow {
	out := map[string]usageWindow{}
	for _, hw := range headerWindows {
		rawUtil := headerValue(h, "Anthropic-Ratelimit-Unified-"+hw.prefix+"-Utilization")
		rawReset := headerValue(h, "Anthropic-Ratelimit-Unified-"+hw.prefix+"-Reset")
		util, errUtil := strconv.ParseFloat(rawUtil, 64)
		if errUtil != nil || math.IsNaN(util) || math.IsInf(util, 0) || util < 0 {
			continue
		}
		w := usageWindow{Utilization: math.Round(util*1000) / 10, At: at}
		if reset, ok := parseResetHeader(rawReset); ok {
			w.ResetsAt = reset
		}
		out[hw.window] = w
	}
	return out
}

func headerValue(h http.Header, name string) string {
	for key, values := range h {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func parseResetHeader(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// applyHeaderWindows merges header-derived windows into the cached account and records a
// history sample. Accounts the timed check has not listed yet are ignored.
func applyHeaderWindows(authID string, windows map[string]usageWindow, at time.Time) {
	state.mu.Lock()
	acc, ok := state.accounts[authID]
	if !ok {
		state.mu.Unlock()
		return
	}
	acc.Windows = mergeWindows(acc.Windows, windows)
	// A served request proves the credential works, so a stale fetch error no longer applies.
	acc.Error = ""
	if at.After(acc.HeadersAt) {
		acc.HeadersAt = at
	}
	state.accounts[authID] = acc
	state.mu.Unlock()
	history.add(acc, at)
}

// mergeWindows returns a new map holding, for each window, whichever reading is newer.
// It never mutates its inputs, because snapshots share the window maps.
func mergeWindows(old, fresh map[string]usageWindow) map[string]usageWindow {
	out := make(map[string]usageWindow, len(old)+len(fresh))
	for k, v := range old {
		out[k] = v
	}
	for k, v := range fresh {
		if prev, ok := out[k]; ok && prev.At.After(v.At) {
			continue
		}
		if v.ResetsAt.IsZero() {
			// Some responses omit the reset time; keep the one we already know.
			if prev, ok := out[k]; ok {
				v.ResetsAt = prev.ResetsAt
			}
		}
		out[k] = v
	}
	return out
}
