package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	usageURL           = "https://api.anthropic.com/api/oauth/usage"
	manualCooldown     = time.Minute
	minWake            = time.Minute
	postResetGrace     = 2 * time.Minute
	httpTimeout        = 20 * time.Second
	cooldownClearEvery = 30 * time.Minute
	usageUserAgent     = "claude-cli/2.1.280 (external, cli)"
	usageAnthropicBet  = "oauth-2025-04-20"
)

// usageWindow is one quota window from Anthropic's OAuth usage endpoint.
type usageWindow struct {
	Utilization float64
	ResetsAt    time.Time
	At          time.Time // when this reading was taken
}

// accountState is the cached quota snapshot for one Claude credential.
type accountState struct {
	ID            string
	AuthIndex     string
	Label         string
	FileName      string
	Priority      int
	Disabled      bool
	Unavailable   bool
	Status        string
	StatusMessage string
	FetchedAt     time.Time // last successful usage check
	HeadersAt     time.Time // last update from response headers
	Error         string
	LoginExpired  bool
	Windows       map[string]usageWindow
}

func (a accountState) exhausted(weekly string, now time.Time) bool {
	for _, key := range []string{weekly, "five_hour"} {
		if w, ok := a.Windows[key]; ok && w.Utilization >= 100 && w.ResetsAt.After(now) {
			return true
		}
	}
	return false
}

type refresher struct {
	mu          sync.RWMutex
	cleared     map[string]time.Time // last cooldown reset per account
	accounts    map[string]accountState
	lastRun     time.Time
	lastErr     string
	nextRun     time.Time
	running     bool
	trigger     chan struct{}
	stop        chan struct{}
	startedOnce sync.Once
	stopOnce    sync.Once
}

var state = &refresher{
	accounts: map[string]accountState{},
	cleared:  map[string]time.Time{},
	trigger:  make(chan struct{}, 1),
	stop:     make(chan struct{}),
}

func snapshotAccounts() map[string]accountState {
	state.mu.RLock()
	defer state.mu.RUnlock()
	out := make(map[string]accountState, len(state.accounts))
	for k, v := range state.accounts {
		out[k] = v
	}
	return out
}

func startRefresher() {
	state.startedOnce.Do(func() { go state.loop() })
}

func stopRefresher() {
	state.stopOnce.Do(func() { close(state.stop) })
}

// requestRefresh asks the background loop to refresh. Manual requests are ignored when the
// last refresh finished less than manualCooldown ago. Returns whether a refresh was queued.
func requestRefresh(force bool) bool {
	state.mu.RLock()
	recent := !state.lastRun.IsZero() && time.Since(state.lastRun) < manualCooldown
	running := state.running
	state.mu.RUnlock()
	if !force && (recent || running) {
		return false
	}
	select {
	case state.trigger <- struct{}{}:
	default:
	}
	return true
}

func (r *refresher) loop() {
	// Small delay so the host finishes loading credentials before the first fetch.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-timer.C:
		case <-r.trigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		r.refreshAll()
		wait := r.nextWait(time.Now())
		r.mu.Lock()
		r.nextRun = time.Now().Add(wait)
		r.mu.Unlock()
		timer.Reset(wait)
	}
}

// nextWait is the configured interval, shortened so that a refresh happens shortly after
// any cached reset passes (the order changes at that moment).
func (r *refresher) nextWait(now time.Time) time.Duration {
	wait := loadedConfig().interval()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, acc := range r.accounts {
		for _, w := range acc.Windows {
			if w.ResetsAt.After(now) {
				if d := w.ResetsAt.Sub(now) + postResetGrace; d < wait {
					wait = d
				}
			}
		}
	}
	if wait < minWake {
		wait = minWake
	}
	return wait
}

func (r *refresher) refreshAll() {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.lastRun = time.Now()
		r.mu.Unlock()
	}()

	entries, err := listClaudeAuths()
	if err != nil {
		r.mu.Lock()
		r.lastErr = err.Error()
		r.mu.Unlock()
		hostLog("warn", pluginName+": listing credentials failed", map[string]any{"error": err.Error()})
		return
	}
	cfg := loadedConfig()
	weekly := cfg.window()
	next := make(map[string]accountState, len(entries))
	for _, e := range entries {
		acc := accountState{
			ID:            e.ID,
			AuthIndex:     e.AuthIndex,
			Label:         firstNonEmpty(e.Email, e.Label, e.Name),
			FileName:      e.Name,
			Priority:      e.Priority,
			Disabled:      e.Disabled,
			Unavailable:   e.Unavail,
			Status:        e.Status,
			StatusMessage: e.StatusMsg,
		}
		r.mu.RLock()
		prev, hadPrev := r.accounts[e.ID]
		r.mu.RUnlock()
		if hadPrev {
			acc.HeadersAt = prev.HeadersAt
		}
		if e.Disabled {
			next[e.ID] = acc
			continue
		}
		windows, errFetch := fetchUsage(e.AuthIndex)
		if errFetch != nil {
			acc.Error = errFetch.Error()
			acc.LoginExpired = isLoginExpired(acc.Error, e.Status, e.StatusMsg)
			// Keep the previous readings so one failed check does not reshuffle routing.
			if hadPrev {
				acc.Windows = prev.Windows
				acc.FetchedAt = prev.FetchedAt
				if prev.Error == "" && !acc.LoginExpired {
					acc.Error = ""
				}
			}
			if acc.Error != "" {
				hostLog("warn", pluginName+": usage check failed", map[string]any{"account": acc.Label, "error": errFetch.Error()})
			}
		} else {
			acc.FetchedAt = time.Now()
			if hadPrev {
				// Header readings taken while the check was in flight may be newer.
				windows = mergeWindows(prev.Windows, windows)
			}
			acc.Windows = windows
		}
		next[e.ID] = acc
	}
	r.mu.Lock()
	// Keep header updates that arrived while this refresh was running.
	for id, acc := range next {
		if cur, ok := r.accounts[id]; ok && cur.HeadersAt.After(acc.HeadersAt) {
			acc.Windows = mergeWindows(acc.Windows, cur.Windows)
			acc.HeadersAt = cur.HeadersAt
			if !acc.LoginExpired {
				acc.Error = ""
			}
			next[id] = acc
		}
	}
	r.accounts = next
	r.lastErr = ""
	r.mu.Unlock()

	now := time.Now()
	for _, acc := range next {
		if acc.Error == "" && !acc.Disabled && !acc.FetchedAt.IsZero() && now.Sub(acc.FetchedAt) < time.Minute {
			history.add(acc, acc.FetchedAt)
		}
		r.clearStaleCooldown(acc, weekly, now)
	}
	history.save()
	hostLog("info", pluginName+": checked usage", map[string]any{"accounts": len(next), "window": weekly})
}

