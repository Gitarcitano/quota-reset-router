package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

type savedFile struct {
	Name string
	JSON json.RawMessage
}

type fakeHost struct {
	mu      sync.Mutex
	entries []pluginapi.HostAuthFileEntry
	files   map[string]string // auth index -> raw JSON
	names   map[string]string // auth index -> file name
	listErr error
	getErr  map[string]error
	saveErr error
	// saveErrFor fails saves of one file name; afterGet runs under the lock after each read.
	saveErrFor map[string]error
	afterGet   func(h *fakeHost, index string)
	saves      []savedFile
	logs       []string
	calls      int
}

func (h *fakeHost) List() ([]pluginapi.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return append([]pluginapi.HostAuthFileEntry(nil), h.entries...), h.listErr
}

func (h *fakeHost) Get(index string) (string, json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if err := h.getErr[index]; err != nil {
		return "", nil, err
	}
	raw, ok := h.files[index]
	if !ok {
		return "", nil, errors.New("missing")
	}
	name := h.names[index]
	if name == "" {
		name = index + ".json"
	}
	if h.afterGet != nil {
		h.afterGet(h, index)
	}
	return name, json.RawMessage(raw), nil
}

func (h *fakeHost) Save(name string, raw json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.saveErr != nil {
		return h.saveErr
	}
	if err := h.saveErrFor[name]; err != nil {
		return err
	}
	h.saves = append(h.saves, savedFile{Name: name, JSON: append(json.RawMessage(nil), raw...)})
	// Like CPA, the saved file replaces the auth record, so the next list shows the new priority.
	var doc struct {
		Priority int `json:"priority"`
	}
	_ = json.Unmarshal(raw, &doc)
	for i := range h.entries {
		if h.entries[i].Name == name {
			h.entries[i].Priority = doc.Priority
		}
	}
	return nil
}

func (h *fakeHost) Log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	encoded, _ := json.Marshal(fields)
	h.logs = append(h.logs, level+" "+message+" "+string(encoded))
}

func (h *fakeHost) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

type fetchFunc func(context.Context, credential) (quotaSnapshot, error)

func (f fetchFunc) Fetch(ctx context.Context, c credential) (quotaSnapshot, error) { return f(ctx, c) }

func claudeEntry(id, email string, priority int) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{ID: id, AuthIndex: "idx-" + id, Name: id + ".json", Provider: "claude", Type: "claude", Status: "active", Email: email, Priority: priority}
}

func claudeFile(token string) string {
	return `{"type":"claude","access_token":"` + token + `","refresh_token":"REFRESH_SECRET","email":"x@test","note":"<a&b>","nested":{"k":[1,2,3]}}`
}

// testEngine builds an active engine over three Claude accounts (a, b, c) and a Codex account.
// The fetcher returns a weekly reset of 1 + position days, by access token.
func testEngine(t *testing.T) (*engine, *fakeHost, *time.Time, *int) {
	t.Helper()
	host := &fakeHost{files: map[string]string{}, names: map[string]string{}}
	for _, id := range []string{"a", "b", "c"} {
		host.entries = append(host.entries, claudeEntry(id, id+"@example.com", 0))
		host.files["idx-"+id] = claudeFile("SECRET_TOKEN_" + id)
		host.names["idx-"+id] = id + ".json"
	}
	host.entries = append(host.entries, pluginapi.HostAuthFileEntry{ID: "codex", AuthIndex: "idx-codex", Provider: "codex", Type: "codex", Status: "active"})
	host.files["idx-codex"] = `{"type":"codex","access_token":"SECRET_TOKEN_codex"}`
	now := testNow
	fetches := 0
	fetcher := fetchFunc(func(_ context.Context, c credential) (quotaSnapshot, error) {
		fetches++
		days := map[string]int{"SECRET_TOKEN_a": 3, "SECRET_TOKEN_b": 1, "SECRET_TOKEN_c": 2}[c.AccessToken]
		return quotaSnapshot{Weekly: quotaWindow{Utilization: ptr(20.0), ResetsAt: ptr(now.Add(time.Duration(days) * 24 * time.Hour))}, FiveHour: &quotaWindow{Utilization: ptr(5.0), ResetsAt: ptr(now.Add(time.Hour))}}, nil
	})
	cfg, err := decodeConfig([]byte("mode: active"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(host, fetcher, cfg)
	e.now = func() time.Time { return now }
	return e, host, &now, &fetches
}

func testRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Providers: []string{"claude"}, Model: "claude-sonnet-4-6"}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Status: "active"})
	}
	return req
}

