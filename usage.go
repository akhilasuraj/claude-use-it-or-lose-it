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
	{fiveHourWindow, "5h"},
	{allModelsWindow, "7d"},
}

// handleUsage receives one usage record from the host. It must stay cheap: the host delivers
// records from a queue and waits for each call.
func handleUsage(raw []byte) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		hostLog("warn", pluginName+": skipping an unreadable usage record", map[string]any{"error": err.Error()})
		return
	}
	if !isClaude(rec.Provider) || rec.AuthID == "" {
		return
	}
	at := rec.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	if rec.Latency > 0 {
		at = at.Add(rec.Latency)
	}
	if rec.Failed {
		// Not counted as sent, and it proves nothing about the login. A rate limit makes the
		// proxy hold the account back, so show that now rather than at the next check, and
		// keep its headers: they tell whether a limit was full.
		if rec.Failure.StatusCode == http.StatusTooManyRequests {
			markRateLimited(rec.AuthID, rec.Model, windowsFromHeaders(rec.ResponseHeaders, at), at)
		}
		return
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
		rejected := strings.EqualFold(strings.TrimSpace(headerValue(h, "Anthropic-Ratelimit-Unified-"+hw.prefix+"-Status")), "rejected")
		util, errUtil := strconv.ParseFloat(rawUtil, 64)
		if errUtil != nil || math.IsNaN(util) || math.IsInf(util, 0) || util < 0 {
			if !rejected {
				continue
			}
			util = 1
		}
		w := usageWindow{Utilization: math.Round(util*1000) / 10, At: at}
		if rejected {
			// Anthropic refused the request on this limit, so it is full whatever the figure says.
			w.Utilization = math.Max(w.Utilization, 100)
		}
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
	acc.clearProblem()
	if at.After(acc.HeadersAt) {
		acc.HeadersAt = at
	}
	acc.ServedAt = time.Now()
	state.accounts[authID] = acc
	state.mu.Unlock()
	history.add(acc, at)
}

// markRateLimited records a request Anthropic refused with a rate limit: the proxy now holds
// the account back, and the response headers still carry its usage. Unlike a served request
// it does not clear a failed check. The proxy may hold the account back for that model only;
// the next pick for another model offers it again, and markOffered then clears the mark.
func markRateLimited(authID, model string, windows map[string]usageWindow, at time.Time) {
	state.mu.Lock()
	acc, ok := state.accounts[authID]
	if !ok {
		state.mu.Unlock()
		return
	}
	if len(windows) > 0 {
		acc.Windows = mergeWindows(acc.Windows, windows)
		if at.After(acc.HeadersAt) {
			acc.HeadersAt = at
		}
	}
	// Records come through a queue, so a refusal can arrive after the cooldown it caused has
	// ended and a pick has offered the account again. Order by when it happened.
	if at.After(acc.AvailabilityAt) {
		acc.Unavailable, acc.RetryAfter, acc.AvailabilityAt, acc.CooldownModel = true, time.Time{}, at, model
	}
	state.accounts[authID] = acc
	state.mu.Unlock()
	if len(windows) > 0 {
		history.add(acc, at)
	}
}

// isClaude reports whether a provider or type name from the host means Claude.
func isClaude(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), claudeProvider)
}

// limitWindows are the limits that decide whether an account can take requests.
var limitWindows = []string{allModelsWindow, fiveHourWindow}

// mergeChecked combines the readings of a usage check (checked) with what was known before
// (prev). The check is the authority: a window it no longer reports is dropped, and its reset
// time is taken as given, even when empty. Only a reading taken by a served request after
// since (the start of the check) wins over it. It never mutates its inputs.
func mergeChecked(prev, checked map[string]usageWindow, since time.Time) map[string]usageWindow {
	out := make(map[string]usageWindow, len(checked))
	for k, v := range checked {
		out[k] = v
	}
	for k, v := range prev {
		if v.At.Before(since) {
			continue
		}
		if c, ok := out[k]; ok && !v.At.After(c.At) {
			continue
		}
		out[k] = v
	}
	return out
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
			// Some responses omit the reset time; keep the one we already know, if it had not
			// passed yet when this reading was taken (a passed one belongs to an older cycle).
			if prev, ok := out[k]; ok && prev.ResetsAt.After(v.At) {
				v.ResetsAt = prev.ResetsAt
			}
		}
		out[k] = v
	}
	return out
}
