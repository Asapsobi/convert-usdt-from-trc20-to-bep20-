#!/usr/bin/env bash
# Runs the whole product on this machine, with generated local keys and a
# simulated conversion vendor, so no account or secret is needed.
#
#   scripts/dev.sh setup     one-time: databases, config files, keys, dependencies
#   scripts/dev.sh start     build and start every service and the storefront
#   scripts/dev.sh status    what is running, and the URLs
#   scripts/dev.sh logs X    follow one service's log (e.g. relayd)
#   scripts/dev.sh stop      stop everything
#   scripts/dev.sh reset     stop, then delete the local databases and config
#
# Settings (environment variables, all optional):
#   DEV_PG_URL           Postgres server, e.g. postgres://postgres@localhost:5432
#                        (default: tries postgres@ and $USER@ on localhost:5432)
#   DEV_DB_PREFIX        database name prefix (default usdtconv_)
#   DEV_PORT_BASE        first service port (default 18180; uses base..base+10)
#   DEV_STOREFRONT_PORT  storefront port (default 5181)
# setup remembers them in tmp/dev/config.env, so start/stop reuse them.
#
# See docs/local-setup.md. Never send real funds to addresses from a local
# setup: its keys sit in plain files on this machine.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STATE="$ROOT/tmp/dev"
BIN="$STATE/bin"
LOGS="$STATE/logs"
PIDS="$STATE/pids"
CONFIG="$STATE/config.env"

# Service name, its directory, its server command, and its port offset.
SERVICES="ledger s1 screening depositwatcher tronwatcher relayd opsconsole"
svc_dir() { echo "$1"; }
svc_cmd() {
	case "$1" in
	ledger) echo ledgerd ;; s1) echo s1d ;; screening) echo screend ;;
	depositwatcher) echo watcherd ;; tronwatcher) echo tronwatcherd ;;
	relayd) echo relayd ;; opsconsole) echo opsconsoled ;;
	esac
}
svc_port_offset() {
	case "$1" in
	ledger) echo 0 ;; s1) echo 5 ;; tronwatcher) echo 6 ;; relayd) echo 7 ;;
	depositwatcher) echo 8 ;; screening) echo 9 ;; opsconsole) echo 10 ;;
	esac
}
# Modules that own a database, and the variable their migrate command reads.
DB_MODULES="ledger screening s1 depositwatcher tronwatcher relayd"
db_var() {
	case "$1" in
	ledger) echo LEDGER_DATABASE_URL ;; screening) echo SCREENING_DATABASE_URL ;;
	s1) echo S1_DATABASE_URL ;; depositwatcher) echo WATCHER_DATABASE_URL ;;
	tronwatcher) echo TRONWATCHER_DATABASE_URL ;; relayd) echo RELAYD_DATABASE_URL ;;
	esac
}

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

load_config() {
	if [ -f "$CONFIG" ]; then
		# shellcheck disable=SC1090
		. "$CONFIG"
	fi
	DEV_DB_PREFIX="${DEV_DB_PREFIX:-usdtconv_}"
	DEV_PORT_BASE="${DEV_PORT_BASE:-18180}"
	DEV_STOREFRONT_PORT="${DEV_STOREFRONT_PORT:-5181}"
}

port_of() { echo $((DEV_PORT_BASE + $(svc_port_offset "$1"))); }
db_url() { echo "$DEV_PG_URL/${DEV_DB_PREFIX}$1?sslmode=disable"; }
random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
psql_admin() { psql -X -q -v ON_ERROR_STOP=1 "$DEV_PG_URL/postgres" "$@"; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is not installed -- $2"; }

check_tools() {
	step "Checking tools"
	need go "install Go 1.27 or newer: https://go.dev/dl/"
	need node "install Node.js 20 or newer: https://nodejs.org/"
	need npm "it comes with Node.js"
	need psql "install PostgreSQL 16 (macOS: Postgres.app or 'brew install postgresql@16')"
	local gov
	gov="$(go env GOVERSION | sed 's/^go//')"
	case "$gov" in
	1.2[7-9]* | 1.[3-9]* | [2-9].*) ;;
	*) die "Go $gov is too old; install Go 1.27 or newer" ;;
	esac
	local nodev
	nodev="$(node -p 'process.versions.node.split(".")[0]')"
	[ "$nodev" -ge 20 ] || die "Node.js $nodev is too old; install Node.js 20 or newer"
	say "go $gov, node $(node --version), $(psql --version | head -1)"
}

