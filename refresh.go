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
	usageURL          = "https://api.anthropic.com/api/oauth/usage"
	manualCooldown    = time.Minute
	minWake           = time.Minute
	postResetGrace    = 2 * time.Minute
	httpTimeout       = 20 * time.Second
	usageUserAgent    = "claude-cli/2.1.280 (external, cli)"
	usageAnthropicBet = "oauth-2025-04-20"
)

// usageWindow is one quota window from Anthropic's OAuth usage endpoint.
type usageWindow struct {
	Utilization float64
	ResetsAt    time.Time
}

// accountState is the cached quota snapshot for one Claude credential.
type accountState struct {
	ID        string
	Label     string
	FileName  string
	Priority  int
	Disabled  bool
	Status    string
	FetchedAt time.Time
	Error     string
	Windows   map[string]usageWindow
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
		hostLog("warn", "reset-scheduler: listing credentials failed", map[string]any{"error": err.Error()})
		return
	}
	weekly := loadedConfig().window()
	next := make(map[string]accountState, len(entries))
	for _, e := range entries {
		acc := accountState{
			ID:       e.ID,
			Label:    firstNonEmpty(e.Email, e.Label, e.Name),
			FileName: e.Name,
			Priority: e.Priority,
			Disabled: e.Disabled,
			Status:   e.Status,
		}
		if e.Disabled {
			next[e.ID] = acc
			continue
		}
		windows, errFetch := fetchUsage(e.AuthIndex)
		acc.FetchedAt = time.Now()
		if errFetch != nil {
			acc.Error = errFetch.Error()
			// Keep the previous snapshot so one failed fetch does not reshuffle routing.
			r.mu.RLock()
			if prev, ok := r.accounts[e.ID]; ok && prev.Error == "" {
				acc.Windows = prev.Windows
				acc.Error = ""
				acc.FetchedAt = prev.FetchedAt
				acc.Status = firstNonEmpty(e.Status, prev.Status)
			}
			r.mu.RUnlock()
			if acc.Error != "" {
				hostLog("warn", "reset-scheduler: usage fetch failed", map[string]any{"account": acc.Label, "error": errFetch.Error()})
			}
		} else {
			acc.Windows = windows
		}
		next[e.ID] = acc
	}
	r.mu.Lock()
	r.accounts = next
	r.lastErr = ""
	r.mu.Unlock()
	hostLog("info", "reset-scheduler: refreshed weekly reset times", map[string]any{"accounts": len(next), "window": weekly})
}

type claudeAuth struct {
	ID        string
	AuthIndex string
	Name      string
	Label     string
	Email     string
	Status    string
	Disabled  bool
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
			Disabled:  entry.Disabled,
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
	return parseUsage(body)
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
