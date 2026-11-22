package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	reconcileInterval = 30 * time.Second
	maxPollBackoff    = 6 * time.Hour
	priorityStep      = 10
	// CPA refreshes a Claude token 4h before it expires. A priority write is a read-modify-write
	// of the whole credential file, so it is deferred while a refresh could be running, to never
	// save an old token over a refreshed one.
	tokenRefreshGuard  = 4*time.Hour + 10*time.Minute
	recentRefreshGuard = 2 * time.Minute
)

// authHost is the subset of host callbacks the engine uses.
type authHost interface {
	List() ([]pluginapi.HostAuthFileEntry, error)
	// Get returns the credential file name and its raw JSON.
	Get(index string) (string, json.RawMessage, error)
	Save(name string, raw json.RawMessage) error
	Log(level, message string, fields map[string]any)
}

// window is the last known state of one Anthropic rate-limit window.
// Utilization is a percentage (0.69 in a header becomes 69).
type window struct {
	Utilization *float64   `json:"utilization,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	Status      string     `json:"status,omitempty"`
}

// live reports whether the window's reset time is known and still in the future.
func (w window) live(now time.Time) bool {
	return w.ResetsAt != nil && w.ResetsAt.After(now)
}

func (w window) atLeast(percent float64, now time.Time) bool {
	return w.live(now) && w.Utilization != nil && *w.Utilization >= percent
}

func (w window) exhausted(now time.Time) bool {
	return w.live(now) && (strings.EqualFold(w.Status, "rejected") || w.Utilization != nil && *w.Utilization >= 100)
}

type account struct {
	ID          string
	Index       string
	Email       string
	Label       string
	Priority    int
	FiveHour    window
	Weekly      window
	Source      string
	ObservedAt  time.Time
	PollBackoff time.Duration
	NextPoll    time.Time
	PollError   string
	LastWrite   string
	LastRefresh time.Time
}

func (a *account) atReserve(cfg config, now time.Time) bool {
	r := cfg.reserveFor(a.ID, a.Email)
	return r.FiveHourReserve != nil && a.FiveHour.atLeast(100-*r.FiveHourReserve, now) ||
		r.WeeklyReserve != nil && a.Weekly.atLeast(100-*r.WeeklyReserve, now)
}

func (a *account) exhausted(now time.Time) bool {
	return a.FiveHour.exhausted(now) || a.Weekly.exhausted(now)
}

type pickRecord struct {
	At           time.Time `json:"at"`
	Candidates   int       `json:"candidates"`
	WouldExclude []string  `json:"would_exclude,omitempty"`
	Applied      bool      `json:"applied"`
}

type engine struct {
	host     authHost
	fetcher  quotaFetcher
	cfg      config
	file     *reservesFile // nil unless accounts_file is set
	mirrorAt atomic.Pointer[time.Time]
	now      func() time.Time
	mu       sync.Mutex
	accounts map[string]*account
	listErr  string
	last     pickRecord
	picks    atomic.Uint64
	excluded atomic.Uint64
	writes   atomic.Uint64
	failed   atomic.Bool
	tickMu   sync.Mutex
}

func newEngine(host authHost, fetcher quotaFetcher, cfg config) *engine {
	e := &engine{host: host, fetcher: fetcher, cfg: cfg, now: time.Now, accounts: make(map[string]*account)}
	if cfg.AccountsFile != "" {
		e.file = newReservesFile(cfg.AccountsFile)
		e.file.refresh()
	}
	return e
}

// config returns the configuration with the reserves in effect: those from accounts_file once
// it was read successfully, otherwise the inline accounts.
func (e *engine) config() config {
	cfg := e.cfg
	if e.file != nil {
		if accounts, ok := e.file.accounts(); ok {
			cfg.Accounts = accounts
		}
	}
	return cfg
}

// refreshReserves re-reads accounts_file when it changed and logs the outcome.
func (e *engine) refreshReserves() {
	if e.file == nil {
		return
	}
	before := e.file.lastError()
	if e.file.refresh() {
		e.host.Log("info", "quota-reset-router: reserves loaded from accounts_file", map[string]any{"path": e.file.path})
	} else if after := e.file.lastError(); after != "" && after != before {
		e.host.Log("warn", "quota-reset-router: "+after+"; keeping the last good reserves", map[string]any{"path": e.file.path})
	}
}

// inherit copies what a replaced engine learned, so a reconfigure does not
// drop header observations or reset poll backoff. The failed flag is not copied.
func (e *engine) inherit(old *engine) {
	old.mu.Lock()
	defer old.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, acc := range old.accounts {
		copied := *acc
		e.accounts[id] = &copied
	}
}

func (e *engine) run(ctx context.Context) {
	e.tick(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick runs one background pass: roster sync, due polls (one at a time), then priority writes.
func (e *engine) tick(ctx context.Context) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	e.refreshReserves()
	e.mirrorToConfig()
	if !e.syncRoster() {
		return
	}
	e.pollDue(ctx)
	if ctx.Err() == nil {
		e.applyPriorities()
	}
}

func eligibleEntry(entry pluginapi.HostAuthFileEntry) bool {
	provider := entry.Provider
	if provider == "" {
		provider = entry.Type
	}
	return strings.EqualFold(strings.TrimSpace(provider), "claude") && !entry.Disabled && !strings.EqualFold(entry.Status, "disabled") &&
		!entry.RuntimeOnly && entry.ID != "" && entry.AuthIndex != "" && entry.BaseURL == ""
}

// syncRoster refreshes the eligible Claude accounts. It returns false when the host list failed.
func (e *engine) syncRoster() bool {
	entries, err := e.host.List()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.listErr = "credential_list_failed"
		return false
	}
	e.listErr = ""
	seen := make(map[string]bool)
	for _, entry := range entries {
		if !eligibleEntry(entry) {
			continue
		}
		seen[entry.ID] = true
		acc := e.accounts[entry.ID]
		email := strings.TrimSpace(entry.Email)
		// A new auth index or a different account behind the same ID is a new credential:
		// the previous account's windows, timestamps and backoff do not apply to it.
		if acc == nil || acc.Index != entry.AuthIndex || (acc.Email != "" && !strings.EqualFold(acc.Email, email)) {
			acc = &account{ID: entry.ID, Index: entry.AuthIndex}
			e.accounts[entry.ID] = acc
		}
		acc.Email = email
		acc.Label = accountLabel(entry)
		acc.Priority = entry.Priority
		acc.LastRefresh = entry.LastRefresh
	}
	for id := range e.accounts {
		if !seen[id] {
			delete(e.accounts, id)
		}
	}
	return true
}

func accountLabel(entry pluginapi.HostAuthFileEntry) string {
	for _, value := range []string{entry.Email, entry.Label, entry.Name, entry.ID} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// pollDue polls, sequentially, every account whose latest observation is older than poll_interval.
func (e *engine) pollDue(ctx context.Context) {
	now := e.now()
	e.mu.Lock()
	var due []*account
	for _, acc := range e.accounts {
		if now.Sub(acc.ObservedAt) >= e.cfg.PollInterval && !now.Before(acc.NextPoll) {
			copy := *acc
			due = append(due, &copy)
		}
	}
	e.mu.Unlock()
	sort.Slice(due, func(i, j int) bool {
		if !due[i].ObservedAt.Equal(due[j].ObservedAt) {
			return due[i].ObservedAt.Before(due[j].ObservedAt)
		}
		return due[i].ID < due[j].ID
	})
	for _, acc := range due {
		if ctx.Err() != nil {
			return
		}
		// Headers may have refreshed the account while earlier polls ran.
		e.mu.Lock()
		current := e.accounts[acc.ID]
		stillDue := current != nil && current.Index == acc.Index && current.ObservedAt.Equal(acc.ObservedAt)
		e.mu.Unlock()
		if !stillDue {
			continue
		}
		snap, err := e.readQuota(ctx, acc.Index)
		if ctx.Err() != nil {
			return
		}
		now := e.now()
		e.mu.Lock()
		current = e.accounts[acc.ID]
		if current != nil && current.Index == acc.Index {
			if err != nil {
				recordPollFailure(current, err, now, e.cfg.PollInterval)
			} else if current.ObservedAt.Equal(acc.ObservedAt) {
				// A newer header observation wins over this poll result.
				current.FiveHour = windowFromQuota(snap.FiveHour, current.FiveHour)
				current.Weekly = windowFromQuota(&snap.Weekly, current.Weekly)
				current.Source, current.ObservedAt = "poll", now
				current.PollBackoff, current.NextPoll, current.PollError = 0, time.Time{}, ""
			}
		}
		e.mu.Unlock()
	}
}

// recordPollFailure doubles the backoff from poll_interval up to 6h and honors Retry-After.
func recordPollFailure(acc *account, err error, now time.Time, interval time.Duration) {
	if acc.PollBackoff == 0 {
		acc.PollBackoff = interval
	} else {
		acc.PollBackoff = min(acc.PollBackoff*2, maxPollBackoff)
	}
	delay := acc.PollBackoff
	acc.PollError = "quota_refresh_failed"
	var qerr *quotaError
	if errors.As(err, &qerr) {
		acc.PollError = qerr.code
		delay = min(max(delay, qerr.retryAfter), maxPollBackoff)
	} else if err != nil {
		acc.PollError = err.Error()
	}
	acc.NextPoll = now.Add(delay)
}

// windowFromQuota builds a window from a poll result. The poll has no status, so the previous
// status is kept only while the window (its reset time) is the same.
func windowFromQuota(q *quotaWindow, old window) window {
	if q == nil {
		return window{}
	}
	w := window{Utilization: q.Utilization, ResetsAt: q.ResetsAt}
	if sameReset(old.ResetsAt, q.ResetsAt) {
		w.Status = old.Status
	}
	return w
}

func sameReset(a, b *time.Time) bool {
	return a != nil && b != nil && a.Equal(*b)
}

func (e *engine) readQuota(ctx context.Context, index string) (quotaSnapshot, error) {
	_, raw, err := e.host.Get(index)
	if err != nil {
		return quotaSnapshot{}, errors.New("credential_read_failed")
	}
	var cred credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return quotaSnapshot{}, errors.New("credential_decode_failed")
	}
	if strings.TrimSpace(cred.Type) == "" {
		cred.Type = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.RequestTimeout)
	defer cancel()
	return e.fetcher.Fetch(ctx, cred)
}

const ratelimitPrefix = "Anthropic-Ratelimit-Unified-"

// observeUsage records the rate-limit headers of a completed request. It does no I/O.
func (e *engine) observeUsage(record pluginapi.UsageRecord) {
	if record.AuthID == "" || len(record.ResponseHeaders) == 0 {
		return
	}
	five, fiveOK := headerWindow(record.ResponseHeaders, "5h")
	weekly, weeklyOK := headerWindow(record.ResponseHeaders, "7d")
	if !fiveOK && !weeklyOK {
		return
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	acc := e.accounts[record.AuthID]
	if acc == nil {
		return
	}
	if fiveOK {
		acc.FiveHour = mergeWindow(acc.FiveHour, five)
	}
	if weeklyOK {
		acc.Weekly = mergeWindow(acc.Weekly, weekly)
	}
	acc.Source, acc.ObservedAt = "headers", now
}

// headerWindow parses the utilization, reset and status headers of one window.
func headerWindow(headers http.Header, name string) (window, bool) {
	var w window
	found := false
	if raw := headerValue(headers, ratelimitPrefix+name+"-Utilization"); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 && value < 1e6 {
			percent := value * 100
			w.Utilization, found = &percent, true
		}
	}
	if raw := headerValue(headers, ratelimitPrefix+name+"-Reset"); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			reset := time.Unix(seconds, 0).UTC()
			w.ResetsAt, found = &reset, true
		}
	}
	if raw := headerValue(headers, ratelimitPrefix+name+"-Status"); raw != "" {
		w.Status, found = strings.ToLower(raw), true
	}
	return w, found
}

// mergeWindow applies a partial header update. A new reset time starts a new window, so the
// utilization and status of the previous window are dropped.
func mergeWindow(old, update window) window {
	if update.ResetsAt != nil && old.ResetsAt != nil && !old.ResetsAt.Equal(*update.ResetsAt) {
		old = window{}
	}
	if update.Utilization != nil {
		old.Utilization = update.Utilization
	}
	if update.ResetsAt != nil {
		old.ResetsAt = update.ResetsAt
	}
	if update.Status != "" {
		old.Status = update.Status
	}
	return old
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

type planRow struct {
	ID      string
	Index   string
	Label   string
	Current int
	Planned int
}

// planPriorities ranks the accounts: available ones first, then those at reserve or exhausted.
// Inside each group the earliest known future weekly reset wins, then unknown resets, then the
// current priority (descending) and ID. It also reports whether any weekly reset is known.
func planPriorities(accounts map[string]*account, cfg config, now time.Time) ([]planRow, bool) {
	type ranked struct {
		acc     *account
		blocked bool
	}
	rows := make([]ranked, 0, len(accounts))
	known := false
	for _, acc := range accounts {
		rows = append(rows, ranked{acc: acc, blocked: acc.atReserve(cfg, now) || acc.exhausted(now)})
		known = known || acc.Weekly.live(now)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.blocked != b.blocked {
			return !a.blocked
		}
		aLive, bLive := a.acc.Weekly.live(now), b.acc.Weekly.live(now)
		if aLive != bLive {
			return aLive
		}
		if aLive && !a.acc.Weekly.ResetsAt.Equal(*b.acc.Weekly.ResetsAt) {
			return a.acc.Weekly.ResetsAt.Before(*b.acc.Weekly.ResetsAt)
		}
		if a.acc.Priority != b.acc.Priority {
			return a.acc.Priority > b.acc.Priority
		}
		return a.acc.ID < b.acc.ID
	})
	plan := make([]planRow, len(rows))
	for rank, row := range rows {
		plan[rank] = planRow{ID: row.acc.ID, Index: row.acc.Index, Label: row.acc.Label, Current: row.acc.Priority, Planned: (len(rows) - rank) * priorityStep}
	}
	return plan, known
}

// applyPriorities writes the planned priorities in active mode. With no known weekly reset it
// leaves every priority untouched.
func (e *engine) applyPriorities() {
	if e.cfg.Mode != "active" || e.failed.Load() {
		return
	}
	e.mu.Lock()
	plan, known := planPriorities(e.accounts, e.config(), e.now())
	e.mu.Unlock()
	if !known {
		return
	}
	// Rows are in rank order, so the top account is promoted first. A hard failure stops the
	// pass to avoid a half-applied order; the next tick retries. A deferred or skipped row does not.
	for _, row := range plan {
		if row.Planned == row.Current {
			continue
		}
		e.mu.Lock()
		lastRefresh := time.Time{}
		if acc := e.accounts[row.ID]; acc != nil {
			lastRefresh = acc.LastRefresh
		}
		e.mu.Unlock()
		result := e.writePriority(row, lastRefresh)
		e.mu.Lock()
		if acc := e.accounts[row.ID]; acc != nil && acc.Index == row.Index {
			acc.LastWrite = result
			if result == "ok" {
				acc.Priority = row.Planned
			}
		}
		e.mu.Unlock()
		// Only a failed save or encode stops the pass. A credential that cannot be read or
		// parsed is a problem of that file alone, so the accounts below it are still written.
		if result == "save_failed" || result == "encode_failed" {
			return
		}
	}
}

// writePriority re-reads the credential, changes only its top-level priority and saves it
// under the same file name. It defers while CPA may be refreshing the token and skips the
// save when the file changed between two reads.
func (e *engine) writePriority(row planRow, lastRefresh time.Time) string {
	// The host saves by base name into the top of the auth directory, so a credential in a
	// subdirectory (its ID is the relative path) would be duplicated instead of updated.
	if strings.ContainsAny(row.ID, `/\`) {
		return "skipped_unsupported_path"
	}
	now := e.now()
	if !lastRefresh.IsZero() && now.Sub(lastRefresh) < recentRefreshGuard {
		return "deferred_token_refresh"
	}
	name, raw, err := e.host.Get(row.Index)
	if err != nil || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return "read_failed"
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return "invalid_json"
	}
	if expiry, ok := doc["expired"]; ok && !tokenFarFromRefresh(expiry, now) {
		return "deferred_token_refresh"
	}
	doc["priority"] = json.RawMessage(strconv.Itoa(row.Planned))
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(doc); err != nil {
		return "encode_failed"
	}
	if again, raw2, err := e.host.Get(row.Index); err != nil || again != name || !bytes.Equal(raw, raw2) {
		return "deferred_changed"
	}
	if err := e.host.Save(name, bytes.TrimSpace(out.Bytes())); err != nil {
		e.host.Log("warn", "quota-reset-router: priority write failed", map[string]any{"account": row.Label, "priority": row.Planned})
		return "save_failed"
	}
	e.writes.Add(1)
	e.host.Log("info", "quota-reset-router: priority updated", map[string]any{"account": row.Label, "old_priority": row.Current, "new_priority": row.Planned})
	return "ok"
}

// tokenFarFromRefresh reports whether the credential's "expired" timestamp is far enough away
// that CPA will not refresh the token during the write. An unreadable value is not far.
func tokenFarFromRefresh(raw json.RawMessage, now time.Time) bool {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	return err == nil && expiry.Sub(now) > tokenRefreshGuard
}

func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	if strings.EqualFold(c.Status, "disabled") {
		return true
	}
	disabled, _ := c.Metadata["disabled"].(bool)
	return disabled
}

// reserveExclusions lists the candidates at reserve, but only when at least one other
// candidate is open (not disabled, not at reserve, not known to be exhausted): a reserve
// never blocks a request.
func reserveExclusions(candidates []pluginapi.SchedulerAuthCandidate, accounts map[string]*account, cfg config, now time.Time) []string {
	var atReserve []string
	open := false
	for _, c := range candidates {
		if c.ID == "" {
			continue
		}
		acc := accounts[c.ID]
		if acc != nil && acc.atReserve(cfg, now) {
			atReserve = append(atReserve, c.ID)
			continue
		}
		if !candidateDisabled(c) && (acc == nil || !acc.exhausted(now)) {
			open = true
		}
	}
	if !open {
		return nil
	}
	return atReserve
}

// pick never selects or rejects: it only returns reserve exclusions in active mode and leaves
// the choice to CPA's native selector (fill-first, session affinity, retries, cooldowns).
func (e *engine) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	e.picks.Add(1)
	if e.failed.Load() {
		return pluginapi.SchedulerPickResponse{}
	}
	now := e.now()
	active := e.cfg.Mode == "active"
	e.mu.Lock()
	var excluded []string
	// A failed roster read leaves stale state behind, so it disables exclusions (fail-open).
	if e.listErr == "" {
		excluded = reserveExclusions(req.Candidates, e.accounts, e.config(), now)
	}
	e.last = pickRecord{At: now, Candidates: len(req.Candidates), WouldExclude: excluded, Applied: active && len(excluded) > 0}
	e.mu.Unlock()
	if !active || len(excluded) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	e.excluded.Add(1)
	return pluginapi.SchedulerPickResponse{ExcludeCandidates: excluded}
}

type accountReport struct {
	ID              string     `json:"id"`
	Email           string     `json:"email,omitempty"`
	Label           string     `json:"label,omitempty"`
	FiveHour        window     `json:"five_hour"`
	Weekly          window     `json:"seven_day"`
	Source          string     `json:"source,omitempty"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	AgeSeconds      *int64     `json:"age_seconds,omitempty"`
	FiveHourReserve *float64   `json:"five_hour_reserve,omitempty"`
	WeeklyReserve   *float64   `json:"weekly_reserve,omitempty"`
	AtReserve       bool       `json:"at_reserve"`
	Exhausted       bool       `json:"exhausted"`
	Priority        int        `json:"priority"`
	PlannedPriority int        `json:"planned_priority"`
	LastWrite       string     `json:"last_write,omitempty"`
	PollBackoff     string     `json:"poll_backoff,omitempty"`
	NextPoll        *time.Time `json:"next_poll,omitempty"`
	PollError       string     `json:"poll_error,omitempty"`
}

