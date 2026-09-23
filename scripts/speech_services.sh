#!/usr/bin/env bash
# Start / stop the local speech services and the chat gateway (the Go router runs separately: cd backend && go run ./cmd/router):
#   STT  stt/server.py  (.venv,     Python 3.14)  -> http://127.0.0.1:9100  (OpenAI-style /v1/audio/transcriptions)
#   TTS  tts/server.py  (.venv-tts, Python 3.13)  -> http://127.0.0.1:9101  (OpenAI-style /v1/audio/speech)
#   CHAT chat/server.py (.venv)                    -> http://127.0.0.1:9102  (voice chat frontend + gateway -> Go router)
#
#   scripts/speech_services.sh start [--ui]   # --ui also serves the STT test console at :9100
#   ROUTER_URL=http://127.0.0.1:8080/v1/turns scripts/speech_services.sh start   # default; ROUTER_TOKEN=... if the router has API_TOKEN
#   scripts/speech_services.sh stop
#   scripts/speech_services.sh status
#   scripts/speech_services.sh logs
set -euo pipefail
cd "$(dirname "$0")/.."
RUN=.run; mkdir -p "$RUN"
if [[ -f .env ]]; then set -a; source .env; set +a; fi
STT_PORT="${STT_PORT:-9100}"; TTS_PORT="${TTS_PORT:-9101}"; CHAT_PORT="${CHAT_PORT:-9102}"

wait_ok() {  # url, seconds
  for ((i = 0; i < $2; i++)); do
    if curl -sf "$1" 2>/dev/null | grep -q '"status":"ok"'; then return 0; fi
    sleep 1
  done
  return 1
}
port_of() { case $1 in tts) echo "$TTS_PORT";; chat) echo "$CHAT_PORT";; *) echo "$STT_PORT";; esac; }
pid_on_port() { ss -tlnp 2>/dev/null | grep ":$1 " | grep -o 'pid=[0-9]*' | cut -d= -f2 | head -1; }
# The PID that matters is the one listening on the port ($! may be a setsid wrapper that exits at once).
pid_of() { local p; p=$(pid_on_port "$(port_of "$1")"); [[ -n $p ]] && echo "$p" && return; [[ -f "$RUN/$1.pid" ]] && kill -0 "$(cat "$RUN/$1.pid")" 2>/dev/null && cat "$RUN/$1.pid"; }
running() { [[ -n $(pid_of "$1") ]]; }

start_one() {  # name, python, script, env...
  local name=$1 py=$2 script=$3; shift 3
  if running "$name"; then echo "$name: already running (pid $(pid_of "$name"))"; return; fi
  [[ -x "$py" ]] || { echo "$name: $py missing — see $(dirname "$script")/README.md" >&2; return 1; }
  env "$@" setsid nohup "$py" "$script" > "$RUN/$name.log" 2>&1 < /dev/null &
  echo "$name: starting (log $RUN/$name.log)"
}
record_pid() { local p; p=$(pid_on_port "$(port_of "$1")"); [[ -n $p ]] && echo "$p" > "$RUN/$1.pid"; echo "${p:-?}"; }

case "${1:-}" in
  start)
    ui=0; [[ "${2:-}" == "--ui" ]] && ui=1
    start_one stt .venv/bin/python stt/server.py STT_UI=$ui STT_PORT="$STT_PORT"
    start_one tts .venv-tts/bin/python tts/server.py TTS_PORT="$TTS_PORT"
    start_one chat .venv/bin/python chat/server.py CHAT_PORT="$CHAT_PORT" STT_URL="http://127.0.0.1:$STT_PORT" TTS_URL="http://127.0.0.1:$TTS_PORT" \
      ROUTER_URL="${ROUTER_URL:-http://127.0.0.1:8080/v1/turns}" ROUTER_TOKEN="${ROUTER_TOKEN:-${API_TOKEN:-}}" ROUTER_MODE="${ROUTER_MODE:-auto}"
    wait_ok "http://127.0.0.1:$STT_PORT/health" 90 && echo "stt: ready on :$STT_PORT (pid $(record_pid stt))" || { echo "stt: not healthy, see $RUN/stt.log" >&2; exit 1; }
    wait_ok "http://127.0.0.1:$TTS_PORT/health" 180 && echo "tts: ready on :$TTS_PORT (pid $(record_pid tts))" || { echo "tts: not healthy, see $RUN/tts.log" >&2; exit 1; }
    wait_ok "http://127.0.0.1:$CHAT_PORT/health" 30 && echo "chat: ready on http://localhost:$CHAT_PORT (pid $(record_pid chat))" || { echo "chat: not healthy, see $RUN/chat.log" >&2; exit 1; }
    echo "Go router: cd backend && OPENAI_API_KEY=... go run ./cmd/router   (the chat gateway looks for it at ${ROUTER_URL:-http://127.0.0.1:8080/v1/turns})"
    ;;
  stop)
    for name in chat stt tts; do
      p=$(pid_of "$name")
      if [[ -n $p ]]; then kill "$p" && echo "$name: stopped (pid $p)"; else echo "$name: not running"; fi
      rm -f "$RUN/$name.pid"
    done
    for ((i = 0; i < 20; i++)); do ss -tln | grep -qE ":($STT_PORT|$TTS_PORT|$CHAT_PORT) " || break; sleep 0.5; done
    ;;
  status)
    for name in stt tts chat; do
      port=$(port_of "$name")
      if running "$name"; then echo "$name: running (pid $(pid_of "$name")) $(curl -sf "http://127.0.0.1:$port/health" 2>/dev/null | cut -c1-120 || echo '(no health yet)')"; else echo "$name: stopped"; fi
    done
    ;;
  logs) tail -n 30 "$RUN"/stt.log "$RUN"/tts.log "$RUN"/chat.log 2>/dev/null ;;
  *) sed -n '2,10p' "$0"; exit 1 ;;
esac
