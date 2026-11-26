package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	usageAnthropicBeta = "oauth-2025-04-20"
	retryAfterError    = 2 * time.Minute  // a failed check is retried this soon...
	retryChecks        = 8                // ...this many times in a row, then at the normal interval
	tokenRenewSlack    = 30 * time.Second // a token this close to expiry is left for the proxy to renew
	// A login is reported expired only after this many failed checks of the same kind in a row,
	// so a token the proxy is about to renew (for example right after the computer wakes up) is
	// not flagged.
	unauthorizedChecks = 2
	renewingChecks     = retryChecks // about 15 minutes of retries
	shownFailChecks    = 2           // a passing failure is shown only once it repeats
)

// Window keys used by Anthropic's usage endpoint.
const (
	// allModelsWindow is the weekly limit that applies across all models. Plans no longer have
	// separate Opus or Sonnet weekly limits, so it is the one weekly limit accounts are ordered by.
	allModelsWindow = "seven_day"
	fiveHourWindow  = "five_hour"
)

var (
	// errTokenRenewing means the stored access token has expired and the proxy has not renewed
	// it yet. The proxy renews tokens itself; the plugin waits instead of using a dead token.
	errTokenRenewing = errors.New("waiting for the proxy to renew the login")
	errNoAccessToken = errors.New("credential has no access_token")
)

// httpStatusError is a non-200 answer from the usage endpoint.
type httpStatusError struct{ code int }

func (e httpStatusError) Error() string {
	return fmt.Sprintf("usage endpoint returned HTTP %d", e.code)
}

// checkProblem classifies why the last usage check of an account failed.
type checkProblem int

const (
	problemNone         checkProblem = iota
	problemOther                     // network, server or decode error; worth retrying
	problemUnauthorized              // the usage endpoint rejected the token (HTTP 401)
	problemRenewing                  // the stored token expired; the proxy has not renewed it yet
	problemNoToken                   // the credential holds no access token at all
)

func classifyProblem(err error) checkProblem {
	var status httpStatusError
	switch {
	case err == nil:
		return problemNone
	case errors.Is(err, errNoAccessToken):
		return problemNoToken
	case errors.Is(err, errTokenRenewing):
		return problemRenewing
	case errors.As(err, &status) && status.code == http.StatusUnauthorized:
		return problemUnauthorized
	default:
		return problemOther
	}
}

// usageWindow is one quota window from Anthropic's OAuth usage endpoint.
type usageWindow struct {
	Utilization float64
	ResetsAt    time.Time
	At          time.Time // when this reading was taken
}

// accountState is the cached quota snapshot for one Claude credential.
type accountState struct {
	ID             string
	AuthIndex      string
	Label          string
	FileName       string
	Priority       int
	Disabled       bool
	Unavailable    bool      // the proxy is holding it back (cooldown); it is not offered for picks
	RetryAfter     time.Time // when the proxy's cooldown ends, if known
	AvailabilityAt time.Time // when Unavailable was last set: by a refresh, a pick or a refused request (wall clock)
	CooldownModel  string    // the model a refused request was for, when that set Unavailable
	Status         string
	StatusMessage  string
	FetchedAt      time.Time    // last successful usage check
	HeadersAt      time.Time    // newest reading taken from response headers
	ServedAt       time.Time    // when a served request last reported this account (wall clock)
	Error          string       // message of the last failed usage check, empty after a good one
	Problem        checkProblem // kind of the last failed check
	FailCount      int          // failed checks in a row with this same Problem
	FailStreak     int          // failed checks in a row of any kind; limits the quick retries
	LoginExpired   bool
	Windows        map[string]usageWindow
}

// displayName is how the account is shown and keyed in the history: its e-mail address when
// the host knows it.
func (a accountState) displayName() string {
	return firstNonEmpty(a.Label, a.FileName, a.ID)
}

// clearProblem marks the login as working, for example after a request was served with it.
func (a *accountState) clearProblem() {
	a.Error, a.Problem, a.FailCount, a.FailStreak, a.LoginExpired = "", problemNone, 0, 0, false
}

