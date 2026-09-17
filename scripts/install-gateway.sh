#!/bin/sh
# Installs (or updates) a Relay tunnel gateway on a public Linux server.
#
# Relay shows this command with your pairing token in Tunnels → Set up a tunnel:
#
#   curl -fsSL https://raw.githubusercontent.com/ollieInstantOffr/app-relay/main/scripts/install-gateway.sh \
#     | sudo sh -s -- --token rlypair1_…
#
# What it does:
#   1. installs Docker (get.docker.com) when it's missing,
#   2. downloads Relay's source at the same version as your Relay,
#   3. builds and starts the gateway container (relay-gateway) with the token,
#   4. opens ports 80, 443 and 7443 (TCP and UDP) in ufw or firewalld when active,
#   5. waits until the gateway runs and prints its key.
#
# Options:
#   --token TOKEN     one-time pairing token from Relay
#   --ref REF         git branch, tag or commit to build (default: main)
#   --version VER     version label for the build (default: the ref)
#   --reset           forget an earlier pairing first (to pair with a new token)
#   --dir DIR         install directory (default: /opt/relay-gateway)
#   --no-firewall     don't touch ufw / firewalld
#   --uninstall       stop and remove the gateway (keeps its key unless --purge)
#   --purge           with --uninstall: also delete the gateway's key and state
set -eu

REPO="ollieInstantOffr/app-relay"
DIR=/opt/relay-gateway
REF=main
VERSION=
TOKEN=
RESET=0
FIREWALL=1
UNINSTALL=0
PURGE=0

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31mError:\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--token) TOKEN=${2:-}; shift 2 ;;
	--ref) REF=${2:-}; shift 2 ;;
	--version) VERSION=${2:-}; shift 2 ;;
	--dir) DIR=${2:-}; shift 2 ;;
	--reset) RESET=1; shift ;;
	--no-firewall) FIREWALL=0; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	--purge) PURGE=1; shift ;;
	-h | --help) sed -n '2,27p' "$0" 2>/dev/null || true; exit 0 ;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
done

[ "$(uname -s)" = Linux ] || die "the gateway runs on Linux servers"
[ "$(id -u)" = 0 ] || die "run it as root, e.g. with sudo"
case "$REF" in *[!A-Za-z0-9._/-]*) die "invalid --ref" ;; esac
case "$TOKEN" in *[!A-Za-z0-9_-]*) die "invalid --token" ;; esac

COMPOSE="docker compose -p relay-gateway --env-file $DIR/gateway.env -f $DIR/src/deploy/gateway/docker-compose.yml"

if [ "$UNINSTALL" = 1 ]; then
	if [ -f "$DIR/src/deploy/gateway/docker-compose.yml" ]; then
		say "Stopping the gateway"
		if [ "$PURGE" = 1 ]; then $COMPOSE down --volumes; else $COMPOSE down; fi
	fi
	rm -rf "$DIR"
	say "Removed. Delete the gateway in Relay too."
	exit 0
fi

# 1. Docker
if ! command -v docker >/dev/null 2>&1; then
	say "Installing Docker"
	command -v curl >/dev/null 2>&1 || die "curl is required"
	curl -fsSL https://get.docker.com | sh
fi
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (install the docker-compose-plugin package)"
systemctl enable --now docker >/dev/null 2>&1 || true

# 2. Source
say "Downloading Relay ($REF)"
mkdir -p "$DIR"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://codeload.github.com/$REPO/tar.gz/$REF" -o "$tmp/src.tar.gz" || die "couldn't download $REPO at $REF"
mkdir "$tmp/src"
tar -xzf "$tmp/src.tar.gz" -C "$tmp/src" --strip-components=1
rm -rf "$DIR/src"
mv "$tmp/src" "$DIR/src"

# 3. Settings and start
umask 077
{
	echo "RELAY_VERSION=${VERSION:-$REF}"
	echo "RELAY_COMMIT=$REF"
	if [ -n "$TOKEN" ]; then
		echo "RELAY_GATEWAY_PAIR_TOKEN=$TOKEN"
	elif [ -f "$DIR/gateway.env" ]; then
		grep '^RELAY_GATEWAY_PAIR_TOKEN=' "$DIR/gateway.env" || true
	fi
} >"$DIR/gateway.env.new"
mv "$DIR/gateway.env.new" "$DIR/gateway.env"
umask 022

if [ "$RESET" = 1 ] && docker inspect relay-gateway >/dev/null 2>&1; then
	say "Forgetting the previous pairing"
	docker exec relay-gateway relay gateway reset || warn "reset failed; continuing"
fi

say "Building and starting the gateway (this takes a few minutes the first time)"
$COMPOSE up -d --build --force-recreate

# 4. Firewall
if [ "$FIREWALL" = 1 ]; then
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
		say "Opening ports in ufw"
		for p in 80/tcp 443/tcp 7443/tcp 7443/udp; do ufw allow "$p" >/dev/null; done
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		say "Opening ports in firewalld"
		for p in 80/tcp 443/tcp 7443/tcp 7443/udp; do firewall-cmd --quiet --permanent --add-port="$p"; done
		firewall-cmd --quiet --reload
	fi
fi

# 5. Check
say "Waiting for the gateway"
i=0
while [ $i -lt 30 ]; do
	if [ "$(docker inspect -f '{{.State.Running}}' relay-gateway 2>/dev/null)" = true ] && docker exec relay-gateway relay gateway info >/dev/null 2>&1; then
		break
	fi
	i=$((i + 1))
	sleep 2
done
if ! docker exec relay-gateway relay gateway info 2>/dev/null; then
	docker logs --tail 30 relay-gateway >&2 || true
	die "the gateway didn't start; see the log above"
fi

cat <<'EOF'

The gateway is running. Go back to Relay: it pairs automatically.

If your hosting provider has its own firewall (security groups), allow
80/tcp, 443/tcp, 7443/tcp and 7443/udp there too.
EOF