find_postgres() {
	step "Finding PostgreSQL"
	if [ -n "${DEV_PG_URL:-}" ]; then
		psql_admin -c 'select 1' >/dev/null 2>&1 || die "cannot connect to $DEV_PG_URL/postgres"
	else
		local candidate
		for candidate in "postgres://postgres@localhost:5432" "postgres://${USER:-postgres}@localhost:5432"; do
			if psql -X -q "$candidate/postgres" -c 'select 1' >/dev/null 2>&1; then
				DEV_PG_URL="$candidate"
				break
			fi
		done
		[ -n "${DEV_PG_URL:-}" ] || die "no PostgreSQL on localhost:5432 accepted a password-less login.
Start PostgreSQL, or point DEV_PG_URL at it, e.g.
  DEV_PG_URL=postgres://me:secret@localhost:5432 scripts/dev.sh setup"
	fi
	say "using $DEV_PG_URL"
	local can
	can="$(psql_admin -tAc 'select rolsuper or (rolcreatedb and rolcreaterole) from pg_roles where rolname = current_user')"
	[ "$can" = "t" ] || die "the PostgreSQL user must be able to create databases and roles (a local superuser)"
}

build() {
	step "Building"
	mkdir -p "$BIN"
	local s
	for s in $SERVICES; do
		(cd "$ROOT/$(svc_dir "$s")" && go build -o "$BIN/$(svc_cmd "$s")" "./cmd/$(svc_cmd "$s")")
		say "built $(svc_cmd "$s")"
	done
	for s in $DB_MODULES; do
		(cd "$ROOT/$s" && go build -o "$BIN/$s-migrate" ./cmd/migrate)
	done
	(cd "$ROOT/s1" && go build -o "$BIN/seed-slot-key" ./cmd/seed-slot-key && go build -o "$BIN/dev-keys" ./cmd/dev-keys)
	(cd "$ROOT/opsconsole" && go build -o "$BIN/hashpassword" ./cmd/hashpassword)
	say "built the migrate and setup tools"
}

create_databases() {
	step "Creating databases"
	local m name
	for m in $DB_MODULES; do
		name="${DEV_DB_PREFIX}$m"
		if [ "$(psql_admin -tAc "select 1 from pg_database where datname = '$name'")" = "1" ]; then
			say "$name exists"
		else
			psql_admin -c "create database \"$name\"" >/dev/null
			say "created $name"
		fi
	done
}

# esc makes a value safe on the right-hand side of a sed s|...|...| command.
esc() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }

