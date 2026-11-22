package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// With accounts_file set, the reserves exist in two places on each machine: the shared file,
// which is what routing uses, and the inline accounts in CPA's config, which is what CPA's
// plugin settings screen edits. The plugin keeps them in step:
//   - an edit of the inline accounts (seen as a reconfigure) is copied into the file, one
//     changed account at a time, so stale values on the other accounts never overwrite the file;
//   - a file that differs from the inline accounts (an edit made on the other machine) is
//     written back into this CPA's config through its management API.

const mirrorRetry = 5 * time.Minute

// mirrorSettle skips the copy while an engine is new: at startup CPA creates and replaces the
// plugin a few times before its HTTP server listens, so an immediate PATCH would fail.
const mirrorSettle = 10 * time.Second

func normalizeAccounts(accounts map[string]accountReserve) map[string]accountReserve {
	out := make(map[string]accountReserve, len(accounts))
	for key, reserve := range accounts {
		out[accountKey(key)] = reserve
	}
	return out
}

func sameValue(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameReserve(a, b accountReserve) bool {
	return sameValue(a.FiveHourReserve, b.FiveHourReserve) && sameValue(a.WeeklyReserve, b.WeeklyReserve)
}

func accountsEqual(a, b map[string]accountReserve) bool {
	na, nb := normalizeAccounts(a), normalizeAccounts(b)
	if len(na) != len(nb) {
		return false
	}
	for key, reserve := range na {
		other, ok := nb[key]
		if !ok || !sameReserve(reserve, other) {
			return false
		}
	}
	return true
}

// mergeInlineEdit applies to file only the accounts that differ between the old and new inline
// accounts: changed or added accounts take the new value, removed ones are deleted. It reports
// whether the result differs from file.
func mergeInlineEdit(file, oldInline, newInline map[string]accountReserve) (map[string]accountReserve, bool) {
	result := normalizeAccounts(file)
	oldN, newN := normalizeAccounts(oldInline), normalizeAccounts(newInline)
	keys := make(map[string]bool, len(oldN)+len(newN))
	for key := range oldN {
		keys[key] = true
	}
	for key := range newN {
		keys[key] = true
	}
	for key := range keys {
		before, hadBefore := oldN[key]
		after, hasAfter := newN[key]
		if hadBefore == hasAfter && sameReserve(before, after) {
			continue
		}
		if hasAfter {
			result[key] = after
		} else {
			delete(result, key)
		}
	}
	return result, !accountsEqual(result, file)
}

// pushInlineEdit copies an edit of the inline accounts into accounts_file. It runs on reconfigure,
// with the engine that replaces old.
func pushInlineEdit(old, next *engine) {
	if old == nil || next.file == nil || accountsEqual(old.cfg.Accounts, next.cfg.Accounts) {
		return
	}
	current, loaded := next.file.accounts()
	if !loaded {
		// Nothing to merge into: the inline accounts become the file.
		current = map[string]accountReserve{}
	}
	merged, changed := mergeInlineEdit(current, old.cfg.Accounts, next.cfg.Accounts)
	if !changed {
		return
	}
	if err := next.file.write(merged); err != nil {
		next.host.Log("warn", "quota-reset-router: settings edit not saved to accounts_file: "+err.Error(), map[string]any{"path": next.file.path})
		return
	}
	next.host.Log("info", "quota-reset-router: settings edit saved to accounts_file", map[string]any{"path": next.file.path})
}

// mirrorToConfig starts writing the file's reserves into CPA's config when they differ from the
// inline accounts. It runs in its own goroutine because the write makes CPA reconfigure the
// plugin, which waits for this engine's background loop to stop.
func (e *engine) mirrorToConfig() {
	if e.file == nil || e.cfg.ManagementKeyFile == "" || time.Since(e.created) < mirrorSettle {
		return
	}
	accounts, loaded := e.file.accounts()
	if !loaded || accountsEqual(accounts, e.cfg.Accounts) {
		return
	}
	now := e.now()
	if last := e.mirrorAt.Load(); last != nil && now.Sub(*last) < mirrorRetry {
		return
	}
	e.mirrorAt.Store(&now)
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := patchConfigAccounts(ctx, e.cfg.ManagementURL, e.cfg.ManagementKeyFile, accounts); err != nil {
			e.host.Log("warn", "quota-reset-router: reserves not copied to CPA config: "+err.Error(), nil)
			return
		}
		e.host.Log("info", "quota-reset-router: reserves from accounts_file copied to CPA config", nil)
	}()
}

var mirrorClient = &http.Client{Timeout: 15 * time.Second}

func patchConfigAccounts(ctx context.Context, baseURL, keyFile string, accounts map[string]accountReserve) error {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return errors.New("management_key_file cannot be read")
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return errors.New("management_key_file is empty")
	}
	body, err := json.Marshal(map[string]any{"accounts": accounts})
	if err != nil {
		return errors.New("accounts could not be encoded")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/v0/management/plugins/" + pluginID + "/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("management_url is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := mirrorClient.Do(req)
	if err != nil {
		return errors.New("management API unreachable")
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("management API answered %d", resp.StatusCode)
	}
	return nil
}
