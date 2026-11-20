#!/bin/sh
# Checks that a running CLIProxyAPI has quota-reset-router registered.
#
# Usage: CPA_MANAGEMENT_KEY=... [CPA_URL=http://127.0.0.1:8317] scripts/check-installed.sh
# Exits non-zero unless the plugin is listed with registered: true.
# Prints the plugin version and mode. The key is sent only in a request header
# read from stdin, so it never appears in the output or the process list.
set +x
set -eu

PLUGIN=quota-reset-router
CPA_URL="${CPA_URL:-http://127.0.0.1:8317}"
CPA_URL="${CPA_URL%/}"
if [ -z "${CPA_MANAGEMENT_KEY:-}" ]; then
	echo "CPA_MANAGEMENT_KEY is not set" >&2
	exit 2
fi
case "$CPA_MANAGEMENT_KEY" in
*"$(printf '\r')"* | *"
"*)
	echo "CPA_MANAGEMENT_KEY must be a single line" >&2
	exit 2
	;;
esac

# The header is passed literally on stdin (-H @-), not parsed as curl config syntax.
get() {
	printf 'Authorization: Bearer %s\n' "$CPA_MANAGEMENT_KEY" |
		curl --silent --show-error --fail --max-time 10 -H @- "$CPA_URL$1"
}

if ! plugins="$(get /v8/management/plugins)"; then
	echo "could not list plugins at $CPA_URL" >&2
	exit 1
fi

version="$(printf '%s' "$plugins" | python3 -c '
import json, sys
plugins = json.load(sys.stdin).get("plugins") or []
entry = next((p for p in plugins if p.get("id") == sys.argv[1]), None)
if not entry:
    sys.exit(sys.argv[1] + " is not installed")
if entry.get("registered") is not True:
    sys.exit(sys.argv[1] + " is installed but not registered")
print((entry.get("metadata") or {}).get("version") or "unknown")
' "$PLUGIN")"

mode=unknown
if status="$(get "/v0/management/plugins/$PLUGIN/status")"; then
	mode="$(printf '%s' "$status" | python3 -c '
import json, sys
print(json.load(sys.stdin).get("mode") or "unknown")
' || echo unknown)"
fi

echo "$PLUGIN $version registered, mode $mode"