func (e *engine) status() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	cfg := e.config()
	plan, known := planPriorities(e.accounts, cfg, now)
	planned := make(map[string]int, len(plan))
	for _, row := range plan {
		planned[row.ID] = row.Planned
	}
	accounts := make(map[string]accountReport, len(e.accounts))
	for id, acc := range e.accounts {
		reserve := cfg.reserveFor(acc.ID, acc.Email)
		report := accountReport{
			ID: id, Email: acc.Email, Label: acc.Label, FiveHour: acc.FiveHour, Weekly: acc.Weekly, Source: acc.Source,
			FiveHourReserve: reserve.FiveHourReserve, WeeklyReserve: reserve.WeeklyReserve,
			AtReserve: acc.atReserve(cfg, now), Exhausted: acc.exhausted(now),
			Priority: acc.Priority, PlannedPriority: planned[id], LastWrite: acc.LastWrite, PollError: acc.PollError,
		}
		if !acc.ObservedAt.IsZero() {
			observed := acc.ObservedAt
			age := int64(now.Sub(observed) / time.Second)
			report.ObservedAt, report.AgeSeconds = &observed, &age
		}
		if acc.PollBackoff > 0 {
			report.PollBackoff = acc.PollBackoff.String()
			next := acc.NextPoll
			report.NextPoll = &next
		}
		accounts[id] = report
	}
	return struct {
		Version        string                    `json:"version"`
		Policy         string                    `json:"selection_policy"`
		Mode           string                    `json:"mode"`
		PollInterval   string                    `json:"poll_interval"`
		RequestTimeout string                    `json:"request_timeout"`
		WeeklyKnown    bool                      `json:"weekly_reset_known"`
		Accounts       map[string]accountReport  `json:"accounts"`
		Configured     map[string]accountReserve `json:"configured_accounts,omitempty"`
		ListError      string                    `json:"list_error,omitempty"`
		LastPick       pickRecord                `json:"last_pick"`
		Picks          uint64                    `json:"picks"`
		Excluded       uint64                    `json:"excluded_picks"`
		Writes         uint64                    `json:"priority_writes"`
		WorkerFailed   bool                      `json:"worker_failed"`
		AccountsFile   string                    `json:"accounts_file,omitempty"`
		FileError      string                    `json:"accounts_file_error,omitempty"`
		FileLoadedAt   *time.Time                `json:"accounts_file_loaded_at,omitempty"`
	}{pluginVersion, "priority_steering_with_reserve_exclusion", cfg.Mode, cfg.PollInterval.String(), cfg.RequestTimeout.String(), known, accounts, cfg.Accounts, e.listErr, e.last, e.picks.Load(), e.excluded.Load(), e.writes.Load(), e.failed.Load(), fileField(e.file, (*reservesFile).pathValue), fileField(e.file, (*reservesFile).lastError), fileLoadedAt(e.file)}
}

func fileField(f *reservesFile, get func(*reservesFile) string) string {
	if f == nil {
		return ""
	}
	return get(f)
}

func fileLoadedAt(f *reservesFile) *time.Time {
	if f == nil {
		return nil
	}
	return f.lastLoad()
}
