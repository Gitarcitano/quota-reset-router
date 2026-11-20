package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const reconcileInterval = 30 * time.Second

type authHost interface {
	List() ([]pluginapi.HostAuthFileEntry, error)
	Get(string) (json.RawMessage, error)
}

type accountState struct {
	Index       string         `json:"auth_index"`
	Provider    string         `json:"provider"`
	Label       string         `json:"label,omitempty"`
	Stamp       string         `json:"-"`
	Snapshot    *quotaSnapshot `json:"quota,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	NextRefresh time.Time      `json:"next_refresh"`
	Failures    int            `json:"consecutive_failures"`
}

type decision struct {
	AuthID string `json:"auth_id,omitempty"`
	Reason string `json:"reason"`
	Reject bool   `json:"reject,omitempty"`
}

type sessionBind struct {
	authID  string
	expires time.Time
}

type engine struct {
	host     authHost
	fetcher  quotaFetcher
	cfg      config
	now      func() time.Time
	mu       sync.RWMutex
	states   map[string]accountState
	sessions map[string]sessionBind
	listErr  string
	last     decision
	picks    atomic.Uint64
	routed   atomic.Uint64
	failed   atomic.Bool
	refresh  sync.Mutex
}

func newEngine(host authHost, fetcher quotaFetcher, cfg config) *engine {
	return &engine{host: host, fetcher: fetcher, cfg: cfg, now: time.Now, states: make(map[string]accountState), sessions: make(map[string]sessionBind)}
}

func (e *engine) run(ctx context.Context) {
	e.refreshQuota(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshQuota(ctx)
		}
	}
}

func (e *engine) refreshQuota(ctx context.Context) {
	e.refresh.Lock()
	defer e.refresh.Unlock()
	if ctx.Err() != nil {
		return
	}
	entries, err := e.host.List()
	if err != nil {
		e.mu.Lock()
		e.listErr = "credential_list_failed"
		e.mu.Unlock()
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	eligible := make(map[string]bool)
	e.mu.Lock()
	e.listErr = ""
	for _, entry := range entries {
		provider := entry.Provider
		if provider == "" {
			provider = entry.Type
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		if (provider != "claude" && provider != "codex") || entry.Disabled || entry.Status == "disabled" || entry.RuntimeOnly || entry.ID == "" || entry.AuthIndex == "" || entry.BaseURL != "" {
			continue
		}
		eligible[entry.ID] = true
		state := e.states[entry.ID]
		stamp := fmt.Sprintf("%q:%q:%q:%d:%d", entry.Path, entry.Account, entry.Email, entry.Size, entry.ModTime.UnixNano())
		if state.Index != entry.AuthIndex || state.Provider != provider || state.Stamp != stamp {
			state = accountState{Index: entry.AuthIndex, Provider: provider, Stamp: stamp}
		}
		state.Label = accountLabel(entry)
		e.states[entry.ID] = state
	}
	for id := range e.states {
		if !eligible[id] {
			delete(e.states, id)
		}
	}
	e.mu.Unlock()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !eligible[entry.ID] {
			continue
		}
		provider := entry.Provider
		if provider == "" {
			provider = entry.Type
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		e.mu.RLock()
		state := e.states[entry.ID]
		e.mu.RUnlock()
		if e.now().Before(state.NextRefresh) {
			continue
		}
		snap, err := e.readQuota(ctx, entry.AuthIndex, provider)
		if ctx.Err() != nil {
			return
		}
		now := e.now()
		if err != nil {
			state.Failures++
			state.LastError = "quota_refresh_failed"
			var qerr *quotaError
			delay := time.Minute * time.Duration(1<<min(state.Failures-1, 4))
			if errors.As(err, &qerr) {
				state.LastError = qerr.code
				delay = max(delay, qerr.retryAfter)
				if qerr.code == "quota_http_401" || qerr.code == "quota_http_403" {
					state.Snapshot = nil
				}
			}
			state.NextRefresh = now.Add(delay)
		} else {
			state.Snapshot = &snap
			state.LastError = ""
			state.Failures = 0
			state.NextRefresh = nextRefresh(snap, now, e.cfg.PollInterval)
		}
		e.mu.Lock()
		e.states[entry.ID] = state
		e.mu.Unlock()
	}
}

func (e *engine) readQuota(ctx context.Context, index, provider string) (quotaSnapshot, error) {
	raw, err := e.host.Get(index)
	if err != nil {
		return quotaSnapshot{}, errors.New("credential_read_failed")
	}
	var cred credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return quotaSnapshot{}, errors.New("credential_decode_failed")
	}
	if strings.TrimSpace(cred.Type) == "" {
		cred.Type = provider
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.RequestTimeout)
	defer cancel()
	return e.fetcher.Fetch(ctx, cred)
}

func nextRefresh(s quotaSnapshot, now time.Time, interval time.Duration) time.Time {
	next := now.Add(interval)
	for _, w := range []*quotaWindow{&s.Weekly, s.FiveHour, s.Sonnet, s.Opus} {
		if w != nil && w.ResetsAt != nil && w.ResetsAt.Add(5*time.Second).Before(next) {
			next = w.ResetsAt.Add(5 * time.Second)
		}
	}
	return maxTime(next, now.Add(reconcileInterval))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func providerOnly(req pluginapi.SchedulerPickRequest, provider string) bool {
	if req.Provider != provider && !(req.Provider == "" && len(req.Providers) == 1 && req.Providers[0] == provider) {
		return false
	}
	for _, p := range req.Providers {
		if p != provider {
			return false
		}
	}
	for _, c := range req.Candidates {
		if c.Provider != provider {
			return false
		}
	}
	return true
}

func candidateIdentity(c pluginapi.SchedulerAuthCandidate) string {
	account, _ := c.Metadata["account_uuid"].(string)
	org, _ := c.Metadata["organization_uuid"].(string)
	token, _ := c.Metadata["access_token"].(string)
	if account == "" && token == "" {
		return ""
	}
	return (credential{Type: c.Provider, AccountUUID: account, Organization: org, AccessToken: token}).identity()
}

func accountLabel(entry pluginapi.HostAuthFileEntry) string {
	for _, value := range []string{entry.Email, entry.Label, entry.Name, entry.ID} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func choose(req pluginapi.SchedulerPickRequest, states map[string]accountState, now time.Time, maxAge time.Duration) decision {
	cfg, _ := decodeConfig(nil)
	cfg.Mode = "active"
	return chooseWith(req, states, now, maxAge, cfg, "")
}

func chooseWith(req pluginapi.SchedulerPickRequest, states map[string]accountState, now time.Time, maxAge time.Duration, cfg config, sticky string) decision {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" && len(req.Providers) == 1 {
		provider = strings.ToLower(strings.TrimSpace(req.Providers[0]))
	}
	if provider != "claude" && provider != "codex" || !providerOnly(req, provider) {
		return decision{Reason: "provider_not_exclusively_supported"}
	}
	type ranked struct {
		candidate *pluginapi.SchedulerAuthCandidate
		reset     time.Time
		window    bool
	}
	var best *ranked
	consider := func(c *pluginapi.SchedulerAuthCandidate) (ranked, bool) {
		if c.ID == "" || strings.EqualFold(c.Status, "disabled") {
			return ranked{}, false
		}
		if disabled, _ := c.Metadata["disabled"].(bool); disabled {
			return ranked{}, false
		}
		window, ok := eligible(states[c.ID], *c, req.Model, provider, now, maxAge, cfg)
		if !ok {
			return ranked{}, false
		}
		return ranked{candidate: c, reset: *states[c.ID].Snapshot.Weekly.ResetsAt, window: window}, true
	}
	better := func(next, current ranked) bool {
		if next.candidate.Priority != current.candidate.Priority {
			return next.candidate.Priority > current.candidate.Priority
		}
		if next.window != current.window {
			return next.window
		}
		if next.reset.Equal(current.reset) {
			return next.candidate.ID < current.candidate.ID
		}
		return next.reset.Before(current.reset)
	}
	if sticky != "" {
		for i := range req.Candidates {
			if req.Candidates[i].ID != sticky {
				continue
			}
			if _, ok := consider(&req.Candidates[i]); ok {
				return decision{AuthID: sticky, Reason: "session_affinity"}
			}
		}
	}
	for i := range req.Candidates {
		row, ok := consider(&req.Candidates[i])
		if !ok {
			continue
		}
		if best == nil || better(row, *best) {
			copy := row
			best = &copy
		}
	}
	if best == nil {
		return decision{Reason: "no_account_within_policy", Reject: true}
	}
	reason := "earliest_weekly_reset"
	if best.window {
		reason = "release_window"
	}
	return decision{AuthID: best.candidate.ID, Reason: reason}
}

func eligible(state accountState, c pluginapi.SchedulerAuthCandidate, model, provider string, now time.Time, maxAge time.Duration, cfg config) (bool, bool) {
	if blockReason(state, c, model, provider, now, maxAge, cfg) != "" {
		return false, false
	}
	window := state.Snapshot.Weekly.ResetsAt.Sub(now) <= cfg.releaseBefore(c.ID)
	return window, true
}

func blockReason(state accountState, c pluginapi.SchedulerAuthCandidate, model, provider string, now time.Time, maxAge time.Duration, cfg config) string {
	if c.ID == "" || strings.EqualFold(c.Status, "disabled") {
		return "disabled"
	}
	if disabled, _ := c.Metadata["disabled"].(bool); disabled {
		return "disabled"
	}
	s := state.Snapshot
	stateProvider := state.Provider
	if stateProvider == "" {
		stateProvider = provider
	}
	identity := candidateIdentity(c)
	if stateProvider != provider || s == nil || s.Identity == "" || identity != "" && s.Identity != identity {
		return "quota_unknown"
	}
	if reason := s.availability(model, now, maxAge); reason != "ready" {
		return reason
	}
	if s.Weekly.ResetsAt == nil || !s.Weekly.ResetsAt.After(now) {
		return "reset_pending_refresh"
	}
	if overCap(s.FiveHour, cfg.fiveHourCap(c.ID)) {
		return "five_hour_cap"
	}
	if s.Weekly.ResetsAt.Sub(now) > cfg.releaseBefore(c.ID) && overCap(&s.Weekly, cfg.weeklyCap(c.ID)) {
		return "weekly_cap"
	}
	return ""
}

func overCap(window *quotaWindow, cap float64) bool {
	return window != nil && window.Utilization != nil && *window.Utilization >= cap
}

func (e *engine) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	if e.failed.Load() {
		return pluginapi.SchedulerPickResponse{}
	}
	now := e.now()
	sid := sessionID(req)
	e.mu.Lock()
	sticky := ""
	if sid != "" && e.cfg.SessionTTL > 0 {
		if bind, ok := e.sessions[sid]; ok && now.Before(bind.expires) {
			sticky = bind.authID
		} else {
			delete(e.sessions, sid)
		}
	}
	d := chooseWith(req, e.states, now, e.cfg.MaxAge, e.cfg, sticky)
	e.last = d
	if e.cfg.Mode == "active" && d.AuthID != "" && sid != "" && e.cfg.SessionTTL > 0 {
		e.sessions[sid] = sessionBind{authID: d.AuthID, expires: now.Add(e.cfg.SessionTTL)}
	}
	e.mu.Unlock()
	e.picks.Add(1)
	if e.cfg.Mode != "active" || d.AuthID == "" && !d.Reject {
		return pluginapi.SchedulerPickResponse{}
	}
	if d.AuthID == "" {
		return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_policy", RejectReason: d.Reason}
	}
	e.routed.Add(1)
	return pluginapi.SchedulerPickResponse{AuthID: d.AuthID, Handled: true}
}

func sessionID(req pluginapi.SchedulerPickRequest) string {
	if req.Options.Metadata != nil {
		for _, key := range []string{"canonical_session_id", "session_id"} {
			if value, _ := req.Options.Metadata[key].(string); strings.TrimSpace(value) != "" {
				value = strings.TrimSpace(value)
				if key == "canonical_session_id" || strings.Contains(value, ":") {
					return value
				}
				return "session:" + value
			}
		}
	}
	for _, key := range []string{"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Session-Id", "X-Session-Affinity", "X-Client-Request-Id"} {
		value := headerValue(req.Options.Headers, key)
		if value == "" {
			continue
		}
		if strings.Contains(strings.ToLower(key), "claude") {
			return "claude:" + value
		}
		return "session:" + value
	}
	return ""
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

type accountReport struct {
	accountState
	FiveHourCap     float64 `json:"five_hour_cap"`
	WeeklyCap       float64 `json:"weekly_cap"`
	ReleaseBefore   string  `json:"release_before"`
	InReleaseWindow bool    `json:"in_release_window"`
	Stale           bool    `json:"stale"`
	BlockReason     string  `json:"block_reason,omitempty"`
}

func (e *engine) reportAccount(id string, state accountState, now time.Time) accountReport {
	report := accountReport{
		accountState:  state,
		FiveHourCap:   e.cfg.fiveHourCap(id),
		WeeklyCap:     e.cfg.weeklyCap(id),
		ReleaseBefore: e.cfg.releaseBefore(id).String(),
	}
	if state.Snapshot == nil {
		report.BlockReason = "quota_unknown"
		report.Stale = true
		return report
	}
	candidate := pluginapi.SchedulerAuthCandidate{ID: id, Provider: state.Provider, Status: "active"}
	report.BlockReason = blockReason(state, candidate, "", state.Provider, now, e.cfg.MaxAge, e.cfg)
	report.Stale = report.BlockReason == "stale" || state.Snapshot.availability("", now, e.cfg.MaxAge) == "stale"
	if state.Snapshot.Weekly.ResetsAt != nil && state.Snapshot.Weekly.ResetsAt.After(now) {
		report.InReleaseWindow = state.Snapshot.Weekly.ResetsAt.Sub(now) <= e.cfg.releaseBefore(id)
	}
	return report
}

func (e *engine) status() any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	now := e.now()
	accounts := make(map[string]accountReport, len(e.states))
	for id, state := range e.states {
		accounts[id] = e.reportAccount(id, state, now)
	}
	return struct {
		Version       string                   `json:"version"`
		Policy        string                   `json:"selection_policy"`
		Mode          string                   `json:"mode"`
		PollInterval  string                   `json:"poll_interval"`
		MaxAge        string                   `json:"max_age"`
		FiveHourCap   float64                  `json:"five_hour_cap"`
		WeeklyCap     float64                  `json:"weekly_cap"`
		ReleaseBefore string                   `json:"release_before"`
		SessionTTL    string                   `json:"session_ttl"`
		Accounts      map[string]accountReport `json:"accounts"`
		ListError     string                   `json:"list_error,omitempty"`
		LastDecision  decision                 `json:"last_decision"`
		Picks         uint64                   `json:"picks"`
		Routed        uint64                   `json:"routed"`
		WorkerFailed  bool                     `json:"worker_failed"`
	}{pluginVersion, "affinity_then_priority_then_release_window_then_weekly_reset", e.cfg.Mode, e.cfg.PollInterval.String(), e.cfg.MaxAge.String(), e.cfg.FiveHourCap, e.cfg.WeeklyCap, e.cfg.ReleaseBefore.String(), e.cfg.SessionTTL.String(), accounts, e.listErr, e.last, e.picks.Load(), e.routed.Load(), e.failed.Load()}
}