// failureShown reports whether the status page should show the last failed check. A single
// passing failure is not worth a warning; a token being renewed always is.
func (a accountState) failureShown() bool {
	return a.Problem == problemRenewing || (a.Problem != problemNone && a.FailCount >= shownFailChecks)
}

// retrySoon reports whether a failed check should be retried before the normal interval.
func (a accountState) retrySoon() bool {
	if a.Problem == problemNone || a.LoginExpired || a.Disabled {
		return false
	}
	// A token being renewed keeps the quick pace until it counts as expired, so that takes
	// about renewingChecks retries however the failures before it went.
	return a.FailStreak <= retryChecks || (a.Problem == problemRenewing && a.FailCount <= renewingChecks)
}

// exhausted reports whether the account's weekly or 5-hour limit is used up.
func (a accountState) exhausted(now time.Time) bool {
	for _, key := range limitWindows {
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
	done        chan struct{} // closed when loop returns
	started     atomic.Bool
	startedOnce sync.Once
	stopOnce    sync.Once
}

var state = &refresher{
	accounts: map[string]accountState{},
	cleared:  map[string]time.Time{},
	trigger:  make(chan struct{}, 1),
	stop:     make(chan struct{}),
	done:     make(chan struct{}),
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

// markOffered records that the proxy offered these accounts for a pick, which it only does
// for accounts it is not holding back. Every offered account is stamped with the pick time,
// so a refusal record that arrives later but happened earlier cannot pause it again.
func markOffered(cands []pluginapi.SchedulerAuthCandidate) {
	now := time.Now()
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, c := range cands {
		if acc, ok := state.accounts[c.ID]; ok {
			acc.Unavailable, acc.AvailabilityAt = false, now
			acc.RetryAfter, acc.CooldownModel = time.Time{}, ""
			state.accounts[c.ID] = acc
		}
	}
}

func startRefresher() {
	state.startedOnce.Do(func() {
		state.started.Store(true)
		go state.loop()
	})
}

// stopRefresher stops the background loop and waits for a refresh in progress to finish, so
// nothing calls the host or writes the history after shutdown. The wait is bounded by one
// usage request, since a refresh stops between accounts once asked to.
func stopRefresher() {
	state.stopOnce.Do(func() { close(state.stop) })
	if !state.started.Load() {
		return
	}
	select {
	case <-state.done:
	case <-time.After(httpTimeout + 5*time.Second):
	}
}

func (r *refresher) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// requestRefresh asks the background loop to refresh. Manual requests are ignored while a
// refresh is running or when the last one finished less than manualCooldown ago. Returns
// whether a refresh was queued.
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
	defer close(r.done)
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
	for _, acc := range r.accounts {
		if acc.retrySoon() && retryAfterError < wait {
			wait = retryAfterError
		}
	}
	if wait < minWake {
		wait = minWake
	}
	return wait
}

func (r *refresher) refreshAll() {
	runStart := time.Now()
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
	next := make(map[string]accountState, len(entries))
	// Accounts whose last readings showed a full limit. Only for those can a check explain a
	// cooldown away (see clearStaleCooldown).
	wasFull := map[string]bool{}
	for _, e := range entries {
		if r.stopping() {
			return
		}
		acc := accountState{
			ID:          e.ID,
			AuthIndex:   e.AuthIndex,
			Label:       firstNonEmpty(e.Email, e.Label, e.Name),
			FileName:    e.Name,
			Priority:    e.Priority,
			Disabled:    e.Disabled,
			Unavailable: e.Unavailable,
			RetryAfter:  e.NextRetryAfter,
			// The credential list is read at the start of the refresh; a refused request or a
			// pick after that is newer and wins in keepLiveUpdates.
			AvailabilityAt: runStart,
			Status:         e.Status,
			StatusMessage:  e.StatusMessage,
		}
		r.mu.RLock()
		prev, hadPrev := r.accounts[e.ID]
		r.mu.RUnlock()
		if hadPrev {
			acc.HeadersAt = prev.HeadersAt
			acc.ServedAt = prev.ServedAt
			wasFull[e.ID] = prev.exhausted(runStart)
		}
		if e.Disabled {
			if hadPrev {
				acc.Windows, acc.FetchedAt = prev.Windows, prev.FetchedAt
			}
			next[e.ID] = acc
			continue
		}
		windows, errFetch := fetchUsage(e.AuthIndex)
		if errFetch != nil {
			acc.Error = errFetch.Error()
			acc.Problem = classifyProblem(errFetch)
			acc.FailCount, acc.FailStreak = 1, 1
			if hadPrev {
				acc.FailStreak = prev.FailStreak + 1
				if prev.Problem == acc.Problem {
					acc.FailCount = prev.FailCount + 1
				}
			}
			// An expired login stays expired until a check or a served request shows it works;
			// a passing error of another kind in between proves nothing.
			acc.LoginExpired = (hadPrev && prev.LoginExpired) || isLoginExpired(acc.Problem, acc.FailCount, e.Status, e.StatusMessage)
			// Keep the previous readings so a failed check does not reshuffle routing.
			if hadPrev {
				acc.Windows = prev.Windows
				acc.FetchedAt = prev.FetchedAt
			}
			// Log when the kind of failure changes, not on every retry.
			if !hadPrev || prev.Problem != acc.Problem || prev.LoginExpired != acc.LoginExpired {
				hostLog("warn", pluginName+": usage check failed", map[string]any{"account": acc.Label, "error": acc.Error, "login_expired": acc.LoginExpired})
			}
		} else {
			acc.FetchedAt = time.Now()
			if hadPrev {
				// Header readings taken while the check was in flight may be newer.
				windows = mergeChecked(prev.Windows, windows, runStart)
			}
			acc.Windows = windows
		}
		next[e.ID] = acc
	}
	r.mu.Lock()
	keepLiveUpdates(next, r.accounts, runStart)
	r.accounts = next
	r.lastErr = ""
	// Copy the results while locked: once unlocked, next is the live map that served requests
	// write to, and ranging over it then would be a concurrent map read and write.
	results := make([]accountState, 0, len(next))
	for _, acc := range next {
		results = append(results, acc)
	}
	r.mu.Unlock()

	now := time.Now()
	for _, acc := range results {
		if acc.Problem == problemNone && !acc.Disabled && !acc.FetchedAt.Before(runStart) {
			history.add(acc, acc.FetchedAt)
		}
		r.clearStaleCooldown(acc, wasFull[acc.ID], runStart, now)
	}
	history.save()
	hostLog("info", pluginName+": checked usage", map[string]any{"accounts": len(next)})
}

// keepLiveUpdates carries over what happened while a refresh was running, from current (the
// live state) into next (the refresh result): readings and proof of a working login from
// served requests, and cooldowns seen from refused requests and picks, which are newer than
// the credential list the refresh started with. ServedAt is compared rather than HeadersAt,
// because a record that arrives late still proves the login works.
func keepLiveUpdates(next, current map[string]accountState, runStart time.Time) {
	for id, acc := range next {
		cur, ok := current[id]
		if !ok {
			continue
		}
		if cur.HeadersAt.After(acc.HeadersAt) || cur.ServedAt.After(acc.ServedAt) {
			if acc.FetchedAt.Before(runStart) {
				// This account's check failed (or it is disabled): nothing fresh to defer to.
				acc.Windows = mergeWindows(acc.Windows, cur.Windows)
			} else {
				acc.Windows = mergeChecked(cur.Windows, acc.Windows, runStart)
			}
			acc.HeadersAt = cur.HeadersAt
		}
		if cur.ServedAt.After(acc.ServedAt) {
			acc.ServedAt = cur.ServedAt
			acc.clearProblem()
		}
		if cur.AvailabilityAt.After(acc.AvailabilityAt) {
			acc.Unavailable, acc.RetryAfter, acc.AvailabilityAt, acc.CooldownModel = cur.Unavailable, cur.RetryAfter, cur.AvailabilityAt, cur.CooldownModel
		}
		next[id] = acc
	}
}

// resetCooldown asks the proxy to stop holding an account back. A variable so tests can
// replace the host call.
var resetCooldown = func(authIndex string) error {
	_, err := callHost(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex})
	return err
}