write_config_files() {
	step "Writing config files"
	local s
	for s in $SERVICES; do
		if [ -f "$ROOT/$s/.env.local" ]; then
			say "$s/.env.local exists -- keeping it (scripts/dev.sh reset starts over)"
			return 0
		fi
	done

	umask 077
	local subs="$STATE/substitutions.sed" render="$STATE/render"
	trap 'rm -rf "$STATE/substitutions.sed" "$STATE/render"' EXIT
	: >"$subs"
	mkdir -p "$render"
	add() { printf 's|{{%s}}|%s|g\n' "$1" "$(esc "$2")" >>"$subs"; }

	for s in $SERVICES; do
		add "PORT_$(echo "$s" | tr '[:lower:]' '[:upper:]' | sed 's/DEPOSITWATCHER/WATCHER/')" "$(port_of "$s")"
	done
	add PORT_BROKER $((DEV_PORT_BASE + 1))
	add PORT_DISPATCHER $((DEV_PORT_BASE + 3))
	add DB_LEDGER "$(db_url ledger)"
	add DB_SCREENING "$(db_url screening)"
	add DB_S1 "$(db_url s1)"
	add DB_WATCHER "$(db_url depositwatcher)"
	add DB_TRONWATCHER "$(db_url tronwatcher)"
	add DB_RELAYD "$(db_url relayd)"

	local t
	for t in LEDGER_RELAYD LEDGER_WATCHER LEDGER_TRONWATCHER LEDGER_SCREENING LEDGER_OPSCONSOLE \
		WATCHER_RELAYD WATCHER_OPSCONSOLE TRONWATCHER_RELAYD TRONWATCHER_OPSCONSOLE SCREENING_OPSCONSOLE \
		S1_RELAYD S1_OPSCONSOLE S1_WATCHER S1_TRONWATCHER S1_APPROVER \
		RELAYD_ADMIN RELAYD_OPSCONSOLE RELAYD_SCREENING; do
		add "TOKEN_$t" "$(random_hex 24)"
	done
	add SECRET_OC_SESSION "$(random_hex 32)"
	add OC_AUDIT_LOG_PATH "$STATE/opsconsole-audit.log"

	local password
	password="$(random_hex 9)"
	printf '%s\n' "$password" >"$STATE/admin-password"
	chmod 600 "$STATE/admin-password"
	add OC_ADMIN_HASH "$(printf '%s' "$password" | "$BIN/hashpassword")"

	local line
	while IFS= read -r line; do
		add "${line%%=*}" "${line#*=}"
	done < <("$BIN/dev-keys")

	# Render every file first, so a failure leaves none half-written.
	for s in $SERVICES; do
		sed -f "$subs" "$ROOT/$s/.env.example" >"$render/$s"
		if grep -q '{{[A-Z0-9_]*}}' "$render/$s"; then
			die "$s/.env.example has a value setup does not know: $(grep -o '{{[A-Z0-9_]*}}' "$render/$s" | head -1)"
		fi
	done
	for s in $SERVICES; do
		mv "$render/$s" "$ROOT/$s/.env.local"
		say "wrote $s/.env.local"
	done
	printf 'VITE_RELAYD_BASE_URL=http://127.0.0.1:%s\nVITE_LOCAL_TEST_BANNER=true\n' "$(port_of relayd)" >"$ROOT/relay-storefront/.env.local"
	say "wrote relay-storefront/.env.local"
	rm -rf "$subs" "$render"
	trap - EXIT
}

migrate() {
	step "Migrating databases"
	local m
	for m in $DB_MODULES; do
		(cd "$ROOT/$m" && env "$(db_var "$m")=$(db_url "$m")" "$BIN/$m-migrate" up >"$LOGS/$m-migrate.log" 2>&1) ||
			die "migrating $m failed -- see $LOGS/$m-migrate.log"
		say "$m is up to date"
	done
}

register_treasury() {
	step "Registering the treasury key (S1 slot 1)"
	(cd "$ROOT/s1" && set -a && . ./.env.local && set +a && "$BIN/seed-slot-key" -id 1 -key-id slot-1)
}

install_storefront() {
	step "Installing the storefront's packages"
	(cd "$ROOT/relay-storefront" && npm ci --no-audit --no-fund --loglevel=error)
}

cmd_setup() {
	load_config
	check_tools
	find_postgres
	mkdir -p "$STATE" "$LOGS" "$PIDS"
	cat >"$CONFIG" <<EOF
DEV_PG_URL='$DEV_PG_URL'
DEV_DB_PREFIX='$DEV_DB_PREFIX'
DEV_PORT_BASE='$DEV_PORT_BASE'
DEV_STOREFRONT_PORT='$DEV_STOREFRONT_PORT'
EOF
	build
	create_databases
	write_config_files
	migrate
	register_treasury
	install_storefront
	step "Setup finished"
	say "Next: scripts/dev.sh start"
}

pid_alive() { [ -f "$PIDS/$1" ] && kill -0 "$(cat "$PIDS/$1")" 2>/dev/null; }

wait_healthy() {
	local name="$1" url="$2" i
	for i in $(seq 1 60); do
		if curl -fsS -o /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		pid_alive "$name" || die "$name exited while starting -- see $LOGS/$name.log"
		sleep 0.5
	done
	die "$name did not answer $url within 30s -- see $LOGS/$name.log"
}

