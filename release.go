package main

import (
	"sync"
	"time"
)

// Early release: a weekly reserve protects quota the account's owner may need, but quota left
// at the weekly reset is lost. Near the reset the plugin estimates how long the traffic it routes
// would take to spend what is left of the account, and from that moment on it ignores the weekly
// reserve so the account is used until it runs out. The five-hour reserve still applies.
//
// The consumption rate is measured, not configured: every rise of the weekly utilization of an
// account that is taking traffic (not held at its reserve) is summed over the last 24 hours and
// divided by wall-clock time, idle hours included, since the reset arrives on the clock. Until
// two hours were measured, the released account's own average since its window started is used.

const (
	weeklyWindow     = 7 * 24 * time.Hour
	burnLookback     = 24 * time.Hour
	burnMinSpan      = 2 * time.Hour
	defaultMaxLead   = 24 * time.Hour
	maxReleaseWindow = 72 * time.Hour
	// releaseMargin stretches the time needed to spend what is left: the measured rate covers all
	// routed traffic, but sessions bound to other accounts stay there, so the released account
	// usually gets less. Releasing a bit early costs little; releasing late loses quota.
	releaseMargin = 1.5
	// resetTolerance treats two reset times this close as the same window: the usage poll reports
	// the reset with a sub-second part that changes between polls, the headers in whole seconds.
	resetTolerance = time.Minute
	// minBurnStep ignores rises smaller than a rounding error: the headers give utilization as a
	// fraction, so 0.44 becomes 44.00000000000001 where the poll says 44.
	minBurnStep = 0.01
)

func sameWindow(a, b *time.Time) bool {
	if a == nil || b == nil {
		return false
	}
	d := a.Sub(*b)
	return d > -resetTolerance && d < resetTolerance
}

type burnEvent struct {
	at    time.Time
	delta float64
}

// burnMeter sums the weekly utilization points spent by routed traffic. It survives a
// reconfigure (the replacing engine takes it over) but not a restart.
type burnMeter struct {
	mu     sync.Mutex
	start  time.Time
	events []burnEvent
}

func newBurnMeter(start time.Time) *burnMeter {
	return &burnMeter{start: start}
}

func (m *burnMeter) add(at time.Time, delta float64) {
	if delta <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, burnEvent{at: at, delta: delta})
}

// rate returns the percentage points spent per hour over the last 24 hours, and false while
// less than two hours were measured.
func (m *burnMeter) rate(now time.Time) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := now.Add(-burnLookback)
	kept := m.events[:0]
	sum := 0.0
	for _, ev := range m.events {
		if ev.at.After(from) {
			kept = append(kept, ev)
			sum += ev.delta
		}
	}
	m.events = kept
	if m.start.After(from) {
		from = m.start
	}
	span := now.Sub(from)
	if span < burnMinSpan {
		return 0, false
	}
	return sum / span.Hours(), true
}

// recordWeekly feeds the meter with the rise of an account's weekly utilization. Only the part
// above the highest value seen in the same window counts, so a poll and a header that disagree
// by a rounding step are not counted twice. A held account's rise is its owner's use, not
// traffic this plugin routed, so it only moves the high-water mark. Callers hold e.mu.
//
// held must say whether the account was really left out of routing: see (*engine).held.
func (e *engine) recordWeekly(acc *account, held bool, now time.Time) {
	w := acc.Weekly
	if w.Utilization == nil || w.ResetsAt == nil {
		return
	}
	value := *w.Utilization
	if !sameWindow(acc.peakReset, w.ResetsAt) {
		reset := *w.ResetsAt
		acc.peakReset, acc.peak = &reset, value
		return
	}
	if value-acc.peak < minBurnStep {
		return
	}
	if !held && e.burn != nil {
		e.burn.add(now, value-acc.peak)
	}
	acc.peak = value
}

// released reports whether the weekly reserve is lifted for the account's current window.
func (a *account) released(now time.Time) bool {
	return a.Weekly.live(now) && sameWindow(a.ReleasedUntil, a.Weekly.ResetsAt)
}

func measuredRate(rate float64, measured bool) *float64 {
	if !measured {
		return nil
	}
	return &rate
}

type releasePlan struct {
	Rate       float64
	RateSource string // "measured", "window_average" or "" when unknown
	Lead       time.Duration
	At         time.Time
}

// planRelease computes when the account's weekly reserve should be lifted: the reset minus the
// time the measured rate needs to spend what is left (stretched by releaseMargin), capped at maxLead. It returns false when
// the account has no weekly reserve or no live weekly window.
func planRelease(acc *account, cfg config, rate float64, measured bool, maxLead time.Duration, now time.Time) (releasePlan, bool) {
	reserve := cfg.reserveFor(acc.ID, acc.Email)
	if maxLead <= 0 || reserve.WeeklyReserve == nil || *reserve.WeeklyReserve <= 0 || !acc.Weekly.live(now) || acc.Weekly.Utilization == nil {
		return releasePlan{}, false
	}
	used := *acc.Weekly.Utilization
	plan := releasePlan{Lead: maxLead}
	if measured {
		plan.Rate, plan.RateSource = rate, "measured"
	} else {
		start := acc.Weekly.ResetsAt.Add(-weeklyWindow)
		if elapsed := now.Sub(start); elapsed >= time.Hour {
			plan.Rate, plan.RateSource = used/elapsed.Hours(), "window_average"
		}
	}
	if left := 100 - used; left <= 0 {
		plan.Lead = 0
	} else if plan.Rate > 0 {
		// Compared in hours first: a tiny rate would overflow a Duration.
		if hours := left / plan.Rate * releaseMargin; hours < maxLead.Hours() {
			plan.Lead = time.Duration(hours * float64(time.Hour))
		}
	}
	plan.At = acc.Weekly.ResetsAt.Add(-plan.Lead)
	return plan, true
}

// updateReleases lifts the weekly reserve of the accounts whose release time arrived. A release
// lasts until that window resets, so a later change of the rate does not take it back; a
// release_window reduced below the time left does.
func (e *engine) updateReleases() {
	cfg := e.config()
	now := e.now()
	rate, measured := 0.0, false
	if e.burn != nil {
		rate, measured = e.burn.rate(now)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, acc := range e.accounts {
		if acc.ReleasedUntil != nil && (!acc.released(now) || acc.ReleasedUntil.Sub(now) > cfg.ReleaseWindow) {
			acc.ReleasedUntil = nil
		}
		if acc.ReleasedUntil != nil {
			continue
		}
		plan, ok := planRelease(acc, cfg, rate, measured, cfg.ReleaseWindow, now)
		if !ok || now.Before(plan.At) {
			continue
		}
		reset := *acc.Weekly.ResetsAt
		acc.ReleasedUntil = &reset
		e.host.Log("info", "quota-reset-router: weekly reserve released before the reset", map[string]any{
			"account": acc.Label, "resets_at": reset.Format(time.RFC3339), "rate_per_hour": plan.Rate, "rate_source": plan.RateSource,
		})
	}
}

// held reports whether the account is left out of routing right now: at its reserve, in active
// mode, while another account is open. Otherwise it takes traffic like any other. Callers hold e.mu.
func (e *engine) held(acc *account, cfg config, now time.Time) bool {
	if cfg.Mode != "active" || !acc.atReserve(cfg, now) {
		return false
	}
	for _, other := range e.accounts {
		if other != acc && !other.atReserve(cfg, now) && !other.exhausted(now) {
			return true
		}
	}
	return false
}