func setWindows(e *engine, id string, five, weekly window) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.accounts[id].FiveHour, e.accounts[id].Weekly = five, weekly
}

func win(util float64, reset time.Duration) window {
	return window{Utilization: ptr(util), ResetsAt: ptr(testNow.Add(reset))}
}

func reserves(five, weekly float64) accountReserve {
	return accountReserve{FiveHourReserve: ptr(five), WeeklyReserve: ptr(weekly)}
}

func assertNeverPicks(t *testing.T, got pluginapi.SchedulerPickResponse) {
	t.Helper()
	if got.AuthID != "" || got.Handled || got.Reject || got.RejectReason != "" || got.RejectCode != "" || got.DelegateBuiltin != "" {
		t.Fatalf("plugin made a routing decision: %+v", got)
	}
}

func TestPickNeverSelectsRejectsOrHandles(t *testing.T) {
	for _, mode := range []string{"shadow", "active"} {
		for _, state := range []string{"unknown", "all_exhausted", "all_at_reserve", "one_at_reserve", "failed", "list_error"} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				e, host, _, _ := testEngine(t)
				e.cfg.Mode = mode
				e.cfg.Accounts = map[string]accountReserve{"a": reserves(10, 10), "b": reserves(10, 10), "c": reserves(10, 10)}
				e.syncRoster()
				switch state {
				case "all_exhausted":
					for _, id := range []string{"a", "b", "c"} {
						setWindows(e, id, window{Status: "rejected", ResetsAt: ptr(testNow.Add(time.Hour))}, win(100, 24*time.Hour))
					}
				case "all_at_reserve":
					for _, id := range []string{"a", "b", "c"} {
						setWindows(e, id, win(95, time.Hour), window{})
					}
				case "one_at_reserve":
					setWindows(e, "a", win(95, time.Hour), window{})
				case "failed":
					setWindows(e, "a", win(95, time.Hour), window{})
					e.failed.Store(true)
				case "list_error":
					setWindows(e, "a", win(95, time.Hour), window{})
					host.listErr = errors.New("boom")
					e.syncRoster()
				}
				got := e.pick(testRequest("a", "b", "c"))
				assertNeverPicks(t, got)
				if want := mode == "active" && state == "one_at_reserve"; want != (len(got.ExcludeCandidates) > 0) {
					t.Fatalf("exclusions = %v", got.ExcludeCandidates)
				}
			})
		}
	}
}

func TestPickPanicFailsOpen(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.syncRoster()
	setWindows(e, "a", win(99, time.Hour), window{})
	e.cfg.Accounts = map[string]accountReserve{"a": reserves(10, 0)}
	e.now = func() time.Time { panic("boom") }
	p := &pluginRuntime{}
	p.active.Store(e)
	raw, _ := json.Marshal(testRequest("a", "b"))
	got := p.pick(raw)
	assertNeverPicks(t, got)
	if len(got.ExcludeCandidates) != 0 || !e.failed.Load() {
		t.Fatalf("panic must fail open and mark the worker failed: %+v", got)
	}
	e.now = func() time.Time { return testNow }
	if got := p.pick(raw); len(got.ExcludeCandidates) != 0 {
		t.Fatal("failed worker still excludes")
	}
}

func TestReserveExclusionNeedsAnOpenCandidate(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"a": reserves(10, 0), "B@Example.com": reserves(0, 15)}
	e.syncRoster()
	setWindows(e, "a", win(90, time.Hour), window{})
	got := e.pick(testRequest("a", "b"))
	if !reflect.DeepEqual(got.ExcludeCandidates, []string{"a"}) {
		t.Fatalf("a at its 5h reserve must be excluded while b is open: %+v", got)
	}
	setWindows(e, "b", window{}, win(85, 24*time.Hour))
	if got := e.pick(testRequest("a", "b")); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("all candidates at reserve must not be excluded: %+v", got)
	}
	disabled := testRequest("a", "c")
	disabled.Candidates[1].Status = "disabled"
	if got := e.pick(disabled); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("a disabled candidate is not an open alternative: %+v", got)
	}
	disabled.Candidates[1] = pluginapi.SchedulerAuthCandidate{ID: "c", Status: "active", Metadata: map[string]any{"disabled": true}}
	if got := e.pick(disabled); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("metadata disabled candidate is not an open alternative: %+v", got)
	}
	// An unknown candidate (other provider, not tracked) still counts as open.
	if got := e.pick(testRequest("a", "unknown")); !reflect.DeepEqual(got.ExcludeCandidates, []string{"a"}) {
		t.Fatalf("unexpected exclusions: %+v", got)
	}
	setWindows(e, "c", window{}, win(100, 24*time.Hour))
	if got := e.pick(testRequest("a", "c")); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("an exhausted candidate is not an open alternative: %+v", got)
	}
	setWindows(e, "c", window{}, window{})
	e.cfg.Mode = "shadow"
	if got := e.pick(testRequest("a", "c")); len(got.ExcludeCandidates) != 0 {
		t.Fatal("shadow mode excluded a candidate")
	}
	if !reflect.DeepEqual(e.last.WouldExclude, []string{"a"}) || e.last.Applied {
		t.Fatalf("shadow decision not recorded: %+v", e.last)
	}
}

