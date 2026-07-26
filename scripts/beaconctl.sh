#!/bin/sh
# Beacon control script — POSIX sh, busybox-friendly (no pkill/killall needed).
# Lives next to the beacon binary and its config. Usage: beaconctl.sh {start|stop|restart|status}
set -u

DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$DIR/beacon"
CFG="$DIR/beacon.toml"
LOG="$DIR/beacon.log"

# find_pids prints the PIDs of every running process whose executable name is
# exactly "beacon", by reading /proc/<pid>/comm. This avoids relying on pkill or
# killall, which are not guaranteed to exist (or support the same flags) on ADM.
find_pids() {
    for p in /proc/[0-9]*; do
        [ -r "$p/comm" ] || continue
        if grep -qx beacon "$p/comm" 2>/dev/null; then
            echo "${p##*/}"
        fi
    done
}

stop() {
    pids="$(find_pids)"
    if [ -n "$pids" ]; then
        # shellcheck disable=SC2086
        kill $pids 2>/dev/null
        # Give them a moment; escalate to KILL if still alive.
        sleep 1
        pids="$(find_pids)"
        if [ -n "$pids" ]; then
            # shellcheck disable=SC2086
            kill -9 $pids 2>/dev/null
        fi
    fi
}

start() {
    cd "$DIR" || exit 1
    nohup "$BIN" -config "$CFG" > "$LOG" 2>&1 &
    echo "beacon started (pid $!)"
}

# install swaps a freshly-uploaded beacon.new into place and seeds the config
# if absent. It does NOT restart (deploy calls 'restart' separately when asked).
install() {
    if [ -f "$DIR/beacon.new" ]; then
        chmod +x "$DIR/beacon.new"
        mv -f "$DIR/beacon.new" "$BIN"
        echo "installed new binary"
    fi
    if [ ! -f "$CFG" ] && [ -f "$DIR/beacon.example.toml" ]; then
        cp "$DIR/beacon.example.toml" "$CFG"
        echo "seeded default config"
    fi
}

case "${1:-}" in
    start)   start ;;
    stop)    stop; echo "beacon stopped" ;;
    restart) stop; sleep 1; start ;;
    install) install ;;
    status)
        pids="$(find_pids)"
        if [ -n "$pids" ]; then echo "beacon running (pid $pids)"; else echo "beacon not running"; fi
        ;;
    *) echo "usage: $0 {start|stop|restart|install|status}"; exit 1 ;;
esac
