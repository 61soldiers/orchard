#!/usr/bin/env bash
# Interactive first-run setup for Orchard: starts the server, waits for the
# Apple Music daemon to install, then walks through signing in.
#
# Safe to re-run at any time.
set -euo pipefail

BASE_URL=${ORCHARD_URL:-http://127.0.0.1:8080}
ENV_FILE=.env

bold() { printf '\033[1m%s\033[0m\n' "$1"; }
info() { printf '  %s\n' "$1"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }
die() {
	printf '\n  \033[31m✗ %s\033[0m\n\n' "$1" >&2
	exit 1
}

# JSON has no top-level key ordering guarantee here, but Orchard always emits
# the session state before the nested wrapper object, so split on that.
session_state() { printf '%s' "${1%%\"wrapper\"*}" | grep -o '"state":"[^"]*"' | head -1 | cut -d'"' -f4; }
wrapper_state() { printf '%s' "${1#*\"wrapper\"}" | grep -o '"state":"[^"]*"' | head -1 | cut -d'"' -f4; }
field() { printf '%s' "$1" | grep -o "\"$2\":\"[^\"]*\"" | head -1 | cut -d'"' -f4; }

# Escapes backslashes and double quotes so a password can go into a JSON body.
json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

api() {
	local method=$1 path=$2 body=${3:-}
	if [ -n "$body" ]; then
		curl -sS -m 180 -X "$method" "$BASE_URL$path" \
			-H "Authorization: Bearer $KEY" \
			-H 'Content-Type: application/json' \
			-d "$body"
	else
		curl -sS -m 180 -X "$method" "$BASE_URL$path" -H "Authorization: Bearer $KEY"
	fi
}

# ---------------------------------------------------------------- preflight --
echo
bold "Orchard setup"
echo

OS=$(uname -s)

command -v curl >/dev/null 2>&1 || die "curl is not installed. Install it and run this again."
command -v docker >/dev/null 2>&1 || die "Docker is not installed. See https://docs.docker.com/get-docker/"
docker compose version >/dev/null 2>&1 || die "The Docker Compose plugin is missing. See https://docs.docker.com/compose/install/"

if ! docker info >/dev/null 2>&1; then
	case "$OS" in
	Darwin) die "Docker Desktop is not running. Open it from Applications, wait for the menu-bar whale to stop animating, then run this again." ;;
	*) die "Docker is not running, or your user cannot reach it. Try: sudo usermod -aG docker \$USER, then log out and back in." ;;
	esac
fi

case "$(uname -m)" in
x86_64 | aarch64 | arm64) ;;
*) die "Orchard needs a 64-bit Intel/AMD or ARM machine. Yours reports $(uname -m)." ;;
esac

# The daemon needs unprivileged user namespaces. On Linux that is a host-kernel
# setting we can read now; on macOS the daemon runs inside Docker Desktop's Linux
# VM, which ships with them on — a real problem there surfaces below when the
# component install never reaches "ready".
if [ "$OS" = "Linux" ]; then
	ns=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo 0)
	[ "$ns" -gt 0 ] 2>/dev/null || die "Unprivileged user namespaces are disabled on this machine, and the Apple Music daemon needs them. Ask your administrator to enable user.max_user_namespaces."
fi

ok "System looks good"

# --------------------------------------------------------------- access key --
if [ ! -f "$ENV_FILE" ]; then
	cp .env.example "$ENV_FILE"
fi

if ! grep -q '^ORCHARD_API_KEY=.\{24,\}' "$ENV_FILE"; then
	if command -v openssl >/dev/null 2>&1; then
		generated=$(openssl rand -base64 48 | tr -d '=+/\n')
	else
		generated=$(head -c 48 /dev/urandom | base64 | tr -d '=+/\n')
	fi
	# Rewrite in place without `sed -i` — its syntax differs on macOS/BSD.
	# Replace the key line if present, otherwise append it.
	tmp=$(mktemp)
	awk -v repl="ORCHARD_API_KEY=$generated" '
		/^ORCHARD_API_KEY=/ { print repl; done = 1; next }
		{ print }
		END { if (!done) print repl }
	' "$ENV_FILE" >"$tmp" && mv "$tmp" "$ENV_FILE" || {
		rm -f "$tmp"
		die "Could not write the access key to $ENV_FILE"
	}
	ok "Generated an access key and saved it to $ENV_FILE"
