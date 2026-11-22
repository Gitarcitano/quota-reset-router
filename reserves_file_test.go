package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// writeReserves writes the file and moves its mtime forward, like a sync tool delivering an edit.
func writeReserves(t *testing.T, path, body string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func fileEngine(t *testing.T, path string) *engine {
	t.Helper()
	base, _, _, _ := testEngine(t)
	cfg, err := decodeConfig([]byte("mode: active\naccounts_file: " + path + "\naccounts:\n  a:\n    five_hour_reserve: 50"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(base.host, base.fetcher, cfg)
	e.now = base.now
	e.syncRoster()
	setWindows(e, "a", win(60, time.Hour), window{})
	return e
}

// pass runs a background pass (which also polls quota) and then puts a back at 60% of its 5h window.
func pass(t *testing.T, e *engine) {
	e.tick(t.Context())
	setWindows(e, "a", win(60, time.Hour), window{})
}

func excluded(e *engine) []string {
	return e.pick(testRequest("a", "b")).ExcludeCandidates
}

func TestAccountsFileDrivesReservesAndFollowsEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserves.yaml")
	start := time.Now().Add(-time.Hour)

	// Until the file exists, the inline accounts apply (a keeps 50% of 5h, so 60% is at reserve).
	e := fileEngine(t, path)
	if got := excluded(e); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("inline reserves must apply while the file is missing: %v", got)
	}

	// Once read, the file replaces the inline accounts.
	writeReserves(t, path, "accounts:\n  A@Example.com:\n    five_hour_reserve: 30\n", start)
	pass(t, e)
	if got := excluded(e); len(got) != 0 {
		t.Fatalf("file reserve of 30%% must leave a open at 60%%: %v", got)
	}

	// An edit delivered by the sync tool is picked up on the next pass.
	writeReserves(t, path, "accounts:\n  A@Example.com:\n    five_hour_reserve: 45\n", start.Add(time.Minute))
	pass(t, e)
	if got := excluded(e); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("edited reserve was not applied: %v", got)
	}

	// A broken edit keeps the last good reserves and is reported.
	writeReserves(t, path, "accounts:\n  a:\n    five_hour_reserve: 99\n", start.Add(2*time.Minute))
	pass(t, e)
	if got := excluded(e); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("invalid file must keep the last good reserves: %v", got)
	}
	status, _ := json.Marshal(e.status())
	if !strings.Contains(string(status), `"accounts_file_error":"accounts_file: five_hour_reserve for a must be between 0 and 95"`) {
		t.Fatalf("file error not reported: %s", status)
	}
}

func TestPanelSavesReservesToAccountsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared", "reserves.yaml")
	p := &pluginRuntime{}
	e := fileEngine(t, path)
	p.active.Store(e)

	post := func(body string) pluginapi.ManagementResponse {
		request, _ := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: accountsPath, Body: []byte(body)})
		var response pluginapi.ManagementResponse
		decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
		return response
	}

	if r := post(`{"accounts":{"a":{"five_hour_reserve":96}}}`); r.StatusCode != 400 {
		t.Fatalf("invalid reserve accepted: %d %s", r.StatusCode, r.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected save wrote the file")
	}

	if r := post(`{"accounts":{"a":{"five_hour_reserve":20,"weekly_reserve":30}}}`); r.StatusCode != 200 {
		t.Fatalf("save failed: %d %s", r.StatusCode, r.Body)
	}
	if got := excluded(e); len(got) != 0 {
		t.Fatalf("saved reserve must apply at once: %v", got)
	}
	// What was written is what another machine would load.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := parseReserves(raw)
	if err != nil || *accounts["a"].FiveHourReserve != 20 || *accounts["a"].WeeklyReserve != 30 {
		t.Fatalf("file content: %v %s", err, raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	// Without accounts_file the route refuses, so reserves keep going through CPA's config.
	plain, _, _, _ := testEngine(t)
	p.active.Store(plain)
	if r := post(`{"accounts":{}}`); r.StatusCode != 409 {
		t.Fatalf("save without accounts_file: %d", r.StatusCode)
	}
}

func TestAccountsFilePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg, err := decodeConfig([]byte("accounts_file: ~/sync/reserves.yaml"))
	if err != nil || cfg.AccountsFile != filepath.Join(home, "sync", "reserves.yaml") {
		t.Fatalf("~ not expanded: %q %v", cfg.AccountsFile, err)
	}
	if _, err := decodeConfig([]byte("accounts_file: relative/reserves.yaml")); err == nil {
		t.Fatal("relative accounts_file accepted")
	}
}

func mustConfig(t *testing.T, raw string) config {
	t.Helper()
	cfg, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readReserves(t *testing.T, path string) map[string]accountReserve {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := parseReserves(raw)
	if err != nil {
		t.Fatal(err)
	}
	return accounts
}

func TestSettingsEditReachesFileWithoutStaleValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserves.yaml")
	writeReserves(t, path, "accounts:\n  lorena@x.com:\n    five_hour_reserve: 50\n    weekly_reserve: 40\n  beatriz@x.com:\n    five_hour_reserve: 10\n    weekly_reserve: 15\n", time.Now().Add(-time.Hour))
	p := &pluginRuntime{host: &fakeHost{}}
	defer p.stop()
	head := "mode: active\naccounts_file: " + path + "\naccounts:\n"
	// This machine's settings still hold an old value for lorena (the file was edited elsewhere).
	stale := head + "  lorena@x.com:\n    five_hour_reserve: 70\n    weekly_reserve: 50\n"

	p.configure(mustConfig(t, stale+"  beatriz@x.com:\n    five_hour_reserve: 10\n    weekly_reserve: 15\n"))
	if got := readReserves(t, path); *got["lorena@x.com"].FiveHourReserve != 50 {
		t.Fatalf("startup must not push the settings into the file: %+v", got)
	}

	// Editing beatriz in the settings screen saves beatriz only; lorena's stale value stays out.
	p.configure(mustConfig(t, stale+"  beatriz@x.com:\n    five_hour_reserve: 20\n    weekly_reserve: 15\n"))
	got := readReserves(t, path)
	if *got["beatriz@x.com"].FiveHourReserve != 20 || *got["lorena@x.com"].FiveHourReserve != 50 {
		t.Fatalf("edit not merged account by account: %+v", got)
	}

	// Removing an account in the settings removes it from the file.
	p.configure(mustConfig(t, stale))
	if _, ok := readReserves(t, path)["beatriz@x.com"]; ok {
		t.Fatal("removed account kept in the file")
	}
}

func TestFileEditIsCopiedToConfig(t *testing.T) {
	type patch struct {
		auth, path string
		body       map[string]map[string]accountReserve
	}
	got := make(chan patch, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]map[string]accountReserve
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method == http.MethodPatch {
			got <- patch{r.Header.Get("Authorization"), r.URL.Path, body}
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	path, keyFile := filepath.Join(dir, "reserves.yaml"), filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte("s3cret\n"), 0o600)
	writeReserves(t, path, "accounts:\n  lorena@x.com:\n    five_hour_reserve: 50\n", time.Now())
	base, _, _, _ := testEngine(t)
	settings := "mode: active\naccounts_file: " + path + "\nmanagement_url: " + server.URL + "\nmanagement_key_file: " + keyFile + "\naccounts:\n  lorena@x.com:\n    five_hour_reserve: "

	e := newEngine(base.host, base.fetcher, mustConfig(t, settings+"70"))
	e.tick(t.Context())
	select {
	case p := <-got:
		t.Fatalf("a brand-new engine must wait before writing the config: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
	e.created = e.created.Add(-mirrorSettle)
	e.tick(t.Context())
	select {
	case p := <-got:
		if p.auth != "Bearer s3cret" || p.path != "/v0/management/plugins/quota-reset-router/config" || *p.body["accounts"]["lorena@x.com"].FiveHourReserve != 50 {
			t.Fatalf("bad config write: %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("file reserves were not copied to the config")
	}

	// Once the config matches the file, nothing is written.
	e = newEngine(base.host, base.fetcher, mustConfig(t, settings+"50"))
	e.created = e.created.Add(-mirrorSettle)
	e.tick(t.Context())
	select {
	case p := <-got:
		t.Fatalf("config already in step was written again: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}
