package main

/*
#include "bridge.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxRPCBytes = 16 * 1024 * 1024

type nativeHost struct{ api C.cliproxy_host_api }

var plugin = &pluginRuntime{}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != C.uint32_t(pluginabi.ABIVersion) || host.call == nil || host.free_buffer == nil {
		return 1
	}
	plugin.host = &nativeHost{api: *host}
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if recover() != nil {
			if e := plugin.active.Load(); e != nil {
				e.failed.Store(true)
			}
			if method != nil && C.GoString(method) == pluginabi.MethodSchedulerPick {
				writeResponse(response, success(pluginapi.SchedulerPickResponse{}))
				code = 0
				return
			}
			writeResponse(response, failure("internal_plugin_error"))
			code = 1
		}
	}()
	if method == nil || length > maxRPCBytes || request == nil && length != 0 {
		writeResponse(response, failure("invalid_rpc_request"))
		return 1
	}
	var raw []byte
	if length > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	result := plugin.handle(C.GoString(method), raw)
	writeResponse(response, result)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr)
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	plugin.stop()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

func (h *nativeHost) call(method string, request any, result any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("host_request_encode_failed")
	}
	m := C.CString(method)
	p := C.CBytes(raw)
	defer C.free(unsafe.Pointer(m))
	defer C.free(p)
	var response C.cliproxy_buffer
	code := C.bridge_host_call(&h.api, m, (*C.uint8_t)(p), C.size_t(len(raw)), &response)
	defer C.bridge_host_free(&h.api, &response)
	if code != 0 || response.ptr == nil || response.len == 0 || response.len > maxRPCBytes {
		return errors.New("host_callback_failed")
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &envelope); err != nil || !envelope.OK {
		return errors.New("host_response_failed")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("host_result_invalid")
	}
	return nil
}

func (h *nativeHost) List() ([]pluginapi.HostAuthFileEntry, error) {
	var result struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	err := h.call(pluginabi.MethodHostAuthList, struct{}{}, &result)
	return result.Files, err
}

func (h *nativeHost) Get(index string) (string, json.RawMessage, error) {
	var result pluginapi.HostAuthGetResponse
	if err := h.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index}, &result); err != nil {
		return "", nil, err
	}
	if result.AuthIndex != index {
		return "", nil, errors.New("credential_identity_mismatch")
	}
	// The host saves by base name into the auth directory, so a name that differs from the
	// backing file would write a second file instead of updating this one.
	if result.Path != "" && filepath.Base(result.Path) != result.Name {
		return "", nil, errors.New("credential_name_mismatch")
	}
	return result.Name, result.JSON, nil
}

func (h *nativeHost) Save(name string, raw json.RawMessage) error {
	var result pluginapi.HostAuthSaveResponse
	return h.call(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}, &result)
}

// Log forwards a message to the host logger. Callers never pass credential material.
func (h *nativeHost) Log(level, message string, fields map[string]any) {
	var result json.RawMessage
	_ = h.call(pluginabi.MethodHostLog, struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields,omitempty"`
	}{level, message, fields}, &result)
}
