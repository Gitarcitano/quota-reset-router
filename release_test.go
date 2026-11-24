package main

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBurnMeterRate(t *testing.T) {
	m := newBurnMeter(testNow)
	m.add(testNow.Add(30*time.Minute), 3)
	if _, ok := m.rate(testNow.Add(time.Hour)); ok {
		t.Fatal("a rate needs two measured hours")
	}
	m.add(testNow.Add(90*time.Minute), 5)
	m.add(testNow.Add(2*time.Hour), -4) // never a negative spend
	if rate, ok := m.rate(testNow.Add(4 * time.Hour)); !ok || !near(rate, 2) {
		t.Fatalf("8 points over 4h must be 2/h, got %v %v", rate, ok)
	}
	// After a day only the last 24 hours count, over a 24 hour span.
	m.add(testNow.Add(30*time.Hour), 12)
	if rate, ok := m.rate(testNow.Add(31 * time.Hour)); !ok || !near(rate, 0.5) {
		t.Fatalf("12 points over the last 24h must be 0.5/h, got %v", rate)
	}
}

func TestRecordWeeklyCountsOnlyRoutedNewSpend(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.burn = newBurnMeter(testNow.Add(-4 * time.Hour))
	acc := &account{ID: "a"}
	observe := func(util float64, reset time.Duration, held bool) {
		acc.Weekly = win(util, reset)
		e.recordWeekly(acc, held, testNow)
	}
	observe(60, 48*time.Hour, false) // first sight: baseline only
	observe(62, 48*time.Hour, false) // +2
	observe(61, 48*time.Hour, false) // a poll rounding down
	observe(62, 48*time.Hour, false) // back to the peak: nothing new
	observe(65, 48*time.Hour, true)  // the owner's use while held: not ours
	observe(66, 48*time.Hour, false) // +1
	observe(3, 216*time.Hour, false) // new window: baseline only
	observe(4, 216*time.Hour, false) // +1
	if rate, _ := e.burn.rate(testNow); !near(rate, 1) {
		t.Fatalf("4 routed points over 4h must be 1/h, got %v", rate)
	}
}

func TestPlanRelease(t *testing.T) {
	cfg := config{Accounts: map[string]accountReserve{"r": reserves(10, 15)}}
	acc := &account{ID: "r", Weekly: win(89, 10*time.Hour)}
	plan, ok := planRelease(acc, cfg, 2, true, 24*time.Hour, testNow)
	if !ok || plan.RateSource != "measured" || plan.Lead != 8*time.Hour+15*time.Minute || !plan.At.Equal(testNow.Add(time.Hour+45*time.Minute)) {
		t.Fatalf("11 points left at 2/h need 5h30, 8h15 with the margin: %+v", plan)
	}
	if plan, _ := planRelease(acc, cfg, 0.1, true, 24*time.Hour, testNow); plan.Lead != 24*time.Hour {
		t.Fatalf("a slow rate is capped at release_window: %+v", plan)
	}
	if plan, _ := planRelease(acc, cfg, 0, true, 24*time.Hour, testNow); plan.Lead != 24*time.Hour {
		t.Fatalf("no measured spend releases at release_window: %+v", plan)
	}
	// Unmeasured: the account's own average since its window began (158h ago here).
	plan, _ = planRelease(acc, cfg, 0, false, 24*time.Hour, testNow)
	if plan.RateSource != "window_average" || !near(plan.Rate, 89.0/158) {
		t.Fatalf("fallback rate: %+v", plan)
	}
	for name, tc := range map[string]struct {
		acc *account
		cfg config
		max time.Duration
	}{
		"no weekly reserve":   {acc, config{Accounts: map[string]accountReserve{"r": {FiveHourReserve: ptr(10.0)}}}, 24 * time.Hour},
		"zero weekly reserve": {acc, config{Accounts: map[string]accountReserve{"r": reserves(10, 0)}}, 24 * time.Hour},
		"turned off":          {acc, cfg, 0},
		"past reset":          {&account{ID: "r", Weekly: win(89, -time.Minute)}, cfg, 24 * time.Hour},
		"unknown utilization": {&account{ID: "r", Weekly: window{ResetsAt: ptr(testNow.Add(time.Hour))}}, cfg, 24 * time.Hour},
	} {
		if _, ok := planRelease(tc.acc, tc.cfg, 2, true, tc.max, testNow); ok {
			t.Fatalf("%s: no release plan expected", name)
		}
	}
}