// clearStaleCooldown lifts the proxy's cooldown on an account that a fresh usage check shows
// has room again, for example after Anthropic reset its limit early. Without this the proxy
// keeps skipping the account until its own retry time passes.
func (r *refresher) clearStaleCooldown(acc accountState, weekly string, now time.Time) {
	if !acc.Unavailable || acc.Disabled || acc.Error != "" || acc.AuthIndex == "" || acc.FetchedAt.IsZero() {
		return
	}
	if now.Sub(acc.FetchedAt) > time.Minute || !hasRoom(acc, weekly, now) {
		return
	}
	r.mu.Lock()
	recent := now.Sub(r.cleared[acc.ID]) < cooldownClearEvery
	if !recent {
		r.cleared[acc.ID] = now
	}
	r.mu.Unlock()
	if recent {
		return
	}
	if _, err := callHost(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: acc.AuthIndex}); err != nil {
		hostLog("warn", pluginName+": clearing cooldown failed", map[string]any{"account": acc.Label, "error": err.Error()})
		return
	}
	hostLog("info", pluginName+": cleared cooldown, the account has room again", map[string]any{"account": acc.Label})
}

// hasRoom reports whether the readings show the account below both its limits.
func hasRoom(acc accountState, weekly string, now time.Time) bool {
	for _, key := range []string{weekly, "five_hour"} {
		w, ok := acc.Windows[key]
		if !ok {
			return false
		}
		if w.Utilization >= 100 && w.ResetsAt.After(now) {
			return false
		}
	}
	return true
}

// isLoginExpired recognises a credential that needs a new login, as opposed to a passing error.
func isLoginExpired(fetchErr, status, statusMessage string) bool {
	if strings.Contains(fetchErr, "HTTP 401") || strings.Contains(fetchErr, "no access_token") {
		return true
	}
	if !strings.EqualFold(status, "error") {
		return false
	}
	msg := strings.ToLower(statusMessage)
	for _, hint := range []string{"invalid_grant", "refresh", "unauthorized", "401"} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

type claudeAuth struct {
	ID        string
	AuthIndex string
	Name      string
	Label     string
	Email     string
	Status    string
	StatusMsg string
	Disabled  bool
	Unavail   bool
	Priority  int
}

func listClaudeAuths() ([]claudeAuth, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	var out []claudeAuth
	for _, f := range resp.Files {
		var entry pluginapi.HostAuthFileEntry
		if err := json.Unmarshal(f, &entry); err != nil {
			continue
		}
		if !strings.EqualFold(entry.Provider, claudeProvider) && !strings.EqualFold(entry.Type, claudeProvider) {
			continue
		}
		out = append(out, claudeAuth{
			ID:        entry.ID,
			AuthIndex: entry.AuthIndex,
			Name:      entry.Name,
			Label:     entry.Label,
			Email:     entry.Email,
			Status:    entry.Status,
			StatusMsg: entry.StatusMessage,
			Disabled:  entry.Disabled,
			Unavail:   entry.Unavailable,
			Priority:  entry.Priority,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func fetchUsage(authIndex string) (map[string]usageWindow, error) {
	raw, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, fmt.Errorf("read credential: %w", err)
	}
	var got pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(got.JSON, &cred); err != nil || strings.TrimSpace(cred.AccessToken) == "" {
		return nil, fmt.Errorf("credential has no access_token")
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cred.AccessToken))
	req.Header.Set("anthropic-beta", usageAnthropicBet)
	req.Header.Set("User-Agent", usageUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}
	windows, err := parseUsage(body)
	if err != nil {
		return nil, err
	}
	at := time.Now()
	for k, w := range windows {
		w.At = at
		windows[k] = w
	}
	return windows, nil
}

func parseUsage(body []byte) (map[string]usageWindow, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	out := map[string]usageWindow{}
	for key, value := range payload {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if err := json.Unmarshal(value, &w); err != nil || (w.Utilization == nil && w.ResetsAt == nil) {
			continue
		}
		uw := usageWindow{}
		if w.Utilization != nil {
			uw.Utilization = *w.Utilization
		}
		if w.ResetsAt != nil && strings.TrimSpace(*w.ResetsAt) != "" {
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*w.ResetsAt)); err == nil {
				uw.ResetsAt = t
			}
		}
		out[key] = uw
	}
	return out, nil
}

func hostLog(level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{"level": level, "message": message, "fields": fields})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