// clearStaleCooldown lifts the proxy's cooldown on an account whose full limit was reset
// early: its last readings showed a limit at 100 percent, and a fresh check shows room again.
// Without this the proxy keeps skipping the account until the reset time it was given.
// A cooldown the readings never explained is left alone: Anthropic can refuse requests for
// reasons the usage figures do not show, and the proxy knows better then.
func (r *refresher) clearStaleCooldown(acc accountState, wasFull bool, runStart, now time.Time) {
	if !acc.Unavailable || !wasFull || acc.Disabled || acc.Problem != problemNone || acc.AuthIndex == "" {
		return
	}
	// Only a reading from this refresh counts; an older one says nothing about the cooldown.
	// A refusal that came in after the check is newer than the reading, so leave it alone.
	if acc.FetchedAt.Before(runStart) || acc.AvailabilityAt.After(acc.FetchedAt) || !hasRoom(acc, now) {
		return
	}
	r.mu.RLock()
	recent := now.Sub(r.cleared[acc.ID]) < cooldownClearEvery
	r.mu.RUnlock()
	if recent {
		return
	}
	if err := resetCooldown(acc.AuthIndex); err != nil {
		// Not recorded as cleared, so the next check tries again.
		hostLog("warn", pluginName+": clearing cooldown failed", map[string]any{"account": acc.Label, "error": err.Error()})
		return
	}
	r.mu.Lock()
	r.cleared[acc.ID] = now
	r.mu.Unlock()
	hostLog("info", pluginName+": cleared cooldown, the account's full limit was reset early", map[string]any{"account": acc.Label})
}