func TestAtReserveMath(t *testing.T) {
	cases := []struct {
		name    string
		reserve accountReserve
		acc     account
		want    bool
	}{
		{"5h below threshold", reserves(10, 0), account{FiveHour: win(89.9, time.Hour)}, false},
		{"5h at threshold", reserves(10, 0), account{FiveHour: win(90, time.Hour)}, true},
		{"weekly at threshold", reserves(0, 15), account{Weekly: win(85, 48*time.Hour)}, true},
		{"weekly below threshold", reserves(0, 15), account{Weekly: win(84, 48*time.Hour)}, false},
		{"past reset", reserves(10, 15), account{FiveHour: win(99, -time.Minute), Weekly: win(99, -time.Minute)}, false},
		{"unknown utilization", reserves(10, 15), account{FiveHour: window{ResetsAt: ptr(testNow.Add(time.Hour))}}, false},
		{"unknown reset", reserves(10, 15), account{FiveHour: window{Utilization: ptr(99.0)}}, false},
		{"no reserve", accountReserve{}, account{FiveHour: win(100, time.Hour), Weekly: win(100, time.Hour)}, false},
		{"zero reserve below 100", reserves(0, 0), account{FiveHour: win(99, time.Hour)}, false},
		{"zero reserve at 100", reserves(0, 0), account{FiveHour: win(100, time.Hour)}, true},
		{"only weekly configured", accountReserve{WeeklyReserve: ptr(15.0)}, account{FiveHour: win(99, time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.acc.ID = "id"
			cfg := config{Accounts: map[string]accountReserve{"id": tc.reserve}}
			if got := tc.acc.atReserve(cfg, testNow); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	byEmail := account{ID: "other", Email: "User@Example.com", FiveHour: win(95, time.Hour)}
	if !byEmail.atReserve(config{Accounts: map[string]accountReserve{"user@example.COM": reserves(10, 0)}}, testNow) {
		t.Fatal("email keys must match case-insensitively")
	}
}

func TestPlanPrioritiesOrder(t *testing.T) {
	cfg := config{Accounts: map[string]accountReserve{"reserve": reserves(10, 0)}}
	accounts := map[string]*account{
		"late":      {ID: "late", Weekly: win(10, 72*time.Hour)},
		"early":     {ID: "early", Weekly: win(10, 24*time.Hour)},
		"unknown-z": {ID: "unknown-z", Priority: 0},
		"unknown-a": {ID: "unknown-a", Priority: 0},
		"high-prio": {ID: "high-prio", Priority: 5, Weekly: win(10, -time.Hour)},
		"reserve":   {ID: "reserve", FiveHour: win(95, time.Hour), Weekly: win(10, time.Hour)},
		"exhausted": {ID: "exhausted", FiveHour: window{Status: "rejected", ResetsAt: ptr(testNow.Add(time.Hour))}, Weekly: win(10, 2*time.Hour)},
		"spent":     {ID: "spent", Weekly: win(100, 3*time.Hour)},
	}
	plan, known := planPriorities(accounts, cfg, testNow)
	if !known {
		t.Fatal("known weekly reset not detected")
	}
	var order []string
	for i, row := range plan {
		order = append(order, row.ID)
		if row.Planned != (len(plan)-i)*10 {
			t.Fatalf("row %d planned %d", i, row.Planned)
		}
	}
	want := []string{"early", "late", "high-prio", "unknown-a", "unknown-z", "reserve", "exhausted", "spent"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	_, known = planPriorities(map[string]*account{"x": {ID: "x", Weekly: win(10, -time.Hour)}}, cfg, testNow)
	if known {
		t.Fatal("a past reset is not a known reset")
	}
}

func decodeSaved(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved invalid JSON: %v", err)
	}
	return doc
}

func TestTickRewritesOnlyChangedClaudePrioritiesAndKeepsOtherFields(t *testing.T) {
	e, host, _, _ := testEngine(t)
	// b resets first (1 day), then c (2 days), then a (3 days). c already has its planned priority.
	host.entries[2].Priority = 20
	e.tick(context.Background())
	got := map[string]savedFile{}
	for _, save := range host.saves {
		got[save.Name] = save
	}
	if len(host.saves) != 2 || got["a.json"].Name == "" || got["b.json"].Name == "" {
		t.Fatalf("expected writes for a and b only, got %+v", host.saves)
	}
	for name, priority := range map[string]float64{"a.json": 10, "b.json": 30} {
		doc := decodeSaved(t, got[name].JSON)
		if doc["priority"] != priority {
			t.Fatalf("%s priority %v, want %v", name, doc["priority"], priority)
		}
		delete(doc, "priority")
		original := decodeSaved(t, json.RawMessage(claudeFile("SECRET_TOKEN_"+strings.TrimSuffix(name, ".json"))))
		if !reflect.DeepEqual(doc, original) {
			t.Fatalf("%s lost fields: %v vs %v", name, doc, original)
		}
		if !strings.Contains(string(got[name].JSON), "<a&b>") {
			t.Fatal("HTML characters were escaped")
		}
	}
	logs := strings.Join(host.logs, "\n")
	if strings.Contains(logs, "SECRET") || !strings.Contains(logs, "b@example.com") || !strings.Contains(logs, `"new_priority":30`) || !strings.Contains(logs, `"old_priority":0`) {
		t.Fatalf("bad write logs: %s", logs)
	}
	if _, ok := e.accounts["codex"]; ok {
		t.Fatal("codex account tracked")
	}
	if e.accounts["b"].LastWrite != "ok" || e.accounts["b"].Priority != 30 {
		t.Fatalf("write result not recorded: %+v", e.accounts["b"])
	}
}

func TestPriorityWriteGuards(t *testing.T) {
	setup := func(t *testing.T) (*engine, *fakeHost) {
		e, host, _, _ := testEngine(t)
		e.syncRoster()
		setWindows(e, "a", window{}, win(10, 24*time.Hour))
		return e, host
	}
	t.Run("shadow", func(t *testing.T) {
		e, host := setup(t)
		e.cfg.Mode = "shadow"
		e.applyPriorities()
		if len(host.saves) != 0 {
			t.Fatal("shadow mode wrote")
		}
		if status, _ := json.Marshal(e.status()); !strings.Contains(string(status), `"planned_priority":30`) {
			t.Fatalf("shadow plan not visible: %s", status)
		}
	})
	t.Run("no known reset", func(t *testing.T) {
		e, host := setup(t)
		setWindows(e, "a", window{}, win(10, -time.Hour))
		e.applyPriorities()
		if len(host.saves) != 0 {
			t.Fatal("wrote without a known weekly reset")
		}
	})
	t.Run("failed worker", func(t *testing.T) {
		e, host := setup(t)
		e.failed.Store(true)
		e.applyPriorities()
		if len(host.saves) != 0 {
			t.Fatal("failed worker wrote")
		}
	})
	for _, tc := range []struct {
		name, want string
		change     func(*fakeHost)
	}{
		{"read failure", "read_failed", func(h *fakeHost) { h.getErr = map[string]error{"idx-a": errors.New("x")} }},
		{"invalid JSON", "invalid_json", func(h *fakeHost) { h.files["idx-a"] = `{"priority":` }},
		{"non-object JSON", "invalid_json", func(h *fakeHost) { h.files["idx-a"] = `[1]` }},
		{"name is not a json file", "read_failed", func(h *fakeHost) { h.names["idx-a"] = "a" }},
		{"save failure", "save_failed", func(h *fakeHost) { h.saveErr = errors.New("disk") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, host := setup(t)
			host.mu.Lock()
			tc.change(host)
			host.mu.Unlock()
			e.applyPriorities()
			for _, save := range host.saves {
				if save.Name == "a.json" {
					t.Fatal("unsafe write happened")
				}
			}
			if e.accounts["a"].LastWrite != tc.want || e.accounts["a"].Priority != 0 {
				t.Fatalf("got %+v", e.accounts["a"])
			}
		})
	}
}

func TestObserveUsageHeaders(t *testing.T) {
	e, host, now, _ := testEngine(t)
	e.syncRoster()
	before := host.callCount()
	*now = testNow.Add(time.Minute)
	reset5 := testNow.Add(2 * time.Hour).Unix()
	reset7 := testNow.Add(50 * time.Hour).Unix()
	e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{
		"anthropic-ratelimit-unified-5h-utilization": {"0.69"},
		"ANTHROPIC-RATELIMIT-UNIFIED-5H-RESET":       {strconv.FormatInt(reset5, 10)},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"Allowed"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(reset7, 10)},
	}})
	acc := e.accounts["a"]
	if acc.FiveHour.Utilization == nil || *acc.FiveHour.Utilization < 68.99 || *acc.FiveHour.Utilization > 69.01 {
		t.Fatalf("utilization must be converted to percent: %+v", acc.FiveHour)
	}
	if acc.FiveHour.ResetsAt.Unix() != reset5 || acc.FiveHour.Status != "allowed" || acc.Weekly.ResetsAt.Unix() != reset7 || acc.Weekly.Utilization != nil {
		t.Fatalf("bad windows: %+v %+v", acc.FiveHour, acc.Weekly)
	}
	if acc.Source != "headers" || !acc.ObservedAt.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("bad observation: %+v", acc)
	}
	// A partial update keeps the other fields.
	e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.9"}}})
	if *acc.FiveHour.Utilization < 89.99 || *acc.FiveHour.Utilization > 90.01 || acc.FiveHour.ResetsAt.Unix() != reset5 || acc.FiveHour.Status != "allowed" {
		t.Fatalf("partial update lost data: %+v", acc.FiveHour)
	}
	// Unknown accounts and unrelated headers are ignored.
	e.observeUsage(pluginapi.UsageRecord{AuthID: "codex", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"}}})
	e.observeUsage(pluginapi.UsageRecord{AuthID: "b", ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"bad"}}})
	if _, ok := e.accounts["codex"]; ok || e.accounts["b"].Source != "" {
		t.Fatal("ignored usage changed state")
	}
	if host.callCount() != before {
		t.Fatal("usage.handle made host calls")
	}
}