fi

KEY=$(grep '^ORCHARD_API_KEY=' "$ENV_FILE" | head -1 | cut -d= -f2-)
[ -n "$KEY" ] || die "Could not read ORCHARD_API_KEY from $ENV_FILE"

# ------------------------------------------------------------------- server --
info "Starting the server (the first run builds it, which can take a few minutes)..."
docker compose up -d >/dev/null 2>&1 || die "Docker failed to start Orchard. Run 'docker compose up' to see why."

for _ in $(seq 1 60); do
	if curl -fsS -m 5 "$BASE_URL/healthz" >/dev/null 2>&1; then break; fi
	sleep 2
done
curl -fsS -m 5 "$BASE_URL/healthz" >/dev/null 2>&1 ||
	die "The server did not come up. Run 'docker compose logs' to see why."
ok "Server is running at $BASE_URL"

# ---------------------------------------------------- apple music component --
info "Installing the Apple Music component (about 50 MB, one time only)..."
status=""
for _ in $(seq 1 100); do
	status=$(api GET /v1/apple/status || true)
	case "$(wrapper_state "$status")" in
	ready) break ;;
	unsupported) die "This machine's processor is not supported by the Apple Music component." ;;
	failed) die "Download failed: $(field "$status" error). Check the machine's internet connection and run this again." ;;
	esac
	sleep 3
done
[ "$(wrapper_state "$status")" = "ready" ] ||
	die "The Apple Music component did not finish installing. Run 'docker compose logs' to see why. On macOS and Windows, check that Docker Desktop is on its Linux engine (the default) and has enough memory allotted in Settings."
ok "Apple Music component installed"

# -------------------------------------------------------------------- login --
if [ "$(session_state "$status")" = "ready" ]; then
	echo
	ok "Already signed in to Apple Music — nothing else to do."
	echo
	bold "Your access key"
	info "$KEY"
	echo
	exit 0
fi

echo
bold "Sign in to Apple Music"
info "Use the Apple ID with your Apple Music subscription."
info "Your password is sent to Apple and is never saved by Orchard."
echo

printf '  Apple ID (email): '
read -r APPLE_ID
[ -n "$APPLE_ID" ] || die "No Apple ID entered."
case "$APPLE_ID" in *:*) die "An Apple ID cannot contain a colon." ;; esac

printf '  Password (hidden): '
read -rs APPLE_PW
echo
[ -n "$APPLE_PW" ] || die "No password entered."

echo
info "Signing in. If Apple asks for a code, it will appear on your devices now."
body=$(printf '{"appleId":"%s","password":"%s"}' "$(json_escape "$APPLE_ID")" "$(json_escape "$APPLE_PW")")
resp=$(api POST /v1/apple/login "$body" || true)
unset APPLE_PW

case "$(session_state "$resp")" in
ready)
	ok "Signed in"
	;;
awaiting_2fa)
	echo
	warn "Apple sent a verification code to your devices."
	warn "You have 60 seconds to enter it."
	echo
	printf '  Verification code: '
	read -r CODE
	[ -n "$CODE" ] || die "No code entered. Run ./setup.sh again to retry."
	resp=$(api POST /v1/apple/2fa "{\"code\":\"$(json_escape "$CODE")\"}" || true)
	[ "$(session_state "$resp")" = "ready" ] ||
		die "Sign-in failed: $(field "$resp" error). Run ./setup.sh again to retry."
	ok "Signed in"
	;;
*)
	die "Sign-in failed: $(field "$resp" error). Check the Apple ID and password, then run ./setup.sh again."
	;;
esac

# ------------------------------------------------------------------- finish --
echo
bold "All set"
storefront=$(field "$resp" storefront)
[ -n "$storefront" ] && info "Apple Music store: $storefront"
echo
info "Server address:  $BASE_URL"
info "Access key:      $KEY"
echo
info "Keep the access key private — it grants access to your Apple Music account."
info "It is also saved in $ENV_FILE."
echo
