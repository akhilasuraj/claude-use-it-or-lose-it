package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName      = "reset-scheduler"
	pluginVersion   = "0.2.0"
	resourcePath    = "/status"
	claudeProvider  = "claude"
	defaultInterval = 30
	minInterval     = 5
)

// Config keys double as the labels the management panel shows (it has no separate label
// field), so they are written as plain sentence-case text. The legacy snake_case keys and
// values are still accepted so older config.yaml files keep working.
const (
	keyInterval     = "Check usage every (minutes)"
	keyWindow       = "Weekly limit to order by"
	keyDeprioritize = "Move full accounts to the bottom"

	windowAll    = "All models"
	windowOpus   = "Opus"
	windowSonnet = "Sonnet"
)

var legacyKeys = map[string]string{
	keyInterval:     "refresh_interval_minutes",
	keyWindow:       "weekly_window",
	keyDeprioritize: "deprioritize_exhausted",
}

// pluginConfig mirrors plugins.configs.reset-scheduler in config.yaml.
type pluginConfig struct {
	RefreshIntervalMinutes int
	WeeklyWindow           string // usage API window key: seven_day, seven_day_opus, seven_day_sonnet
	DeprioritizeExhausted  *bool
}

// parseConfig reads the plugin block, preferring the labelled keys over the legacy ones.
func parseConfig(data []byte) (pluginConfig, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return pluginConfig{}, err
	}
	get := func(key string) (any, bool) {
		if v, ok := raw[key]; ok && v != nil {
			return v, true
		}
		v, ok := raw[legacyKeys[key]]
		return v, ok && v != nil
	}
	cfg := pluginConfig{}
	if v, ok := get(keyInterval); ok {
		switch n := v.(type) {
		case int:
			cfg.RefreshIntervalMinutes = n
		case float64:
			cfg.RefreshIntervalMinutes = int(n)
		case string:
			fmt.Sscanf(strings.TrimSpace(n), "%d", &cfg.RefreshIntervalMinutes)
		}
	}
	if v, ok := get(keyWindow); ok {
		cfg.WeeklyWindow = windowKey(fmt.Sprint(v))
	}
	if v, ok := get(keyDeprioritize); ok {
		var b bool
		switch x := v.(type) {
		case bool:
			b = x
		case string:
			b = !strings.EqualFold(strings.TrimSpace(x), "false")
		default:
			b = true
		}
		cfg.DeprioritizeExhausted = &b
	}
	return cfg, nil
}

// windowKey maps a display value (or a legacy API key) to the usage API window key.
func windowKey(v string) string {
	switch strings.TrimSpace(v) {
	case windowOpus, "seven_day_opus":
		return "seven_day_opus"
	case windowSonnet, "seven_day_sonnet":
		return "seven_day_sonnet"
	default:
		return "seven_day"
	}
}

func (c pluginConfig) interval() time.Duration {
	m := c.RefreshIntervalMinutes
	if m <= 0 {
		m = defaultInterval
	}
	if m < minInterval {
		m = minInterval
	}
	return time.Duration(m) * time.Minute
}

func (c pluginConfig) window() string {
	return windowKey(c.WeeklyWindow)
}

func (c pluginConfig) deprioritizeExhausted() bool {
	return c.DeprioritizeExhausted == nil || *c.DeprioritizeExhausted
}

var currentConfig atomic.Value // pluginConfig