start_service() {
	local s="$1"
	if pid_alive "$s"; then
		say "$s is already running"
		return 0
	fi
	(cd "$ROOT/$s" && set -a && . ./.env.local && set +a && exec "$BIN/$(svc_cmd "$s")") >"$LOGS/$s.log" 2>&1 &
	echo $! >"$PIDS/$s"
	wait_healthy "$s" "http://127.0.0.1:$(port_of "$s")/healthz"
	say "$s is up on port $(port_of "$s")"
}

cmd_start() {
	load_config
	[ -f "$CONFIG" ] && [ -f "$ROOT/relayd/.env.local" ] || die "run scripts/dev.sh setup first"
	need curl "install curl"
	mkdir -p "$LOGS" "$PIDS"
	build
	step "Starting services"
	local s
	for s in $SERVICES; do
		start_service "$s"
	done
	if pid_alive storefront; then
		say "storefront is already running"
	else
		(cd "$ROOT/relay-storefront" && exec ./node_modules/.bin/vite --host 127.0.0.1 --port "$DEV_STOREFRONT_PORT" --strictPort) >"$LOGS/storefront.log" 2>&1 &
		echo $! >"$PIDS/storefront"
		wait_healthy storefront "http://127.0.0.1:$DEV_STOREFRONT_PORT/"
		say "storefront is up on port $DEV_STOREFRONT_PORT"
	fi
	print_urls
}

print_urls() {
	say ""
	say "Storefront:    http://127.0.0.1:$DEV_STOREFRONT_PORT"
	say "Admin panel:   http://127.0.0.1:$(port_of opsconsole)  (user: admin, password: $(cat "$STATE/admin-password" 2>/dev/null || echo '?'))"
	say "relayd API:    http://127.0.0.1:$(port_of relayd)"
	say "Logs:          $LOGS  (scripts/dev.sh logs relayd)"
}

cmd_status() {
	load_config
	local s port state
	for s in $SERVICES storefront; do
		if [ "$s" = storefront ]; then port="$DEV_STOREFRONT_PORT"; else port="$(port_of "$s")"; fi
		if pid_alive "$s"; then state="running"; else state="stopped"; fi
		printf '%-15s %-8s port %s\n' "$s" "$state" "$port"
	done
	print_urls
}

cmd_logs() {
	load_config
	[ -n "${1:-}" ] || die "which service? one of: $SERVICES storefront"
	[ -f "$LOGS/$1.log" ] || die "no log for $1 yet"
	tail -n 100 -f "$LOGS/$1.log"
}

cmd_stop() {
	load_config
	local s pid
	for s in storefront $SERVICES; do
		if pid_alive "$s"; then
			pid="$(cat "$PIDS/$s")"
			kill "$pid" 2>/dev/null || true
			for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
			kill -9 "$pid" 2>/dev/null || true
			say "stopped $s"
		fi
		rm -f "$PIDS/$s"
	done
}

cmd_reset() {
	load_config
	if [ "${1:-}" != "-y" ]; then
		printf 'This deletes the local databases (%s*), every .env.local file, and tmp/dev. Continue? [y/N] ' "$DEV_DB_PREFIX"
		local answer
		read -r answer
		[ "$answer" = "y" ] || [ "$answer" = "Y" ] || die "cancelled"
	fi
	cmd_stop
	if [ -n "${DEV_PG_URL:-}" ]; then
		local m
		for m in $DB_MODULES; do
			psql_admin -c "drop database if exists \"${DEV_DB_PREFIX}$m\"" >/dev/null && say "dropped ${DEV_DB_PREFIX}$m"
		done
	fi
	local s
	for s in $SERVICES relay-storefront; do rm -f "$ROOT/$s/.env.local"; done
	rm -rf "$STATE"
	say "reset done -- scripts/dev.sh setup starts over"
}

case "${1:-help}" in
setup) cmd_setup ;;
start) cmd_start ;;
status) cmd_status ;;
logs) cmd_logs "${2:-}" ;;
stop) cmd_stop ;;
reset) cmd_reset "${2:-}" ;;
*) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//' ;;
esac
