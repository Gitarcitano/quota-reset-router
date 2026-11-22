package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const statusPath = "/v0/management/plugins/" + pluginID + "/status"
const accountsPath = "/v0/management/plugins/" + pluginID + "/accounts"
const panelPath = "/v0/resource/plugins/" + pluginID + "/panel"

type pluginRuntime struct {
	host    authHost
	mu      sync.Mutex
	active  atomic.Pointer[engine]
	cancel  context.CancelFunc
	done    chan struct{}
	fetcher *httpQuotaFetcher
}

func success(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return failure("response_encode_failed")
	}
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func failure(code string) []byte {
	return failureMessage(code, code)
}

func failureMessage(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

func (p *pluginRuntime) handle(method string, raw []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return failure("invalid_lifecycle_request")
			}
		}
		cfg, err := decodeConfig(req.ConfigYAML)
		if err != nil {
			// Validation messages name only config keys and limits, never secrets.
			return failureMessage("invalid_configuration", err.Error())
		}
		if p.host == nil {
			return failure("host_unavailable")
		}
		p.configure(cfg)
		return success(registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.stop()
		return success(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return success(p.pick(raw))
	case pluginabi.MethodUsageHandle:
		var record pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &record); err == nil {
			if e := p.active.Load(); e != nil {
				p.observe(e, record)
			}
		}
		return success(struct{}{})
	case pluginabi.MethodManagementRegister:
		return success(pluginapi.ManagementRegistrationResponse{
			Routes: []pluginapi.ManagementRoute{{Method: http.MethodGet, Path: statusPath}, {Method: http.MethodPost, Path: accountsPath}},
			Resources: []pluginapi.ResourceRoute{{
				Path:        "/panel",
				Menu:        "Cotas",
				Description: "Limites de 5 horas e 7 dias, reservas por conta e prioridade pela renovacao semanal.",
			}},
		})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return success(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
		}
		if req.Method == http.MethodPost && req.Path == accountsPath {
			return success(p.saveAccounts(req.Body))
		}
		if req.Method != http.MethodGet {
			return success(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
		}
		if req.Path == panelPath {
			return success(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: []byte(panelHTML)})
		}
		if req.Path != statusPath {
			return success(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
		}
		var status any = map[string]string{"state": "stopped"}
		if e := p.active.Load(); e != nil {
			status = e.status()
		}
		body, _ := json.Marshal(status)
		return success(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
	default:
		return failure("unsupported_method")
	}
}

func jsonResponse(code int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body}
}

// saveAccounts replaces the reserves in accounts_file. Without accounts_file the reserves live
// in CPA's config and are saved through its own config endpoint instead.
func (p *pluginRuntime) saveAccounts(body []byte) pluginapi.ManagementResponse {
	e := p.active.Load()
	if e == nil || e.file == nil {
		return jsonResponse(http.StatusConflict, map[string]string{"error": "accounts_file is not configured"})
	}
	var req struct {
		Accounts map[string]accountReserve `json:"accounts"`
		// Previous is what the caller saw before editing. With it, only the accounts the caller
		// changed are applied, so an edit that arrived from the other machine in the meantime stays.
		Previous map[string]accountReserve `json:"previous"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "body must be {\"accounts\": {...}}"})
	}
	if err := validateAccounts(req.Accounts); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	accounts := req.Accounts
	if req.Previous != nil {
		current, loaded := e.file.accounts()
		if !loaded {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "accounts_file was not read yet"})
		}
		merged, changed := mergeInlineEdit(current, req.Previous, req.Accounts)
		if !changed {
			return jsonResponse(http.StatusOK, e.status())
		}
		accounts = merged
	}
	if err := e.file.write(accounts); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	e.host.Log("info", "quota-reset-router: reserves saved to accounts_file", map[string]any{"path": e.file.path})
	return jsonResponse(http.StatusOK, e.status())
}

// pick delegates to CPA's selector. Any decode error or panic yields an empty response, so the
// plugin never blocks or picks an account (fail-open).
func (p *pluginRuntime) pick(raw []byte) (response pluginapi.SchedulerPickResponse) {
	e := p.active.Load()
	defer func() {
		if recover() != nil {
			if e != nil {
				e.failed.Store(true)
			}
			response = pluginapi.SchedulerPickResponse{}
		}
	}()
	var req pluginapi.SchedulerPickRequest
	if e == nil || json.Unmarshal(raw, &req) != nil {
		return pluginapi.SchedulerPickResponse{}
	}
	response = e.pick(req)
	// Enforce the contract even if the engine misbehaves: never pick, reject or handle.
	return pluginapi.SchedulerPickResponse{ExcludeCandidates: response.ExcludeCandidates}
}

func (p *pluginRuntime) observe(e *engine, record pluginapi.UsageRecord) {
	defer func() {
		if recover() != nil {
			e.failed.Store(true)
		}
	}()
	e.observeUsage(record)
}

func (p *pluginRuntime) configure(cfg config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.active.Load()
	p.stopLocked()
	p.fetcher = newHTTPFetcher()
	e := newEngine(p.host, p.fetcher, cfg)
	e.adoptFile(old)
	pushInlineEdit(old, e)
	if old != nil {
		e.inherit(old)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.active.Store(e)
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				e.failed.Store(true)
			}
		}()
		e.run(ctx)
	}()
}

func (p *pluginRuntime) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *pluginRuntime) stopLocked() {
	p.active.Store(nil)
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel, p.done = nil, nil
	}
	if p.fetcher != nil {
		p.fetcher.client.CloseIdleConnections()
		p.fetcher = nil
	}
}

func registration() any {
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{
		pluginabi.SchemaVersion,
		pluginapi.Metadata{Name: pluginID, Version: pluginVersion, Author: "Shreyash (webdevcaptain)", GitHubRepository: "https://github.com/webdevcaptain/quota-reset-router", ConfigFields: []pluginapi.ConfigField{
			{Name: "mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"shadow", "active"}, Default: "shadow", Description: "Shadow only reports the plan. Active rewrites account priorities and excludes accounts at their reserve."},
			{Name: "poll_interval", Type: pluginapi.ConfigFieldTypeString, Default: "30m", Description: "Poll the usage endpoint for accounts not observed for this long, 5m to 6h. Default 30m."},
			{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "10s", Description: "Usage poll timeout, 1s to 30s. Default 10s."},
			{Name: "accounts", Type: pluginapi.ConfigFieldTypeObject, Description: "Per account (auth ID or email): five_hour_reserve and weekly_reserve, 0 to 95 percent kept unused while another account is available."},
			{Name: "accounts_file", Type: pluginapi.ConfigFieldTypeString, Description: "Optional absolute path (or ~/...) of a YAML file with the accounts map. It replaces the inline accounts once read and is re-read when it changes, so it can be synced between machines."},
			{Name: "management_url", Type: pluginapi.ConfigFieldTypeString, Default: "http://127.0.0.1:8317", Description: "This CPA's address, used with management_key_file to copy the reserves from accounts_file into these settings."},
			{Name: "management_key_file", Type: pluginapi.ConfigFieldTypeString, Description: "Optional file holding this CPA's plaintext management key. With it, edits made on the other machine show up in these settings."},
		}},
		map[string]bool{"scheduler": true, "scheduler_across_priorities": true, "usage_plugin": true, "management_api": true},
	}
}