// hasRoom reports whether the readings show the account below both its limits.
func hasRoom(acc accountState, now time.Time) bool {
	for _, key := range limitWindows {
		if _, ok := acc.Windows[key]; !ok {
			return false
		}
	}
	return !acc.exhausted(now)
}

// isLoginExpired recognises a credential that needs a new login, as opposed to a passing error.
// failCount is the number of failed checks in a row with this same problem, including this one.
// A rejected or expired token alone is not enough at first: the proxy renews tokens on its own
// schedule, and right after the computer wakes up the stored token can be stale for a while.
func isLoginExpired(problem checkProblem, failCount int, status, statusMessage string) bool {
	switch {
	case problem == problemNoToken:
		return true
	case problem == problemUnauthorized && failCount >= unauthorizedChecks:
		return true
	case problem == problemRenewing && failCount >= renewingChecks:
		return true
	}
	if !strings.EqualFold(status, "error") {
		return false
	}
	msg := strings.ToLower(statusMessage)
	for _, hint := range []string{"invalid_grant", "refresh token", "refresh_token", "unauthorized", "401"} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

func listClaudeAuths() ([]pluginapi.HostAuthFileEntry, error) {
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
	var out []pluginapi.HostAuthFileEntry
	for _, f := range resp.Files {
		var entry pluginapi.HostAuthFileEntry
		if err := json.Unmarshal(f, &entry); err != nil {
			hostLog("warn", pluginName+": skipping an unreadable credential entry", map[string]any{"error": err.Error()})
			continue
		}
		if !isClaude(entry.Provider) && !isClaude(entry.Type) {
			continue
		}
		out = append(out, entry)
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
		Expired     string `json:"expired"`
	}
	if err := json.Unmarshal(got.JSON, &cred); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return nil, errNoAccessToken
	}
	if tokenStale(cred.Expired, time.Now()) {
		return nil, errTokenRenewing
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cred.AccessToken))
	req.Header.Set("anthropic-beta", usageAnthropicBeta)
	req.Header.Set("User-Agent", usageUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
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

// tokenStale reports whether the credential's access token has expired or is about to. An
// unknown or unreadable expiry counts as fresh, so the usage endpoint decides.
func tokenStale(expired string, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(expired))
	return err == nil && now.Add(tokenRenewSlack).After(exp)
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