// Beatriz's case: at her weekly reserve with the reset a few hours away, she must be used again
// as soon as the measured rate needs the time left, and stay in use until the window resets.
func TestEarlyReleaseLiftsTheWeeklyReserveUntilTheReset(t *testing.T) {
	e, _, now, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"b": reserves(10, 15)}
	e.burn = newBurnMeter(testNow.Add(-24 * time.Hour))
	e.burn.add(testNow.Add(-time.Hour), 48) // 2 points per hour over the last day
	e.syncRoster()
	setWindows(e, "b", win(5, time.Hour), win(89, 9*time.Hour))
	setWindows(e, "a", win(5, time.Hour), win(50, 72*time.Hour))
	setWindows(e, "c", win(5, time.Hour), win(50, 96*time.Hour))

	// 11 points left at 2/h need 8h15 with the margin; the reset is 9h away, so not yet.
	e.updateReleases()
	if got := e.pick(testRequest("a", "b", "c")); !reflect.DeepEqual(got.ExcludeCandidates, []string{"b"}) {
		t.Fatalf("b must stay held 9h before the reset: %+v", got)
	}
	status := statusJSON(t, e)
	if !strings.Contains(status, `"weekly_release_at":"`+testNow.Add(45*time.Minute).Format(time.RFC3339)) || !strings.Contains(status, `"burn_rate_per_hour":2`) {
		t.Fatalf("status must show the planned release and the rate: %s", status)
	}

	*now = testNow.Add(45 * time.Minute)
	e.updateReleases()
	if got := e.pick(testRequest("a", "b", "c")); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("b must be released 8h15 before the reset: %+v", got)
	}
	plan, _ := planPriorities(e.accounts, e.config(), *now)
	if plan[0].ID != "b" {
		t.Fatalf("the released account resets first, so it must lead: %+v", plan)
	}
	if !strings.Contains(statusJSON(t, e), `"weekly_reserve_released":true`) {
		t.Fatal("status must show the release")
	}

	// A faster rate later does not take the release back, but the five-hour reserve still holds.
	e.burn.add(*now, 200)
	e.updateReleases()
	setWindows(e, "b", win(95, time.Hour), win(93, 9*time.Hour))
	if got := e.pick(testRequest("a", "b", "c")); !reflect.DeepEqual(got.ExcludeCandidates, []string{"b"}) {
		t.Fatalf("the five-hour reserve must still apply: %+v", got)
	}
	setWindows(e, "b", win(5, time.Hour), win(93, 9*time.Hour))
	e.updateReleases()
	if got := e.pick(testRequest("a", "b", "c")); len(got.ExcludeCandidates) != 0 {
		t.Fatalf("the release must last until the reset: %+v", got)
	}

	// The next window starts held again.
	setWindows(e, "b", win(5, time.Hour), win(90, 7*24*time.Hour))
	e.updateReleases()
	if got := e.pick(testRequest("a", "b", "c")); !reflect.DeepEqual(got.ExcludeCandidates, []string{"b"}) {
		t.Fatalf("a new window must not inherit the release: %+v", got)
	}
}

func TestEarlyReleaseOffClearsReleases(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"b": reserves(10, 15)}
	e.syncRoster()
	setWindows(e, "b", window{}, win(89, time.Hour))
	e.updateReleases()
	if !e.accounts["b"].released(testNow) {
		t.Fatal("one hour before the reset is inside the default 24h window")
	}
	e.cfg.ReleaseWindow = 0
	e.updateReleases()
	if e.accounts["b"].released(testNow) || !e.accounts["b"].atReserve(e.config(), testNow) {
		t.Fatal("release_window 0 must turn early release off")
	}
}

func TestReconfigureKeepsReleaseAndRate(t *testing.T) {
	old, host, _, _ := testEngine(t)
	old.cfg.Accounts = map[string]accountReserve{"b": reserves(10, 15)}
	old.burn = newBurnMeter(testNow.Add(-24 * time.Hour))
	old.burn.add(testNow, 24)
	old.syncRoster()
	setWindows(old, "b", window{}, win(89, time.Hour))
	old.updateReleases()
	next := newEngine(host, old.fetcher, old.cfg)
	next.now = old.now
	next.inherit(old)
	if !next.accounts["b"].released(testNow) {
		t.Fatal("the release must survive a reconfigure")
	}
	if rate, ok := next.burn.rate(testNow); !ok || !near(rate, 1) {
		t.Fatalf("the measured rate must survive a reconfigure: %v %v", rate, ok)
	}
}

func TestObservedSpendFeedsTheRate(t *testing.T) {
	e, _, now, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"b": reserves(0, 15)}
	e.burn = newBurnMeter(testNow.Add(-2 * time.Hour))
	e.syncRoster()
	reset := strconv.FormatInt(testNow.Add(72*time.Hour).Unix(), 10)
	usage := func(id, util string) {
		e.observeUsage(pluginapi.UsageRecord{AuthID: id, ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {util},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {reset},
		}})
	}
	usage("a", "0.40")
	usage("a", "0.44")
	usage("b", "0.86") // b at its reserve: its owner's use
	usage("b", "0.88")
	if rate, ok := e.burn.rate(*now); !ok || !near(rate, 2) {
		t.Fatalf("only a's 4 routed points over 2h count, got %v %v", rate, ok)
	}
}