func TestPollOnlyStaleAccountsAndBackoff(t *testing.T) {
	e, host, now, fetches := testEngine(t)
	e.syncRoster()
	e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"}}})
	*now = testNow.Add(10 * time.Minute)
	e.tick(context.Background())
	if *fetches != 2 || e.accounts["a"].Source != "headers" || e.accounts["b"].Source != "poll" {
		t.Fatalf("fresh account was polled or stale one skipped: fetches=%d", *fetches)
	}
	if e.accounts["b"].Weekly.ResetsAt == nil || !e.accounts["b"].ObservedAt.Equal(*now) {
		t.Fatal("poll did not update the observation")
	}
	failures := 0
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		failures++
		return quotaSnapshot{}, &quotaError{code: "quota_http_429", retryAfter: 2 * time.Hour}
	})
	*now = testNow.Add(time.Hour)
	before := e.accounts["b"].Weekly
	plan := plannedOrder(e, *now)
	e.tick(context.Background())
	if failures != 3 {
		t.Fatalf("expected one poll per stale account, got %d", failures)
	}
	b := e.accounts["b"]
	if b.PollBackoff != 30*time.Minute || !b.NextPoll.Equal(now.Add(2*time.Hour)) || b.PollError != "quota_http_429" {
		t.Fatalf("Retry-After not honored: %+v", b)
	}
	if !reflect.DeepEqual(b.Weekly, before) {
		t.Fatal("poll failure changed known windows")
	}
	if after := plannedOrder(e, *now); !reflect.DeepEqual(plan, after) {
		t.Fatal("poll failure changed the routing plan")
	}
	e.tick(context.Background())
	if failures != 3 {
		t.Fatal("polled during backoff")
	}
	// Backoff doubles from poll_interval up to 6h.
	acc := &account{}
	var got []time.Duration
	for i := 0; i < 6; i++ {
		recordPollFailure(acc, errors.New("x"), testNow, 30*time.Minute)
		got = append(got, acc.PollBackoff)
	}
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backoff %v, want %v", got, want)
	}
	// Success resets the backoff.
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		return quotaSnapshot{Weekly: quotaWindow{Utilization: ptr(1.0), ResetsAt: ptr(testNow.Add(24 * time.Hour))}}, nil
	})
	*now = testNow.Add(4 * time.Hour)
	e.tick(context.Background())
	if b := e.accounts["b"]; b.PollBackoff != 0 || !b.NextPoll.IsZero() || b.PollError != "" {
		t.Fatalf("success did not reset backoff: %+v", b)
	}
	if strings.Contains(strings.Join(host.logs, ""), "SECRET") {
		t.Fatal("token logged")
	}
}

