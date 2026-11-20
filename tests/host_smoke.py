"""Run the official CPA release binary against synthetic credentials and local TLS.

CPA runs with fill-first and session affinity. The quota endpoint is down, so the
plugin learns only from the rate-limit headers on fixture responses. Checks:
a. the plugin registers;
b. priorities are rewritten so the earliest weekly reset is on top, and the auth
   files keep every other field;
c. a session bound to the other account stays there, while a new session goes to
   the new top account;
d. an account past its reserve gets no new sessions while another account is
   available, and gets traffic again when it is the only one.
"""

from concurrent.futures import ThreadPoolExecutor
import itertools
import datetime as dt
import hashlib
import http.server
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request


PLUGIN = "quota-reset-router"
PLUGINS = "/v8/management/plugins"
AUTH_STATUS = "/v0/management/auth-files/status"
STATUS = "/v0/management/plugins/" + PLUGIN + "/status"
CONFIG = "/v0/management/plugins/" + PLUGIN + "/config"
MODEL = "claude-opus-4-6"


def main():
    assert os.environ.get("PLUGIN_ISOLATED_TEST") == "1", (
        "Requires api.anthropic.com pinned to 127.0.0.1, as in the Linux test "
        "container or macOS CI"
    )
    if len(sys.argv) != 4:
        sys.exit(
            "usage: host_smoke.py <cpa-archive> <cpa-checksums.txt> <plugin-library>"
        )
    archive, checksums_path, library = (Path(arg).resolve() for arg in sys.argv[1:])
    soak = float(os.environ.get("SOAK_SECONDS", "0"))
    checksums = dict(
        line.split()[::-1] for line in checksums_path.read_text().splitlines()
    )
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == checksums[archive.name]
    ids = ["alpha", "beta"]
    start = int(time.time())
    # Per account and window: (utilization fraction, reset as unix seconds).
    # beta's weekly window resets first, so beta must end on top.
    limits = {
        "alpha": {"5h": (0.1, start + 3 * 3600), "7d": (0.2, start + 120 * 3600)},
        "beta": {"5h": (0.1, start + 3 * 3600), "7d": (0.3, start + 48 * 3600)},
    }
    fixture_errors = []
    usage_polls = []

    class Fixture(http.server.BaseHTTPRequestHandler):
        def account(self):
            token = self.headers.get("Authorization", "").removeprefix(
                "Bearer sk-ant-oat-test-"
            )
            if token not in ids:
                fixture_errors.append("unknown synthetic account")
            return token

        def send_json(self, code, body, extra=None):
            raw = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            for name, value in (extra or {}).items():
                self.send_header(name, value)
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            if self.path != "/api/oauth/usage":
                fixture_errors.append("unexpected GET " + self.path)
                return self.send_json(404, {})
            usage_polls.append(self.account())
            # The quota endpoint is down: headers are the only quota source.
            self.send_json(503, {"error": "synthetic quota outage"})

        def do_POST(self):
            if self.path.split("?")[0] != "/v1/messages":
                fixture_errors.append("unexpected POST " + self.path)
                return self.send_json(404, {})
            account = self.account()
            self.rfile.read(int(self.headers["Content-Length"]))
            headers = {}
            for name, (utilization, reset) in limits[account].items():
                prefix = "anthropic-ratelimit-unified-" + name + "-"
                headers[prefix + "utilization"] = str(utilization)
                headers[prefix + "reset"] = str(reset)
                headers[prefix + "status"] = "allowed"
            message = {
                "id": "msg_synthetic",
                "type": "message",
                "role": "assistant",
                "model": MODEL,
                "content": [{"type": "text", "text": account}],
                "stop_reason": "end_turn",
                "stop_sequence": None,
                "usage": {"input_tokens": 1, "output_tokens": 1},
            }
            self.send_json(200, message, headers)

        def log_message(self, format, *args):
            pass

    with tempfile.TemporaryDirectory(prefix="cpa-host-smoke-") as tmp:
        root = Path(tmp)
        binary = root / "cli-proxy-api"
        with tarfile.open(archive) as tar:
            member = next(
                m for m in tar if Path(m.name).name == "cli-proxy-api" and m.isfile()
            )
            extracted = tar.extractfile(member)
            assert extracted is not None
            binary.write_bytes(extracted.read())
        binary.chmod(0o700)
        if sys.platform == "darwin":
            # Go on macOS ignores SSL_CERT_FILE; CI trusts this cert in the keychain.
            cert, key = (
                Path(os.environ["FIXTURE_CERT"]),
                Path(os.environ["FIXTURE_KEY"]),
            )
        else:
            cert, key = root / "cert.pem", root / "key.pem"
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
        # macOS allows unprivileged binds below port 1024 only on the wildcard address.
        bind = "0.0.0.0" if sys.platform == "darwin" else "127.0.0.1"
        server = http.server.ThreadingHTTPServer((bind, 443), Fixture)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        auths = root / "auths"
        auths.mkdir()
        for account in ids:
            (auths / (account + ".json")).write_text(
                json.dumps(
                    {
                        "type": "claude",
                        "access_token": "sk-ant-oat-test-" + account,
                        "refresh_token": "synthetic-refresh-" + account,
                        "account_uuid": account,
                        "email": account + "@example.invalid",
                        "expired": "2099-01-01T00:00:00Z",
                        # Older than the plugin's recent-refresh guard, so writes are not deferred.
                        "last_refresh": (dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=1)).isoformat(),
                        "note": "<keep & me>",
                        "priority": 0,
                    }
                )
            )
        plugin_dir = root / "plugins"
        plugin_dir.mkdir()
        (plugin_dir / (PLUGIN + library.suffix)).write_bytes(library.read_bytes())
        config = root / "config.yaml"
        config.write_text(f"""host: 127.0.0.1
port: 18318
auth-dir: {auths}
api-keys: [synthetic-client-key]
remote-management:
  allow-remote: false
  secret-key: synthetic-management-key
  disable-control-panel: true
routing:
  strategy: fill-first
  session-affinity: true
request-retry: 3
max-retry-credentials: 0
max-retry-interval: 1
plugins:
  enabled: true
  dir: {plugin_dir}
  configs:
    {PLUGIN}:
      enabled: true
      priority: 10
      mode: shadow
      accounts:
        BETA@example.invalid:
          five_hour_reserve: 10
""")
        log = (root / "host.log").open("w+")
        env = dict(os.environ, SSL_CERT_FILE=str(cert), GOMEMLIMIT="256MiB")
        if soak:
            # Force frequent GC in both runtimes while they share the process.
            env["GOGC"] = "10"
        proc = subprocess.Popen(
            [str(binary), "--config", str(config), "--local-model"],
            cwd=root,
            env=env,
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def api(path, method="GET", body=None, management=True, headers=None):
            key = "synthetic-management-key" if management else "synthetic-client-key"
            request = urllib.request.Request(
                "http://127.0.0.1:18318" + path,
                data=None if body is None else json.dumps(body).encode(),
                headers={
                    "Authorization": "Bearer " + key,
                    "Content-Type": "application/json",
                    **(headers or {}),
                },
                method=method,
            )
            try:
                with opener.open(request, timeout=15) as response:
                    return response.read()
            except urllib.error.HTTPError as exc:
                # Still a URLError, so wait_for keeps retrying, but with the body.
                raise urllib.error.URLError(
                    f"{method} {path}: HTTP {exc.code}: {exc.read()[:500]!r}"
                ) from None

        # The plugin reconciles every 30 s, so waits for writes allow a full cycle.
        def wait_for(check, label, timeout=45):
            end = time.monotonic() + timeout
            last = None
            while time.monotonic() < end:
                try:
                    last = check()
                    if last:
                        return last
                except (urllib.error.URLError, ValueError, KeyError) as exc:
                    last = repr(exc)
                if proc.poll() is not None:
                    raise AssertionError("CPA exited during " + label)
                time.sleep(0.2)
            raise AssertionError(label + ": " + repr(last))

        def status():
            return json.loads(api(STATUS))

        def account_status(account):
            return status()["accounts"].get(account + ".json") or {}

        def observed(account):
            state = account_status(account)
            return state.get("source") == "headers" and state.get("seven_day", {}).get(
                "resets_at"
            )

        def stored(account):
            return json.loads((auths / (account + ".json")).read_text())

        def set_disabled(account, disabled):
            api(AUTH_STATUS, "PATCH", {"name": account + ".json", "disabled": disabled})

        def message(session):
            # CPA rebuilds its model registry after startup and after an auth status
            # change; until then it answers "unknown provider", which never reaches the
            # scheduler, so only that error is retried.
            def send():
                try:
                    return api(
                        "/v1/messages",
                        "POST",
                        {
                            "model": MODEL,
                            "max_tokens": 16,
                            "messages": [
                                {"role": "user", "content": "Synthetic routing test"}
                            ],
                        },
                        False,
                        {"X-Claude-Code-Session-Id": session},
                    )
                except urllib.error.URLError as exc:
                    if "unknown provider" in str(exc.reason):
                        return None
                    # Any other failure is real: escape wait_for instead of retrying.
                    raise AssertionError(str(exc.reason)) from None

            raw = wait_for(send, "model " + MODEL + " registered", timeout=15)
            return json.loads(raw)["content"][0]["text"]

        def registered():
            listing = json.loads(api(PLUGINS))
            entry = next((p for p in listing["plugins"] if p["id"] == PLUGIN), None)
            return entry and entry["registered"] and entry

        try:
            # a. The plugin registers and tracks both accounts.
            entry = wait_for(registered, "plugin registration")
            assert entry["metadata"]["version"] == "0.4.0", entry["metadata"]
            wait_for(
                lambda: len(status()["accounts"]) == len(ids), "account discovery"
            )
            data = status()
            assert data["mode"] == "shadow" and data["version"] == "0.4.0", data

            # b. Learn both accounts from response headers. With equal priorities
            # fill-first takes alpha, so beta is reached while alpha is disabled.
            set_disabled("alpha", True)
            assert message("probe-beta") == "beta"
            wait_for(lambda: observed("beta"), "beta header observation")
            set_disabled("alpha", False)
            wait_for(lambda: account_status("alpha"), "alpha back in the roster")
            assert message("session-bound-alpha") == "alpha"
            wait_for(lambda: observed("alpha"), "alpha header observation")
            before = {account: stored(account) for account in ids}
            planned = {
                account: account_status(account)["planned_priority"] for account in ids
            }
            assert planned == {"beta": 20, "alpha": 10}, planned
            assert all(before[a]["priority"] == 0 for a in ids), "shadow wrote a file"
            assert status()["priority_writes"] == 0

            api(CONFIG, "PATCH", {"mode": "active"})
            wait_for(
                lambda: (
                    stored("beta").get("priority") == 20
                    and stored("alpha").get("priority") == 10
                ),
                "priority rewrite in the auth files",
            )
            for account in ids:
                after = stored(account)
                assert dict(after, priority=0) == before[account], (
                    "auth file lost fields",
                    account,
                )
            assert account_status("beta")["last_write"] == "ok"

            # c. The bound session stays; a new session goes to the new top.
            assert message("session-bound-alpha") == "alpha", "binding was broken"
            assert message("session-new-1") == "beta", "new session missed the top"
            sessions = itertools.count()
            end, soaked = time.monotonic() + soak, 0
            with ThreadPoolExecutor(max_workers=8) as pool:
                while True:
                    replies = list(
                        pool.map(
                            lambda _: message("soak-" + str(next(sessions))), range(32)
                        )
                    )
                    assert replies == ["beta"] * 32, "new sessions left the top"
                    soaked += len(replies)
                    if time.monotonic() >= end:
                        break

            # d. beta crosses its 10% five-hour reserve on its next response.
            limits["beta"]["5h"] = (0.95, start + 3 * 3600)
            assert message("session-new-1") == "beta"
            wait_for(lambda: account_status("beta").get("at_reserve"), "beta at reserve")
            assert message("session-new-2") == "alpha", "at-reserve account selected"
            last_pick = status()["last_pick"]
            assert last_pick["applied"] and last_pick["would_exclude"] == [
                "beta.json"
            ], last_pick
            assert message("session-new-3") == "alpha", "at-reserve account selected"
            set_disabled("alpha", True)
            assert message("session-new-4") == "beta", (
                "the only account was excluded"
            )
            set_disabled("alpha", False)
            assert not fixture_errors, fixture_errors
            assert set(usage_polls) <= set(ids)
            text = json.dumps(status())
            assert "sk-ant-oat-test" not in text and "synthetic-refresh" not in text
            soak_note = f"{soaked} requests in a {soak:.0f} s soak, " if soak else ""
            print(
                "PASS: official release checksum verified; registration, header-driven "
                "priority rewrite that keeps auth fields, session affinity kept, "
                + soak_note
                + "reserve exclusion and last-account fallback; RSS "
                + rss_kib(proc.pid)
                + " KiB"
            )
        except BaseException:
            log.flush()
            log.seek(0)
            print(
                "\n".join(
                    line for line in log.read().splitlines() if STATUS not in line
                ),
                file=sys.stderr,
            )
            raise
        finally:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
                raise AssertionError("CPA shutdown timed out")
            log.close()
            server.shutdown()


def rss_kib(pid):
    if sys.platform == "darwin":
        return subprocess.run(
            ["ps", "-o", "rss=", "-p", str(pid)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    status = Path(f"/proc/{pid}/status").read_text()
    return next(
        line.split()[1] for line in status.splitlines() if line.startswith("VmRSS:")
    )


if __name__ == "__main__":
    main()