func TestReleaseWindowConfig(t *testing.T) {
	cfg, err := decodeConfig(nil)
	if err != nil || cfg.ReleaseWindow != 24*time.Hour {
		t.Fatalf("default release_window must be 24h: %v %v", cfg.ReleaseWindow, err)
	}
	for raw, want := range map[string]time.Duration{"release_window: 0": 0, `release_window: "0s"`: 0, "release_window: 6h": 6 * time.Hour, "release_window: 72h": 72 * time.Hour, `release_window: ""`: 24 * time.Hour} {
		if cfg, err := decodeConfig([]byte(raw)); err != nil || cfg.ReleaseWindow != want {
			t.Fatalf("%s: got %v %v", raw, cfg.ReleaseWindow, err)
		}
	}
	for _, raw := range []string{"release_window: 73h", "release_window: -1h", "release_window: soon", "release_window: 5"} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Fatalf("%s must be rejected", raw)
		}
	}
}

func statusJSON(t *testing.T, e *engine) string {
	t.Helper()
	raw, err := json.Marshal(e.status())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The usage poll reports the reset with a sub-second part that changes between polls, and the
// headers in whole seconds: the same window must keep its release and its high-water mark.
func TestResetJitterKeepsReleaseAndPeak(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"b": reserves(10, 15)}
	e.burn = newBurnMeter(testNow.Add(-4 * time.Hour))
	e.syncRoster()
	reset := testNow.Add(time.Hour)
	setWindows(e, "b", window{}, window{Utilization: ptr(89.0), ResetsAt: ptr(reset.Add(327187 * time.Microsecond))})
	e.updateReleases()
	acc := e.accounts["b"]
	acc.Weekly = window{Utilization: ptr(90.0), ResetsAt: ptr(reset.Add(485688 * time.Microsecond))}
	e.recordWeekly(acc, false, testNow)
	acc.Weekly = window{Utilization: ptr(92.0), ResetsAt: ptr(reset)}
	e.recordWeekly(acc, false, testNow)
	if !acc.released(testNow) || acc.atReserve(e.config(), testNow) {
		t.Fatal("a reset that differs by a fraction of a second is the same window")
	}
	if rate, _ := e.burn.rate(testNow); !near(rate, 0.5) {
		t.Fatalf("2 points over 4h must be 0.5/h, got %v", rate)
	}
}

// A rate made of float noise (a header's 44.00000000000001 after a poll's 44) must neither feed
// the meter nor overflow the lead into a release days before the reset.
func TestTinyRateNeverReleasesEarly(t *testing.T) {
	cfg := config{Accounts: map[string]accountReserve{"r": reserves(10, 15)}}
	acc := &account{ID: "r", Weekly: win(89, 6*24*time.Hour)}
	plan, ok := planRelease(acc, cfg, 7e-15, true, 24*time.Hour, testNow)
	if !ok || plan.Lead != 24*time.Hour || !plan.At.Equal(testNow.Add(5*24*time.Hour)) {
		t.Fatalf("a tiny rate must cap at release_window: %+v", plan)
	}
	e, _, _, _ := testEngine(t)
	e.burn = newBurnMeter(testNow.Add(-4 * time.Hour))
	noisy := &account{ID: "a"}
	for _, util := range []float64{44, 0.44 * 100, 44.005} {
		noisy.Weekly = win(util, 48*time.Hour)
		e.recordWeekly(noisy, false, testNow)
	}
	if rate, _ := e.burn.rate(testNow); rate != 0 {
		t.Fatalf("rounding noise must not count as spend, got %v", rate)
	}
}

func TestShorterReleaseWindowTakesBackARelease(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"b": reserves(10, 15)}
	e.syncRoster()
	setWindows(e, "b", window{}, win(89, 3*time.Hour))
	e.updateReleases()
	if !e.accounts["b"].released(testNow) {
		t.Fatal("3h before the reset is inside the default 24h window")
	}
	e.cfg.ReleaseWindow = 2 * time.Hour
	e.updateReleases()
	if e.accounts["b"].released(testNow) {
		t.Fatal("a release earlier than the new release_window must be taken back")
	}
}

// An account at its reserve still takes traffic in shadow mode and when no other account is
// open, so its rise is routed spend there.
func TestHeldOnlyWhenReallyExcluded(t *testing.T) {
	e, _, _, _ := testEngine(t)
	e.cfg.Accounts = map[string]accountReserve{"a": reserves(0, 15), "b": reserves(0, 15), "c": reserves(0, 15)}
	e.syncRoster()
	for _, id := range []string{"a", "b", "c"} {
		setWindows(e, id, window{}, win(90, 48*time.Hour))
	}
	cfg := e.config()
	if e.held(e.accounts["a"], cfg, testNow) {
		t.Fatal("with every account at its reserve none is excluded")
	}
	setWindows(e, "c", window{}, win(50, 48*time.Hour))
	if !e.held(e.accounts["a"], cfg, testNow) {
		t.Fatal("a is excluded while c is open")
	}
	cfg.Mode = "shadow"
	if e.held(e.accounts["a"], cfg, testNow) {
		t.Fatal("shadow mode never excludes")
	}
	if e.held(e.accounts["c"], e.config(), testNow) {
		t.Fatal("an open account is not held")
	}
}
