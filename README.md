# quota-reset-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin for Claude OAuth accounts. It keeps the account whose **7-day quota resets soonest** at the top of CLIProxyAPI's own routing, and it keeps a per-account reserve of the 5-hour and 7-day quota out of reach of new sessions.

- **Author:** Shreyash ([webdevcaptain](https://github.com/webdevcaptain))
- **License:** [MIT](LICENSE)

> Not affiliated with or endorsed by Anthropic or CLIProxyAPI. Check the provider's terms before routing subscription credentials through a proxy.

## How it works

The plugin never picks an account. CLIProxyAPI's native selector keeps choosing, with its session affinity, retries and cooldowns. The plugin steers it in two ways:

1. **Priority.** It rewrites the `priority` field of each Claude credential so the account that renews its weekly quota first is on top. With `routing.strategy: fill-first`, new sessions go to that account, while sessions already bound to another account stay there.
2. **Reserve exclusion.** When an account reaches its reserve, the plugin asks CLIProxyAPI to leave it out of the current selection (`ExcludeCandidates`), but only while at least one other candidate is not at its reserve. An account at its reserve is never excluded when it is the only option.

### Ranking

Eligible Claude accounts are split into two groups:

- first: accounts that are neither at their reserve nor exhausted;
- second: everything else. An account is exhausted when a window reports status `rejected` or 100% utilization and its reset is still in the future.

Inside each group, the earliest known future weekly reset comes first, then accounts with no known reset. Ties go to the current priority (higher first), then the credential ID. The account in position `rank` (0-based) out of `n` gets priority `(n - rank) * 10`.

Only accounts whose priority changes are written. Nothing is written until at least one weekly reset is known.

### Reserve

An account is at its reserve when, for the 5-hour or the 7-day window, utilization is at least `100 - reserve` and that window's reset is still in the future. Each window is checked on its own. An account with no reserve configured is never excluded.

Example: with `five_hour_reserve: 10`, the account stops taking new sessions at 90% of its 5-hour window, until that window resets or until it is the only account left.

### Quota data

- **Response headers.** Every Claude response carries `anthropic-ratelimit-unified-5h-*` and `anthropic-ratelimit-unified-7d-*` (utilization as a fraction, reset as Unix seconds, status). The plugin reads them from CLIProxyAPI's usage records, with no extra requests.
- **Polling.** An account whose last observation is older than `poll_interval` is read from `GET https://api.anthropic.com/api/oauth/usage`, one account at a time. A failed poll backs off from `poll_interval`, doubling up to 6 hours, and honors `Retry-After`. A success resets the backoff. A poll failure never changes routing.
- No model requests and no token refreshes.

### Priority writes

- Writes run only in the background loop (every 30 seconds), never on the request path. A new observation can take up to 30 seconds to show in the priorities.
- Each write re-reads the credential file with `host.auth.get`, changes only the top-level `priority` key, and saves it under the same name with `host.auth.save`. If the read fails or the file is not valid JSON, the account is skipped.
- `host.auth.save` replaces the whole file, so a write must never race CLIProxyAPI's token refresh. A write is deferred while the token expires in less than 4h10m (CLIProxyAPI refreshes Claude tokens 4h before expiry), within 2 minutes of a refresh, or when the file changed between the read and a second read right before the save. Deferred writes are retried on the next pass.
- Accounts are written in rank order, top first. A failed save stops the pass, so a half-applied order is not left behind; the next pass retries. A credential that cannot be read or parsed only skips that account (`last_write: read_failed` or `invalid_json`).
- The log line names the account by email or ID with the old and new priority. Tokens are never logged.

### Fail-open

Any internal error (credential list failure, panic, malformed data) means no exclusions and no writes. CLIProxyAPI's routing then works as without the plugin, using the priorities last written to the credential files. Priorities are not rolled back automatically; back them up before switching to `active` if you want to restore them later.

## Modes

- **`shadow`** (default): the plugin reads quota and computes the plan, and the status page and panel show it. It never writes a priority and never excludes an account.
- **`active`**: priorities are written and reserve exclusions are applied.

Start in `shadow`, check the planned priorities on the panel, then switch to `active`.

## Requirements

- CLIProxyAPI v8.0.79 (plugin ABI 1, schema 10). The [compatibility workflow](#compatibility-check) tests the latest release every week.
- Linux amd64 or arm64 (glibc 2.34 or newer), macOS 12 or newer on Apple silicon or Intel, or Windows amd64.
- The Intel macOS build uses a patched Go toolchain. See [Intel Macs](#intel-macs).
- Recommended routing: `strategy: fill-first` and `session-affinity: true`. Other strategies still honor the priorities and exclusions, but only fill-first sends all new sessions to the top account.
- Polling needs direct network access to `api.anthropic.com`. Credentials with `proxy_url` are not polled (they can still be observed from headers). Credentials with `base_url` are not tracked at all: no reserve, no header observation and no priority writes, and polling ignores CLIProxyAPI's `proxy-url` and proxy environment variables.

## Install

Download `quota-reset-router_<version>_<os>_<arch>.zip` for your platform from the [latest release](https://github.com/WebDevCaptain/quota-reset-router/releases/latest) and check it against `checksums.txt`. Optionally verify its build provenance: `gh attestation verify <zip> --repo WebDevCaptain/quota-reset-router`.

### Linux

```sh
unzip quota-reset-router_<version>_linux_amd64.zip quota-reset-router.so
install -m 0644 quota-reset-router.so /path/to/cliproxyapi/plugins/
```

### macOS

```sh
unzip quota-reset-router_<version>_darwin_arm64.zip quota-reset-router.dylib
install -m 0644 quota-reset-router.dylib /path/to/cliproxyapi/plugins/
# Only if the file was downloaded through a browser and carries the quarantine flag:
xattr -d com.apple.quarantine /path/to/cliproxyapi/plugins/quota-reset-router.dylib
```

Use the `darwin_amd64` zip on Intel Macs. On Windows, place `quota-reset-router.dll` the same way.

The file must be a regular file in `plugins.dir`; symlinks are not loaded. Then merge into `config.yaml`:

```yaml
routing:
  strategy: fill-first
  session-affinity: true
plugins:
  enabled: true
  dir: plugins
  configs:
    quota-reset-router:
      enabled: true
      priority: 10
      mode: shadow
```

Restart CLIProxyAPI and run the [install check](#install-check). The plugin-level `priority` orders plugins, not accounts.

## Configuration

| Key | Default | Allowed | Purpose |
|---|---|---|---|
| `mode` | `shadow` | `shadow`, `active` | `shadow` only computes the plan. `active` writes priorities and applies exclusions. |
| `poll_interval` | `30m` | `5m` to `6h` | Poll an account only when its last observation is older than this. Also the first poll backoff. |
| `request_timeout` | `10s` | `1s` to `30s` | Timeout per poll. |
| `accounts` | empty | map | Per-account reserves, keyed by credential ID or email. |
| `accounts_file` | empty | absolute path or `~/...` | YAML file with the `accounts` map. Once read it replaces the inline `accounts`. |
| `management_url` | `http://127.0.0.1:8317` | http(s) URL | This CPA's address, for copying the file's reserves into its config. |
| `management_key_file` | empty | absolute path or `~/...` | File with this CPA's plaintext management key. Enables that copy. |

Each `accounts` entry takes `five_hour_reserve` and `weekly_reserve`, both percentages from `0` to `95`. Email keys match case-insensitively. When both an ID key and an email key match, the ID key wins. Unknown keys, including the keys of 0.3.x, are ignored.

Example: keep 10% of the 5-hour window and 15% of the weekly window of one account.

```yaml
plugins:
  configs:
    quota-reset-router:
      enabled: true
      priority: 10
      mode: active
      poll_interval: 30m
      accounts:
        me@example.com:
          five_hour_reserve: 10
          weekly_reserve: 15
```

### Sharing reserves between machines

Set `accounts_file` to keep the reserves in their own file, for example one synced by Syncthing between two machines that each run CLIProxyAPI:

```yaml
plugins:
  configs:
    quota-reset-router:
      enabled: true
      mode: active
      accounts_file: ~/cpa-shared/quota-reserves.yaml
```

```yaml
# ~/cpa-shared/quota-reserves.yaml
accounts:
  me@example.com:
    five_hour_reserve: 10
    weekly_reserve: 15
```

- The plugin checks the file's size and modification time on every pass (30 seconds) and re-reads it when they change. No restart is needed.
- Until the file is read successfully, the inline `accounts` apply. A missing or invalid file keeps the last good reserves, and the status shows `accounts_file_error`.
- The **Cotas** panel saves the reserves to the file through `POST /v0/management/plugins/quota-reset-router/accounts` with `{"accounts": {...}}`. The write goes to a temp file named `.<name>.*.tmp` in the same directory and is then renamed, so sync tools never see a partial file. `mode` and `poll_interval` stay in each machine's own config.
- Editing the reserves in CPA's own plugin settings also works. On reconfigure, the plugin saves to the file only the accounts whose values changed, so stale values for other accounts never overwrite the file. Removing an account there removes it from the file.
- With `management_key_file`, a file that differs from the inline `accounts` (an edit made on the other machine, or the file being newer at startup) is written into this CPA's config with `PATCH .../config {"accounts": ...}`, so the settings screen shows the reserves in effect. A failed write is retried every 5 minutes. Keep the key file at mode 600.

Change settings without a restart (top-level keys are merged, so `accounts` is replaced as a whole):

```text
PATCH /v0/management/plugins/quota-reset-router/config
{"mode": "active"}
```

A reconfigure keeps what the plugin already learned about each account.

## Panel and status

The management panel registers a **Cotas** page at `/v0/resource/plugins/quota-reset-router/panel` (Portuguese UI). For each account it shows the email, 5-hour and 7-day utilization and reset, where the data came from and how old it is, the reserves, whether the account is at its reserve, the current and planned priority, the last write result, and the poll backoff. It edits `mode`, `poll_interval` and the reserves.

```text
GET /v0/management/plugins/quota-reset-router/status
```

Requires Management API authentication. Returns the same per-account data plus the configuration, the last selection, and counters. Credential IDs and emails are included. OAuth tokens are not.

## Install check

[`scripts/check-installed.sh`](scripts/check-installed.sh) asks a running CLIProxyAPI whether the plugin is registered:

```sh
CPA_MANAGEMENT_KEY=... scripts/check-installed.sh
# quota-reset-router 0.6.0 registered, mode shadow
```

It reads `CPA_URL` (default `http://127.0.0.1:8317`) and `CPA_MANAGEMENT_KEY`, calls `GET /v8/management/plugins`, and exits non-zero unless `quota-reset-router` is listed with `registered: true`. The mode comes from the status endpoint. The key is never printed. It needs `curl` and `python3`.

## Compatibility check

[`.github/workflows/compat.yml`](.github/workflows/compat.yml) runs every Monday and on demand. It resolves the latest CLIProxyAPI release, builds the Linux amd64 library and runs `make host-test` against that release. On failure it opens an issue titled `Compatibility break with CLIProxyAPI <version>`, or comments on the open one with that title.

## Disable or upgrade

- **Disable** without a restart. CLIProxyAPI stops consulting the plugin after it reloads the configuration. Priorities already written stay in the credential files until you change them.

  ```text
  PATCH /v0/management/plugins/quota-reset-router/enabled
  {"enabled": false}
  ```

- **Upgrade:** stop CLIProxyAPI, replace the plugin file, start CLIProxyAPI.

## Limitations

- Native plugins run inside the CLIProxyAPI process with access to its credentials and traffic. Expected errors fail open; a native crash can still affect the proxy.
- The plugin owns the `priority` field of Claude credentials in `active` mode. Manual priorities on those accounts are overwritten.
- Excluding an account at its reserve also moves the sessions already bound to it, because CLIProxyAPI drops excluded accounts from session affinity too.
- With `session-affinity-rebind-on: never`, a session bound to an account at its reserve fails instead of moving. Keep the default rebind setting when you configure reserves.
- `host.auth.save` writes to the top level of the auth directory, so credentials in subdirectories are never written (`last_write: skipped_unsupported_path`) and keep their priority.
- CLIProxyAPIHome dispatch does not consult plugin schedulers.
- The usage endpoint and the rate-limit headers are not stable public APIs. Unrecognized data is ignored.

## Development

Build and test targets take `TARGET`: `linux_amd64` (default), `linux_arm64`, `darwin_amd64`, `darwin_arm64`, or `windows_amd64`.

- Linux and Windows builds, `linux-test`, `native-test`, and Linux `host-test` run in a pinned Docker image.
- macOS builds need a macOS host with Go 1.21+ and the Xcode Command Line Tools. Go 1.26.10 is downloaded automatically.
- `darwin_amd64` first builds the patched toolchain into `dist/_go-tls` (about a minute, once per checkout), then uses it for `make test` and `make build`.
- `make test` needs Go 1.26+ and a C toolchain.

```sh
make test         # gofmt, go vet, and unit tests with the race detector
make linux-test   # same, in the pinned Linux container
make build        # writes dist/<target>/quota-reset-router.<so|dylib|dll>
make native-test  # Linux only: loads the library through the plugin C ABI
make host-test    # Linux, or macOS CI: runs the official CLIProxyAPI release with the library
make zip          # writes dist/release/quota-reset-router_<version>_<target>.zip
make load-test    # loads the zip into the official CLIProxyAPI; TARGET must match this machine
```

- `host-test` and `load-test` download the official CLIProxyAPI release (`CPA_VERSION`, default 8.0.79) and verify its checksum.
- `host-test` runs CLIProxyAPI with fill-first and session affinity against a local fixture whose quota endpoint is down, so the plugin learns only from response headers. It checks registration, the priority rewrite (and that the credential files keep their other fields), that a bound session stays on its account while a new session goes to the new top account, and the reserve exclusion with its last-account fallback.
- `native-test` and `host-test` run with networking disabled, local TLS fixtures, and synthetic credentials. On macOS, `host-test` instead needs `api.anthropic.com` pinned to 127.0.0.1 and the fixture certificate trusted in the System keychain, which CI sets up; `SOAK_SECONDS` adds a soak under GC pressure.
- `load-test` starts CLIProxyAPI without credentials, so the plugin makes no network requests.
- CI builds every target and loads each zip into the official CLIProxyAPI on its own platform.
- `make clean` removes build output.

### Intel Macs

Stock Go on macOS amd64 keeps each thread's current goroutine in one fixed thread-local slot, slot 6, which Apple reserves for Go. Every Go runtime in a process uses that slot. CLIProxyAPI is itself a Go program, so a plugin built with stock Go runs on CLIProxyAPI's goroutines and heap, and CLIProxyAPI crashes at startup (`fatal error: addspecial on invalid pointer`; CLIProxyAPI's own Go scheduler example fails with `unknown caller pc`). Linux, Windows, and Apple silicon give each runtime its own slot, so they are unaffected.

The `darwin_amd64` library is built with Go 1.26.10 plus [`scripts/go-tls-slot.patch`](scripts/go-tls-slot.patch). The patch changes two constants so the plugin's runtime uses slot 11, which Apple also reserves and leaves unused. CLIProxyAPI keeps slot 6. The toolchain is built from the checksum-verified upstream source, and the build fails if any goroutine access in the library still uses slot 6.

## Release

The CLIProxyAPI plugin store installs from this repository's latest published GitHub release. Each release contains `checksums.txt` and one `quota-reset-router_<version>_<os>_<arch>.zip` per target, holding the plugin, `LICENSE`, and `THIRD_PARTY_NOTICES.md`.

1. Set `pluginVersion` in `config.go`, for example `0.2.0`, and merge to `main`.
2. Push the tag `v<version>`. CI builds and tests every target, attests build provenance, and creates a draft release with the assets.
3. Review the draft and publish it.

`make release-assets` builds the same set locally on a macOS host with Docker.

## Third-party notices

Binary releases link the CLIProxyAPI plugin SDK (MIT), `gopkg.in/yaml.v3` (MIT and Apache-2.0), and the Go standard library (BSD-3-Clause; patched on `darwin_amd64`). Windows builds also statically link parts of the MinGW-w64 runtime. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
