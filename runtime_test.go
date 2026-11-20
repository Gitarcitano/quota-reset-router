package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestConfigValidation(t *testing.T) {
	valid := []string{
		"",
		"mode: active",
		"poll_interval: 5m\nrequest_timeout: 1s",
		"poll_interval: 6h",
		"accounts:\n  User@Example.com:\n    five_hour_reserve: 10\n    weekly_reserve: 15",
		"accounts:\n  a:\n    weekly_reserve: 0\n  b:\n    five_hour_reserve: 95",
		// Keys from 0.3.x and unknown keys do not break loading.
		"enabled: true\npriority: 10\nmode: shadow\nmax_age: 10m\nfive_hour_cap: 80\nweekly_cap: 90\nrelease_before: 5h\nsession_ttl: 1h\nfuture_key: x",
		"accounts:\n  a:\n    weekly_cap: 50\n    five_hour_reserve: 10",
	}
	for _, raw := range valid {
		if _, err := decodeConfig([]byte(raw)); err != nil {
			t.Fatalf("valid config %q: %v", raw, err)
		}
	}
	invalid := []string{
		"mode: wrong", "poll_interval: 4m", "poll_interval: 7h", "poll_interval: invalid",
		"request_timeout: 0s", "request_timeout: 31s",
		"accounts:\n  a:\n    five_hour_reserve: 96", "accounts:\n  a:\n    weekly_reserve: -1",
		"accounts:\n  a:\n    weekly_reserve: lots", "accounts:\n  \"\":\n    weekly_reserve: 10",
	}
	for _, raw := range invalid {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid config accepted: %q", raw)
		}
	}
	cfg, _ := decodeConfig(nil)
	if cfg.Mode != "shadow" || cfg.PollInterval != 30*time.Minute || cfg.RequestTimeout != 10*time.Second {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	cfg, _ = decodeConfig([]byte("accounts:\n  User@Example.com:\n    five_hour_reserve: 10\n    weekly_reserve: 15\n  id-1:\n    weekly_reserve: 5"))
	if r := cfg.reserveFor("other", "user@example.COM"); r.FiveHourReserve == nil || *r.FiveHourReserve != 10 || *r.WeeklyReserve != 15 {
		t.Fatalf("email lookup failed: %+v", r)
	}
	if r := cfg.reserveFor("id-1", "user@example.com"); r.FiveHourReserve != nil || *r.WeeklyReserve != 5 {
		t.Fatal("auth ID key must win over the email key")
	}
	if r := cfg.reserveFor("ID-1", ""); r.WeeklyReserve == nil || *r.WeeklyReserve != 5 {
		t.Fatal("auth ID key must match case-insensitively")
	}
	if r := cfg.reserveFor("nobody", ""); r.FiveHourReserve != nil || r.WeeklyReserve != nil {
		t.Fatal("unconfigured account has a reserve")
	}
}

func decodeEnvelope(t *testing.T, raw []byte, result any) pluginabi.Envelope {
	t.Helper()
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if result != nil && envelope.OK {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			t.Fatal(err)
		}
	}
	return envelope
}

