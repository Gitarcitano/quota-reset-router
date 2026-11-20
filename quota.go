package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
const maxUsageBytes = 128 * 1024

type credential struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
	Disabled    bool   `json:"disabled"`
	ProxyURL    string `json:"proxy_url"`
	BaseURL     string `json:"base_url"`
}

type quotaWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type quotaSnapshot struct {
	Provider string       `json:"provider"`
	Weekly   quotaWindow  `json:"seven_day"`
	FiveHour *quotaWindow `json:"five_hour,omitempty"`
	Sonnet   *quotaWindow `json:"seven_day_sonnet,omitempty"`
	Opus     *quotaWindow `json:"seven_day_opus,omitempty"`
	ReadAt   time.Time    `json:"read_at"`
}

func validateWindow(w *quotaWindow, required bool) bool {
	if w == nil {
		return !required
	}
	if w.Utilization == nil || math.IsNaN(*w.Utilization) || math.IsInf(*w.Utilization, 0) || *w.Utilization < 0 || *w.Utilization > 100 {
		return false
	}
	// A zero-use short window can legitimately have no countdown yet.
	return w.ResetsAt != nil && !w.ResetsAt.IsZero() || !required && *w.Utilization == 0
}

func parseQuota(raw []byte) (quotaSnapshot, error) {
	var snap quotaSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snap, errors.New("invalid_quota_json")
	}
	if !validateWindow(&snap.Weekly, true) || !validateWindow(snap.FiveHour, false) || !validateWindow(snap.Sonnet, false) || !validateWindow(snap.Opus, false) {
		return quotaSnapshot{}, errors.New("invalid_quota_windows")
	}
	return snap, nil
}

type quotaFetcher interface {
	Fetch(context.Context, credential) (quotaSnapshot, error)
}

type httpQuotaFetcher struct {
	client *http.Client
}

func newHTTPFetcher() *httpQuotaFetcher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 2
	transport.MaxIdleConnsPerHost = 1
	return &httpQuotaFetcher{client: &http.Client{
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type quotaError struct {
	code       string
	retryAfter time.Duration
}

func (e *quotaError) Error() string { return e.code }

func (f *httpQuotaFetcher) Fetch(ctx context.Context, cred credential) (quotaSnapshot, error) {
	provider := strings.ToLower(strings.TrimSpace(cred.Type))
	if provider != "claude" || cred.Disabled || strings.TrimSpace(cred.AccessToken) == "" {
		return quotaSnapshot{}, errors.New("ineligible_credential")
	}
	// Quota polling connects directly; refuse rather than bypass a per-auth proxy or custom endpoint.
	if cred.ProxyURL != "" || cred.BaseURL != "" {
		return quotaSnapshot{}, errors.New("custom_transport_unsupported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return quotaSnapshot{}, errors.New("quota_request_failed")
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := f.client.Do(req)
	if err != nil {
		return quotaSnapshot{}, errors.New("quota_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := time.Duration(0)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retry = retryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return quotaSnapshot{}, &quotaError{code: fmt.Sprintf("quota_http_%d", resp.StatusCode), retryAfter: retry}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBytes+1))
	if err != nil || len(body) > maxUsageBytes {
		return quotaSnapshot{}, errors.New("invalid_quota_body")
	}
	snap, err := parseQuota(body)
	if err != nil {
		return quotaSnapshot{}, err
	}
	snap.ReadAt = time.Now().UTC()
	snap.Provider = provider
	return snap, nil
}

func retryAfter(raw string, now time.Time) time.Duration {
	delay := time.Duration(0)
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		if seconds > int64(maxPollBackoff/time.Second) {
			return maxPollBackoff
		}
		if seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
	} else if deadline, err := http.ParseTime(raw); err == nil {
		delay = deadline.Sub(now)
	}
	return min(maxPollBackoff, max(0, delay))
}