func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return pluginConfig{}
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		parsed, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
		cfg = parsed
	}
	previous := loadedConfig()
	currentConfig.Store(cfg)
	if previous.window() != cfg.window() || previous.interval() != cfg.interval() {
		requestRefresh(true)
	}
	return nil
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		startRefresher()
		return okEnvelope(registration())
	case pluginabi.MethodSchedulerPick:
		return pick(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(map[string]any{
			"resources": []map[string]string{{
				"Path":        resourcePath,
				"Menu":        "Reset Scheduler",
				"Description": "Claude accounts ordered by weekly reset (soonest first).",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return handleResource(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:    pluginName,
			Version: pluginVersion,
			Author:  "local build",
			// Required by the host; this plugin is local-only and not published anywhere.
			GitHubRepository: "https://github.com/local/cliproxy-reset-scheduler",
			Logo:             pluginLogo,
			// The host HTML-escapes descriptions and the panel shows them as plain text, so keep
			// them free of ' " & < > (TestConfigFieldDescriptionsArePlain guards this).
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        keyInterval,
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     defaultInterval,
					Description: "How often the plugin checks account usage with Anthropic. Minimum 5, default 30. Requests never trigger a check.",
				},
				{
					Name:        keyWindow,
					Type:        pluginapi.ConfigFieldTypeEnum,
					Default:     windowAll,
					EnumValues:  []string{windowAll, windowOpus, windowSonnet},
					Description: "Accounts are ordered by when this limit resets. Keep All models unless your plan has separate Opus or Sonnet weekly limits.",
				},
				{
					Name:        keyDeprioritize,
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "Accounts at 100 percent of their weekly or 5-hour limit are tried last. Default on.",
				},
			},
		},
		"capabilities": map[string]any{
			"scheduler":                   true,
			"scheduler_across_priorities": true,
			"management_api":              true,
		},
	}
}

// ---- scheduling --------------------------------------------------------------------------

type pickStats struct {
	mu       sync.Mutex
	counts   map[string]int64
	lastID   string
	lastAt   time.Time
	lastNote string
}

var stats = &pickStats{counts: map[string]int64{}}

func (s *pickStats) record(id, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		s.counts[id]++
		s.lastID = id
	}
	s.lastAt = time.Now()
	s.lastNote = note
}

func pick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	id, note := choose(req.Candidates, snapshotAccounts(), loadedConfig(), time.Now())
	if id == "" {
		// Leave the decision to the configured built-in routing strategy.
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	stats.record(id, note)
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: id, Handled: true})
}

// choose returns the candidate ID to use, or "" to fall back to the built-in strategy.
// It only reads cached data and never performs network calls.
func choose(cands []pluginapi.SchedulerAuthCandidate, accounts map[string]accountState, cfg pluginConfig, now time.Time) (string, string) {
	if len(cands) == 0 {
		return "", ""
	}
	for _, c := range cands {
		if !strings.EqualFold(strings.TrimSpace(c.Provider), claudeProvider) {
			return "", "non-claude candidates present"
		}
	}
	ranked := rankCandidates(cands, accounts, cfg, now)
	if len(ranked) == 0 || !ranked[0].hasReset {
		return "", "no cached reset data"
	}
	return ranked[0].id, "soonest weekly reset"
}

type rankedCandidate struct {
	id        string
	priority  int
	hasReset  bool
	resetsAt  time.Time
	exhausted bool
}

func rankCandidates(cands []pluginapi.SchedulerAuthCandidate, accounts map[string]accountState, cfg pluginConfig, now time.Time) []rankedCandidate {
	out := make([]rankedCandidate, 0, len(cands))
	for _, c := range cands {
		rc := rankedCandidate{id: c.ID, priority: c.Priority}
		if acc, ok := accounts[c.ID]; ok && acc.Error == "" {
			if w, ok := acc.Windows[cfg.window()]; ok && !w.ResetsAt.IsZero() && w.ResetsAt.After(now) {
				rc.hasReset = true
				rc.resetsAt = w.ResetsAt
			}
			if cfg.deprioritizeExhausted() && acc.exhausted(cfg.window(), now) {
				rc.exhausted = true
			}
		}
		out = append(out, rc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.exhausted != b.exhausted {
			return !a.exhausted
		}
		if a.hasReset != b.hasReset {
			return a.hasReset
		}
		if a.hasReset && !a.resetsAt.Equal(b.resetsAt) {
			return a.resetsAt.Before(b.resetsAt)
		}
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		return a.id < b.id
	})
	return out
}

// ---- resource page -----------------------------------------------------------------------

type managementRequest struct {
	Method string
	Path   string
	Query  url.Values
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func handleResource(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode resource request: %w", err)
		}
	}
	notice := ""
	if strings.EqualFold(req.Query.Get("action"), "refresh") {
		if requestRefresh(false) {
			notice = "Refresh started. This page reloads automatically."
		} else {
			notice = "A refresh ran less than a minute ago, so this one was skipped."
		}
	}
	body := renderPage(notice, time.Now())
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"content-type":  []string{"text/html; charset=utf-8"},
			"cache-control": []string{"no-store"},
		},
		Body: body,
	})
}