func plannedOrder(e *engine, now time.Time) []string {
	plan, _ := planPriorities(e.accounts, e.cfg, now)
	var out []string
	for _, row := range plan {
		out = append(out, row.ID+"="+strconv.Itoa(row.Planned))
	}
	return out
}

func TestPollsRunOneAtATime(t *testing.T) {
	e, _, _, _ := testEngine(t)
	var inFlight, maxInFlight atomic.Int32
	e.fetcher = fetchFunc(func(context.Context, credential) (quotaSnapshot, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		// Hold the poll open so concurrent ticks would overlap without the tick lock.
		time.Sleep(2 * time.Millisecond)
		return quotaSnapshot{}, errors.New("x")
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.tick(context.Background()) }()
	}
	wg.Wait()
	if maxInFlight.Load() != 1 {
		t.Fatalf("max polls in flight %d", maxInFlight.Load())
	}
}

func TestRosterFiltersAndResetsOnIndexChange(t *testing.T) {
	e, host, _, _ := testEngine(t)
	host.entries = append(host.entries,
		pluginapi.HostAuthFileEntry{ID: "off", AuthIndex: "i1", Provider: "claude", Disabled: true},
		pluginapi.HostAuthFileEntry{ID: "off2", AuthIndex: "i2", Provider: "claude", Status: "disabled"},
		pluginapi.HostAuthFileEntry{ID: "runtime", AuthIndex: "i3", Provider: "claude", RuntimeOnly: true},
		pluginapi.HostAuthFileEntry{ID: "custom", AuthIndex: "i4", Provider: "claude", BaseURL: "https://x.test"},
		pluginapi.HostAuthFileEntry{ID: "noindex", Provider: "claude"},
		pluginapi.HostAuthFileEntry{ID: "typed", AuthIndex: "i5", Type: "claude"},
	)
	e.syncRoster()
	for _, id := range []string{"off", "off2", "runtime", "custom", "noindex", "codex"} {
		if _, ok := e.accounts[id]; ok {
			t.Fatalf("%s must not be tracked", id)
		}
	}
	if _, ok := e.accounts["typed"]; !ok {
		t.Fatal("type-only claude entry missing")
	}
	setWindows(e, "a", win(50, time.Hour), window{})
	host.entries[0].AuthIndex = "idx-new"
	e.syncRoster()
	if e.accounts["a"].FiveHour.Utilization != nil {
		t.Fatal("observations survived a credential swap")
	}
}