func TestRuntimeRegistrationReconfigureShutdown(t *testing.T) {
	p := &pluginRuntime{host: &fakeHost{}}
	defer p.stop()
	var reg struct {
		SchemaVersion uint32          `json:"schema_version"`
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginRegister, []byte(`{}`)), &reg); !env.OK {
		t.Fatalf("register: %s", env.Error)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion || !reg.Capabilities["scheduler"] || !reg.Capabilities["scheduler_across_priorities"] || !reg.Capabilities["management_api"] || !reg.Capabilities["usage_plugin"] || len(reg.Capabilities) != 4 {
		t.Fatalf("unexpected capabilities: %+v", reg)
	}
	if p.active.Load().cfg.Mode != "shadow" {
		t.Fatal("unsafe startup mode")
	}
	old := p.active.Load()
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginReconfigure, []byte(`{"config_yaml":"`+base64.StdEncoding.EncodeToString([]byte("mode: invalid"))+`"}`)), nil); env.OK {
		t.Fatal("invalid configuration accepted")
	}
	if p.active.Load() != old {
		t.Fatal("invalid reconfigure interrupted working instance")
	}
	request, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("mode: active")})
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodPluginReconfigure, request), nil); !env.OK {
		t.Fatalf("reconfigure: %s", env.Error)
	}
	if p.active.Load().cfg.Mode != "active" || p.active.Load() == old {
		t.Fatal("reconfigure did not replace worker")
	}
	p.handle(pluginabi.MethodPluginQuiesce, nil)
	if p.active.Load() != nil {
		t.Fatal("quiesce did not stop")
	}
	p.stop()
	var pick pluginapi.SchedulerPickResponse
	decodeEnvelope(t, p.handle(pluginabi.MethodSchedulerPick, []byte(`{}`)), &pick)
	if pick.Handled {
		t.Fatal("stopped runtime handled pick")
	}
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodUsageHandle, []byte(`{}`)), nil); !env.OK {
		t.Fatal("usage.handle must succeed without an active worker")
	}
}

func TestRuntimeUsageHandleUpdatesState(t *testing.T) {
	p := &pluginRuntime{}
	e, host, _, _ := testEngine(t)
	e.syncRoster()
	p.active.Store(e)
	calls := host.callCount()
	record, _ := json.Marshal(pluginapi.UsageRecord{AuthID: "a", Provider: "claude", ResponseHeaders: map[string][]string{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}}})
	var result map[string]any
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodUsageHandle, record), &result); !env.OK || len(result) != 0 {
		t.Fatalf("usage.handle response: %+v %v", env, result)
	}
	if e.accounts["a"].Weekly.Utilization == nil || *e.accounts["a"].Weekly.Utilization != 50 || e.accounts["a"].Source != "headers" {
		t.Fatalf("usage not recorded: %+v", e.accounts["a"])
	}
	if host.callCount() != calls {
		t.Fatal("usage.handle called the host")
	}
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodUsageHandle, []byte(`broken`)), nil); !env.OK {
		t.Fatal("malformed usage must not fail")
	}
}

func TestRuntimeManagementIsReadOnlyAndSanitized(t *testing.T) {
	p := &pluginRuntime{}
	e, _, _, _ := testEngine(t)
	p.active.Store(e)
	request, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: statusPath})
	var response pluginapi.ManagementResponse
	decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"mode":"active"`) {
		t.Fatalf("bad status response: %s", response.Body)
	}
	if strings.Contains(string(response.Body), "SECRET_TOKEN") {
		t.Fatal("secret leaked")
	}
	request, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: panelPath})
	decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "Cotas") {
		t.Fatal("panel was not served")
	}
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		request, _ = json.Marshal(pluginapi.ManagementRequest{Method: method, Path: statusPath})
		decodeEnvelope(t, p.handle(pluginabi.MethodManagementHandle, request), &response)
		if response.StatusCode != 404 {
			t.Fatal("mutating route exposed")
		}
	}
	var pick pluginapi.SchedulerPickResponse
	if env := decodeEnvelope(t, p.handle(pluginabi.MethodSchedulerPick, []byte(`broken`)), &pick); !env.OK || pick.Handled {
		t.Fatal("malformed pick should delegate")
	}
}

func TestConcurrentReconfigureAndPicks(t *testing.T) {
	p := &pluginRuntime{host: &fakeHost{}}
	defer p.stop()
	cfg, _ := decodeConfig(nil)
	p.configure(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.handle(pluginabi.MethodSchedulerPick, []byte(`{"Provider":"claude"}`))
			}
		}()
	}
	for i := 0; i < 10; i++ {
		p.configure(cfg)
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { p.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadlocked")
	}
}
