"""Exercise the release .so with CPA's exact C ABI and an isolated HTTPS fixture.

The host side is simulated in Python: auth list/get/save/log callbacks, usage
records and scheduler picks. The plugin must never pick an account itself.
"""

import base64
import ctypes as C
import datetime as dt
import http.server
import json
import os
from pathlib import Path
import ssl
import subprocess
import sys
import tempfile
import threading
import time


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("length", C.c_size_t)]


HOST_CALL = C.CFUNCTYPE(
    C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)
)
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PLUGIN_CALL = C.CFUNCTYPE(
    C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)
)
SHUTDOWN = C.CFUNCTYPE(None)


class HostAPI(C.Structure):
    _fields_ = [
        ("abi", C.c_uint32),
        ("context", C.c_void_p),
        ("call", HOST_CALL),
        ("free", FREE),
    ]


class PluginAPI(C.Structure):
    _fields_ = [
        ("abi", C.c_uint32),
        ("call", PLUGIN_CALL),
        ("free", FREE),
        ("shutdown", SHUTDOWN),
    ]


def main():
    library = Path(sys.argv[1]).resolve()
    assert os.environ.get("PLUGIN_ISOLATED_TEST") == "1", (
        "Run only inside the network-isolated test container"
    )
    libc = C.CDLL(None)
    libc.malloc.argtypes, libc.malloc.restype = [C.c_size_t], C.c_void_p
    libc.free.argtypes = [C.c_void_p]
    claude_ids = ["a-seven-days", "b-two-days", "c-one-day", "z-five-hours"]
    ids = claude_ids + ["codex-ignored"]
    durations = dict(zip(claude_ids, [168, 48, 24, 5]))
    files = {
        i: {
            "type": "claude" if i in claude_ids else "codex",
            "access_token": ("test-token-" if i in claude_ids else "codex-token-") + i,
            "refresh_token": "refresh-secret-" + i,
            "email": i + "@example.invalid",
            "note": "<keep & me>",
            "nested": {"list": [1, 2, 3]},
            "priority": 0,
        }
        for i in ids
    }
    original = json.loads(json.dumps(files))
    allocations = set()
    callback_errors = []
    methods = set()
    upstream_requests = []
    saves = []
    lock = threading.Lock()

    def deadline(hours):
        return (
            dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=hours)
        ).isoformat()

    class Fixture(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            token = self.headers.get("Authorization", "").removeprefix(
                "Bearer test-token-"
            )
            assert token in durations and self.path == "/api/oauth/usage"
            assert self.headers.get("anthropic-beta") == "oauth-2025-04-20"
            upstream_requests.append(token)
            body = json.dumps(
                {
                    "seven_day": {
                        "utilization": 20,
                        "resets_at": deadline(durations[token]),
                    },
                    "five_hour": {"utilization": 0, "resets_at": None},
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format, *args):
            pass

    @HOST_CALL
    def host_call(context, method, request, length, output):
        try:
            assert context == 0x123456
            method = method.decode()
            methods.add(method)
            data = json.loads(C.string_at(request, length))
            with lock:
                if method == "host.auth.list":
                    result = {
                        "files": [
                            {
                                "id": i,
                                "name": i + ".json",
                                "auth_index": "idx-" + i,
                                "provider": files[i]["type"],
                                "type": files[i]["type"],
                                "status": "active",
                                "email": files[i]["email"],
                                "priority": files[i]["priority"],
                            }
                            for i in ids
                        ]
                    }
                elif method == "host.auth.get":
                    i = data["auth_index"].removeprefix("idx-")
                    assert i in ids
                    result = {
                        "auth_index": data["auth_index"],
                        "name": i + ".json",
                        "path": "/auths/" + i + ".json",
                        "json": files[i],
                    }
                elif method == "host.auth.save":
                    i = data["name"].removesuffix(".json")
                    assert i in claude_ids, "saved a non-Claude credential"
                    files[i] = data["json"]
                    saves.append((i, data["json"]["priority"]))
                    result = {"name": data["name"], "path": "/auths/" + data["name"]}
                elif method == "host.log":
                    text = json.dumps(data)
                    assert "token-" not in text and "secret" not in text, text
                    result = {}
                else:
                    raise AssertionError("Unexpected host capability: " + method)
            raw = json.dumps({"ok": True, "result": result}).encode()
            pointer = libc.malloc(len(raw))
            C.memmove(pointer, raw, len(raw))
            allocations.add(pointer)
            output[0] = Buffer(pointer, len(raw))
            return 0
        except BaseException as exc:
            callback_errors.append(str(exc))
            return 1

    @FREE
    def host_free(pointer, length):
        if pointer not in allocations:
            callback_errors.append("unrecognized host buffer freed")
            return
        allocations.remove(pointer)
        libc.free(pointer)

    def wait_for(check, label, timeout=20):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if check():
                return
            time.sleep(0.02)
        raise AssertionError(label + ": callbacks=" + repr(callback_errors))

    with tempfile.TemporaryDirectory(prefix="quota-reset-router-native-") as tmp:
        cert, key = Path(os.environ["SSL_CERT_FILE"]), Path(tmp) / "key.pem"
        subprocess.run(
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-days",
                "1",
                "-keyout",
                str(key),
                "-out",
                str(cert),
                "-subj",
                "/CN=api.anthropic.com",
                "-addext",
                "subjectAltName=DNS:api.anthropic.com",
            ],
            check=True,
            capture_output=True,
        )
        os.environ["SSL_CERT_FILE"] = str(cert)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 443), Fixture)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        lib = C.CDLL(str(library))
        init = lib.cliproxy_plugin_init
        init.argtypes, init.restype = (
            [C.POINTER(HostAPI), C.POINTER(PluginAPI)],
            C.c_int,
        )
        host, api = HostAPI(1, 0x123456, host_call, host_free), PluginAPI()
        invalid_host = HostAPI(999, 0x123456, host_call, host_free)
        assert init(C.byref(invalid_host), C.byref(api)) != 0
        assert init(C.byref(host), C.byref(api)) == 0
        assert api.abi == 1

        def call(method, request):
            raw = json.dumps(request).encode()
            input_buffer = C.create_string_buffer(raw)
            output = Buffer()
            code = api.call(method.encode(), input_buffer, len(raw), C.byref(output))
            assert output.ptr
            try:
                envelope = json.loads(C.string_at(output.ptr, output.length))
            finally:
                api.free(output.ptr, output.length)
            assert code == 0 and envelope["ok"], envelope
            return envelope["result"]

        def status():
            result = call(
                "management.handle",
                {
                    "Method": "GET",
                    "Path": "/v0/management/plugins/quota-reset-router/status",
                },
            )
            return json.loads(base64.b64decode(result["Body"]))

        def configure(yaml_text, method="plugin.reconfigure"):
            payload = {"config_yaml": base64.b64encode(yaml_text.encode()).decode()}
            reg = call(method, payload)
            assert reg["schema_version"] == 10, reg
            assert reg["capabilities"] == {
                "scheduler": True,
                "scheduler_across_priorities": True,
                "usage_plugin": True,
                "management_api": True,
            }, reg

        def assert_no_pick(result):
            assert not result.get("Handled") and not result.get("AuthID"), result
            assert not result.get("Reject"), result

        request = {
            "Provider": "claude",
            "Providers": ["claude"],
            "Model": "claude-sonnet-4-6",
            "Candidates": [
                {"ID": i, "Provider": "claude", "Status": "active"} for i in claude_ids
            ],
        }
        reserve = "accounts:\n  z-five-hours@example.invalid:\n    five_hour_reserve: 10\n"
        configure("mode: shadow\n" + reserve, "plugin.register")
        wait_for(
            lambda: all(
                (status()["accounts"].get(i) or {}).get("seven_day", {}).get("resets_at")
                for i in claude_ids
            ),
            "quota polls",
        )
        data = status()
        assert data["version"] == "0.6.0" and data["mode"] == "shadow", data
        assert "codex-ignored" not in data["accounts"], "Codex account tracked"
        planned = {i: data["accounts"][i]["planned_priority"] for i in claude_ids}
        assert planned == {
            "z-five-hours": 40,
            "c-one-day": 30,
            "b-two-days": 20,
            "a-seven-days": 10,
        }, planned
        assert_no_pick(call("scheduler.pick", request))
        assert not call("scheduler.pick", request).get("ExcludeCandidates")
        assert not saves, "shadow mode wrote a priority"

        configure("mode: active\n" + reserve)
        wait_for(lambda: len(saves) == 4, "priority writes")
        for i in claude_ids:
            expected = dict(original[i], priority=planned[i])
            assert files[i] == expected, (i, files[i])
        assert files["codex-ignored"] == original["codex-ignored"]

        # A usage record crosses the 10% five-hour reserve of z-five-hours.
        reset = int(time.time()) + 3600
        call(
            "usage.handle",
            {
                "AuthID": "z-five-hours",
                "Provider": "claude",
                "ResponseHeaders": {
                    "anthropic-ratelimit-unified-5h-utilization": ["0.95"],
                    "anthropic-ratelimit-unified-5h-reset": [str(reset)],
                    "anthropic-ratelimit-unified-5h-status": ["allowed"],
                },
            },
        )
        assert status()["accounts"]["z-five-hours"]["at_reserve"]
        result = call("scheduler.pick", request)
        assert_no_pick(result)
        assert result.get("ExcludeCandidates") == ["z-five-hours"], result
        only = dict(request, Candidates=request["Candidates"][-1:])
        result = call("scheduler.pick", only)
        assert_no_pick(result)
        assert not result.get("ExcludeCandidates"), "excluded the only account"
        for _ in range(500):
            assert_no_pick(call("scheduler.pick", request))
        text = json.dumps(status())
        assert "token-" not in text and "secret" not in text, "status leaked a token"
        call("plugin.quiesce", {})
        result = call("scheduler.pick", request)
        assert_no_pick(result)
        assert not result.get("ExcludeCandidates")
        api.shutdown()
        assert not allocations, "host response buffer leak"
        assert not callback_errors, callback_errors
        assert methods <= {"host.auth.list", "host.auth.get", "host.auth.save", "host.log"}
        assert {"host.auth.list", "host.auth.get", "host.auth.save"} <= methods, methods
        assert sorted(upstream_requests) == sorted(claude_ids), upstream_requests
        server.shutdown()
        print(
            "PASS: release .so native ABI, TLS quota reads, shadow plan, active priority writes "
            "that keep other fields, usage headers, reserve exclusion, 500 picks without a "
            "selection, quiesce, buffer ownership"
        )


if __name__ == "__main__":
    main()
