#!/bin/sh
# ADM start/stop hook for Beacon. Runs the server from the App Central install
# directory, seeding a default config on first launch.
set -u

NAME=Beacon
PKG_PATH=/usr/local/AppCentral/Beacon
BIN="$PKG_PATH/beacon"
CFG="$PKG_PATH/beacon.toml"
LOG="$PKG_PATH/beacon.log"
PIDFILE="$PKG_PATH/beacon.pid"

running_pids() {
    for p in /proc/[0-9]*; do
        [ -r "$p/comm" ] || continue
        if grep -qx beacon "$p/comm" 2>/dev/null; then
            echo "${p##*/}"
        fi
    done
}

start() {
    [ -f "$CFG" ] || cp "$PKG_PATH/beacon.example.toml" "$CFG"
    chmod +x "$BIN" 2>/dev/null
    # Ensure ffprobe/ffmpeg (in ADM's builtin/local bin dirs) are discoverable,
    # since App Central may launch us with a minimal PATH.
    export PATH="/usr/local/bin:/usr/builtin/bin:/usr/bin:/bin:$PATH"
    cd "$PKG_PATH" || exit 1
    nohup "$BIN" -config "$CFG" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
}

stop() {
    if [ -f "$PIDFILE" ]; then
        kill "$(cat "$PIDFILE")" 2>/dev/null
        rm -f "$PIDFILE"
    fi
    pids="$(running_pids)"
    if [ -n "$pids" ]; then
        # shellcheck disable=SC2086
        kill $pids 2>/dev/null
    fi
}

case "$1" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; sleep 1; start ;;
    *) echo "usage: $0 {start|stop|restart}"; exit 1 ;;
esac
exit 0
