package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	historyFile      = "history.json"
	historyBucket    = 10 * time.Minute    // at most one stored reading per account per bucket
	historyRetention = 15 * 24 * time.Hour // two weekly cycles plus a day
	paceLookback     = 24 * time.Hour      // burn rate is the average over the last day
	paceMinSpan      = 2 * time.Hour       // shorter spans give no rate
	cycleMatch       = 30 * time.Minute    // readings whose reset time is this close share a cycle
)

// sample is one stored usage reading for an account.
type sample struct {
	T int64              `json:"t"`           // Unix seconds
	U map[string]float64 `json:"u"`           // window key -> utilization percent
	R map[string]int64   `json:"r,omitempty"` // window key -> reset time, Unix seconds
}

func (s sample) time() time.Time { return time.Unix(s.T, 0) }

// historyStore keeps usage readings per account so the page can chart them and work out a
// burn rate. Accounts are keyed by their label (the e-mail address), which survives a new
// login, unlike the credential ID.
type historyStore struct {
	mu       sync.Mutex
	path     string
	isLoaded bool
	dirty    bool
	accounts map[string][]sample
}

var history = &historyStore{accounts: map[string][]sample{}}

type historyFileData struct {
	Version  int                 `json:"version"`
	Accounts map[string][]sample `json:"accounts"`
}

func (c pluginConfig) historyPath() string {
	if c.HistoryDir != "" {
		return filepath.Join(c.HistoryDir, historyFile)
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, pluginName, historyFile)
	}
	return filepath.Join(os.TempDir(), pluginName, historyFile)
}

func historyKey(acc accountState) string {
	return strings.ToLower(firstNonEmpty(acc.Label, acc.FileName, acc.ID))
}

func (h *historyStore) loaded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isLoaded
}

// load switches to the file at path. Readings already in memory are kept and merged, so
// changing the folder moves the history rather than losing it.
func (h *historyStore) load(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.path = path
	h.isLoaded = true
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			hostLog("warn", pluginName+": reading usage history failed", map[string]any{"path": path, "error": err.Error()})
		}
		h.dirty = len(h.accounts) > 0
		return
	}
	var data historyFileData
	if err := json.Unmarshal(raw, &data); err != nil {
		hostLog("warn", pluginName+": usage history is unreadable, starting fresh", map[string]any{"path": path, "error": err.Error()})
		return
	}
	for key, samples := range data.Accounts {
		h.accounts[key] = mergeSamples(h.accounts[key], samples)
	}
	h.prune(time.Now())
}