func TestReconfigureKeepsObservations(t *testing.T) {
	old, host, _, _ := testEngine(t)
	old.syncRoster()
	setWindows(old, "a", win(50, time.Hour), win(20, 48*time.Hour))
	old.failed.Store(true)
	cfg, _ := decodeConfig([]byte("mode: shadow"))
	next := newEngine(host, old.fetcher, cfg)
	next.inherit(old)
	if next.failed.Load() || next.accounts["a"] == old.accounts["a"] {
		t.Fatal("inherit must copy state and start healthy")
	}
	if u := next.accounts["a"].FiveHour.Utilization; u == nil || *u != 50 || next.accounts["a"].Weekly.ResetsAt == nil {
		t.Fatalf("observations lost on reconfigure: %+v", next.accounts["a"])
	}
	setWindows(old, "a", window{}, window{})
	if next.accounts["a"].FiveHour.Utilization == nil {
		t.Fatal("inherited state aliases the old engine")
	}
}

func TestStatusReportWithoutTokens(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"a@example.com": reserves(10, 15)}
	e.tick(context.Background())
	e.pick(testRequest("a", "b"))
	raw, _ := json.Marshal(e.status())
	body := string(raw)
	for _, want := range []string{`"version":"0.4.0"`, `"mode":"active"`, `"email":"a@example.com"`, `"five_hour_reserve":10`, `"weekly_reserve":15`, `"at_reserve":false`, `"planned_priority":`, `"last_write":"ok"`, `"source":"poll"`, `"age_seconds":0`, `"seven_day":`, `"configured_accounts"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "SECRET") {
		t.Fatal("secret leaked")
	}
}

func TestConcurrentTicksPicksUsageAndStatus(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"a": reserves(10, 10)}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.tick(context.Background())
				e.pick(testRequest("a", "b", "c"))
				e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.95"}}})
				_, _ = json.Marshal(e.status())
			}
		}()
	}
	wg.Wait()
}

func BenchmarkPick(b *testing.B) {
	host := &fakeHost{}
	cfg := config{Mode: "active", PollInterval: 30 * time.Minute, RequestTimeout: time.Second, Accounts: map[string]accountReserve{}}
	e := newEngine(host, nil, cfg)
	var ids []string
	for i := 0; i < 50; i++ {
		id := "acc-" + strconv.Itoa(i)
		ids = append(ids, id)
		e.accounts[id] = &account{ID: id, FiveHour: window{Utilization: ptr(float64(i * 2)), ResetsAt: ptr(time.Now().Add(time.Hour))}}
		cfg.Accounts[id] = reserves(10, 10)
	}
	req := testRequest(ids...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.pick(req)
	}
}

func claudeFileExpiring(token string, expiry string) string {
	return `{"type":"claude","access_token":"` + token + `","refresh_token":"REFRESH_SECRET","expired":"` + expiry + `"}`
}

func TestPriorityWriteNeverRacesTokenRefresh(t *testing.T) {
	// a has a known weekly reset, so the plan is a=30, b=20, c=10 and a is written first.
	setup := func(t *testing.T) (*engine, *fakeHost) {
		e, host, _, _ := testEngine(t)
		e.syncRoster()
		setWindows(e, "a", window{}, win(10, 24*time.Hour))
		return e, host
	}
	saved := func(host *fakeHost, name string) bool {
		for _, save := range host.saves {
			if save.Name == name {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name   string
		expiry string
		want   string
	}{
		{"inside the refresh lead", testNow.Add(4 * time.Hour).Format(time.RFC3339), "deferred_token_refresh"},
		{"just inside the guard", testNow.Add(tokenRefreshGuard).Format(time.RFC3339), "deferred_token_refresh"},
		{"already expired", testNow.Add(-time.Hour).Format(time.RFC3339), "deferred_token_refresh"},
		{"unreadable expiry", "soon", "deferred_token_refresh"},
		{"far from refresh", testNow.Add(tokenRefreshGuard + time.Minute).Format(time.RFC3339), "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, host := setup(t)
			host.files["idx-a"] = claudeFileExpiring("SECRET_TOKEN_a", tc.expiry)
			e.applyPriorities()
			if got := e.accounts["a"].LastWrite; got != tc.want {
				t.Fatalf("write result %q, want %q", got, tc.want)
			}
			if saved(host, "a.json") != (tc.want == "ok") {
				t.Fatalf("a.json saved=%v with result %q", saved(host, "a.json"), tc.want)
			}
			// A deferral is transient and does not stop the other writes.
			if !saved(host, "b.json") || !saved(host, "c.json") {
				t.Fatalf("other accounts not written: %+v", host.saves)
			}
		})
	}
	t.Run("recent refresh", func(t *testing.T) {
		e, host := setup(t)
		e.accounts["a"].LastRefresh = testNow.Add(-time.Minute)
		e.applyPriorities()
		if e.accounts["a"].LastWrite != "deferred_token_refresh" || saved(host, "a.json") {
			t.Fatalf("wrote right after a refresh: %+v", e.accounts["a"])
		}
	})
	t.Run("file changed between reads", func(t *testing.T) {
		e, host := setup(t)
		reads := 0
		host.afterGet = func(h *fakeHost, index string) {
			if index == "idx-a" {
				reads++
				if reads == 1 {
					// CPA refreshes the token after the first read.
					h.files["idx-a"] = claudeFile("SECRET_TOKEN_a_REFRESHED")
				}
			}
		}
		e.applyPriorities()
		if e.accounts["a"].LastWrite != "deferred_changed" || saved(host, "a.json") {
			t.Fatalf("saved over a concurrent change: %+v", e.accounts["a"])
		}
		if host.files["idx-a"] != claudeFile("SECRET_TOKEN_a_REFRESHED") {
			t.Fatal("refreshed credential was overwritten")
		}
	})
}

func TestHardWriteFailureStopsThePass(t *testing.T) {
	e, host, _, _ := testEngine(t)
	e.syncRoster()
	setWindows(e, "a", window{}, win(10, 24*time.Hour))
	host.saveErrFor = map[string]error{"a.json": errors.New("disk")}
	e.applyPriorities()
	if len(host.saves) != 0 {
		t.Fatalf("lower ranks were written after the top write failed: %+v", host.saves)
	}
	if e.accounts["a"].LastWrite != "save_failed" || e.accounts["b"].LastWrite != "" {
		t.Fatalf("got a=%q b=%q", e.accounts["a"].LastWrite, e.accounts["b"].LastWrite)
	}
}

func TestSubdirectoryCredentialIsNeverWritten(t *testing.T) {
	e, host, _, _ := testEngine(t)
	calls := host.callCount()
	for _, id := range []string{"team/a.json", `team\a.json`} {
		if got := e.writePriority(planRow{ID: id, Index: "ia", Planned: 30}, time.Time{}); got != "skipped_unsupported_path" {
			t.Fatalf("%s: got %q", id, got)
		}
	}
	if host.callCount() != calls || len(host.saves) != 0 {
		t.Fatal("a subdirectory credential reached the host")
	}
}

func TestPollResultNeverOverwritesNewerHeaders(t *testing.T) {
	e, _, _, fetches := testEngine(t)
	e.syncRoster()
	ids := map[string]string{"SECRET_TOKEN_a": "a", "SECRET_TOKEN_b": "b", "SECRET_TOKEN_c": "c"}
	e.fetcher = fetchFunc(func(_ context.Context, c credential) (quotaSnapshot, error) {
		*fetches++
		// A response for the same account lands while the poll is in flight.
		e.observeUsage(pluginapi.UsageRecord{AuthID: ids[c.AccessToken], ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.95"},
		}})
		return quotaSnapshot{FiveHour: &quotaWindow{Utilization: ptr(10.0), ResetsAt: ptr(testNow.Add(time.Hour))}, Weekly: quotaWindow{Utilization: ptr(1.0), ResetsAt: ptr(testNow.Add(24 * time.Hour))}}, nil
	})
	e.tick(context.Background())
	for _, id := range []string{"a", "b", "c"} {
		acc := e.accounts[id]
		if acc.Source != "headers" || acc.FiveHour.Utilization == nil || *acc.FiveHour.Utilization < 94.99 {
			t.Fatalf("%s: stale poll overwrote newer headers: %+v", id, acc.FiveHour)
		}
	}
	// An account refreshed by headers after the queue was built is not polled at all.
	e2, _, now, fetches2 := testEngine(t)
	e2.syncRoster()
	*now = testNow.Add(time.Hour)
	e2.fetcher = fetchFunc(func(_ context.Context, c credential) (quotaSnapshot, error) {
		*fetches2++
		for _, id := range []string{"a", "b", "c"} {
			e2.observeUsage(pluginapi.UsageRecord{AuthID: id, ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}}})
		}
		return quotaSnapshot{}, errors.New("x")
	})
	e2.tick(context.Background())
	if *fetches2 != 1 {
		t.Fatalf("accounts refreshed by headers were still polled: %d polls", *fetches2)
	}
}

func TestNewResetStartsANewWindow(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.syncRoster()
	r1, r2 := testNow.Add(time.Hour).Unix(), testNow.Add(6*time.Hour).Unix()
	e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.95"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(r1, 10)},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"rejected"},
	}})
	e.observeUsage(pluginapi.UsageRecord{AuthID: "a", ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(r2, 10)},
	}})
	five := e.accounts["a"].FiveHour
	if five.Utilization != nil || five.Status != "" || five.ResetsAt.Unix() != r2 {
		t.Fatalf("previous window leaked into the new one: %+v", five)
	}
	// A poll keeps the last header status only for the same window.
	old := window{Utilization: ptr(100.0), ResetsAt: ptr(time.Unix(r1, 0).UTC()), Status: "rejected"}
	if got := windowFromQuota(&quotaWindow{Utilization: ptr(1.0), ResetsAt: ptr(time.Unix(r2, 0).UTC())}, old); got.Status != "" {
		t.Fatalf("status of the previous window kept: %+v", got)
	}
	if got := windowFromQuota(&quotaWindow{Utilization: ptr(100.0), ResetsAt: ptr(time.Unix(r1, 0).UTC())}, old); got.Status != "rejected" {
		t.Fatalf("status of the same window dropped: %+v", got)
	}
}