func mergeSamples(a, b []sample) []sample {
	out := append(append([]sample{}, a...), b...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	dedup := out[:0]
	for _, s := range out {
		if n := len(dedup); n > 0 && dedup[n-1].T == s.T {
			dedup[n-1] = s
			continue
		}
		dedup = append(dedup, s)
	}
	return dedup
}

// add records the account's current readings. Readings in the same 10-minute bucket replace
// each other, so busy accounts do not grow the file.
func (h *historyStore) add(acc accountState, at time.Time) {
	if len(acc.Windows) == 0 || at.IsZero() {
		return
	}
	s := sample{T: at.Unix(), U: map[string]float64{}, R: map[string]int64{}}
	for key, w := range acc.Windows {
		s.U[key] = w.Utilization
		if !w.ResetsAt.IsZero() {
			s.R[key] = w.ResetsAt.Unix()
		}
	}
	key := historyKey(acc)
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.accounts[key]
	if n := len(list); n > 0 {
		last := list[n-1]
		if s.T < last.T {
			return
		}
		if last.time().Truncate(historyBucket).Equal(at.Truncate(historyBucket)) && sameCycle(last, s) {
			// Keep readings from other windows the newer one lacks.
			for k, v := range last.U {
				if _, ok := s.U[k]; !ok {
					s.U[k] = v
				}
			}
			list[n-1] = s
			h.accounts[key] = list
			h.dirty = true
			return
		}
	}
	h.accounts[key] = append(list, s)
	h.dirty = true
	h.prune(at)
}

// sameCycle reports whether two readings belong to the same weekly cycle.
func sameCycle(a, b sample) bool {
	ra, rb := a.R["seven_day"], b.R["seven_day"]
	return ra == 0 || rb == 0 || math.Abs(float64(ra-rb)) < cycleMatch.Seconds()
}

func (h *historyStore) prune(now time.Time) {
	cutoff := now.Add(-historyRetention).Unix()
	for key, list := range h.accounts {
		i := sort.Search(len(list), func(i int) bool { return list[i].T >= cutoff })
		if i == len(list) {
			delete(h.accounts, key)
			continue
		}
		if i > 0 {
			h.accounts[key] = append([]sample{}, list[i:]...)
		}
	}
}

// save writes the history if it changed. It writes to a temporary file first so a crash
// mid-write never leaves a broken file behind.
func (h *historyStore) save() {
	h.mu.Lock()
	if !h.dirty || h.path == "" {
		h.mu.Unlock()
		return
	}
	raw, err := json.Marshal(historyFileData{Version: 1, Accounts: h.accounts})
	path := h.path
	h.dirty = false
	h.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(path, raw)
	}
	if err != nil {
		h.mu.Lock()
		h.dirty = true
		h.mu.Unlock()
		hostLog("warn", pluginName+": saving usage history failed", map[string]any{"path": path, "error": err.Error()})
	}
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// point is one reading of a single window, ready for charting.
type point struct {
	At  time.Time
	Pct float64
}

// cycle returns the account's readings of the given window that belong to the cycle ending
// at resetsAt, oldest first.
func (h *historyStore) cycle(key, window string, resetsAt time.Time) []point {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []point
	start := resetsAt.Add(-7 * 24 * time.Hour)
	for _, s := range h.accounts[key] {
		u, ok := s.U[window]
		if !ok {
			continue
		}
		at := s.time()
		if r, ok := s.R[window]; ok {
			if math.Abs(float64(r-resetsAt.Unix())) > cycleMatch.Seconds() {
				continue
			}
		} else if at.Before(start) || at.After(resetsAt) {
			continue
		}
		out = append(out, point{At: at, Pct: u})
	}
	return out
}

// pace describes how fast an account is using its weekly limit and where that leads.
type pace struct {
	Known     bool
	PerHour   float64   // percentage points per hour, averaged over the last day
	Projected float64   // percent used at reset at the current pace, capped at 100
	Unused    float64   // percent left over at reset at the current pace
	RunsOutAt time.Time // when the limit is reached at the current pace, if before reset
	NeedPerHr float64   // pace that would use the whole limit exactly by reset
}

// paceFor works out the burn rate from the readings of the current cycle.
func paceFor(points []point, cur usageWindow, now time.Time) pace {
	p := pace{}
	left := cur.ResetsAt.Sub(now).Hours()
	if cur.ResetsAt.IsZero() || left <= 0 {
		return p
	}
	p.NeedPerHr = math.Max(0, 100-cur.Utilization) / left
	from := now.Add(-paceLookback)
	var first *point
	for i := range points {
		if !points[i].At.Before(from) {
			first = &points[i]
			break
		}
	}
	if first == nil || now.Sub(first.At) < paceMinSpan {
		return p
	}
	p.Known = true
	p.PerHour = math.Max(0, (cur.Utilization-first.Pct)/now.Sub(first.At).Hours())
	projected := cur.Utilization + p.PerHour*left
	if projected >= 100 {
		p.Projected = 100
		if p.PerHour > 0 {
			p.RunsOutAt = now.Add(time.Duration((100 - cur.Utilization) / p.PerHour * float64(time.Hour)))
		}
	} else {
		p.Projected = projected
	}
	p.Unused = math.Max(0, 100-p.Projected)
	return p
}
