#!/usr/bin/env bash
# Comum aos gates de rede (G1 e G5): sobe alvo + Nginx no namespace raiz, roda o atacante em gen,
# amostra a carga enquanto ele roda e imprime o veredito. Carregar com `source` após `set -uo pipefail`.

[[ -d /usr/local/go/bin ]] && PATH=$PATH:/usr/local/go/bin

readonly NETNS=gen
readonly HOST_V4=10.77.255.1
readonly HOST_V6=fd77:255::1
readonly SAMPLE_STEP=1
readonly READY_ATTEMPTS=100
readonly READY_STEP=0.1
readonly PORT_WAIT=300
readonly PORT_WAIT_STEP=2
readonly NO_LIMIT_RATE=100000
readonly CLOCK_TICKS=$(getconf CLK_TCK)
readonly NGINX=${NGINX:-nginx}
readonly PYTHON=${PYTHON:-${TESTBED_VENV:-$HOME/.venvs/testbed}/bin/python}

GATE_NAME=gate
GATE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE_TMP=$(mktemp -d)
ACCESS_LOG=$GATE_TMP/run/nginx.access.log
ATTACK_ERR=
ATTACK_PID=
GEN_CT=
GEN_TW=
LOAD_PATTERNS=()
SUDO=()
NGINX_PID=
SAMPLE_JOB=
TARGET_PID=

gate_fail() { echo "FAIL $GATE_NAME: $*"; exit 1; }
gate_pass() { echo "PASS $GATE_NAME: $*"; exit 0; }
gate_join() { local IFS=';' joined=${*//; /;}; echo "${joined//;/; }"; }

gate_int() {
  [[ $2 =~ ^[0-9]+$ && 10#$2 -ge $3 ]] || gate_fail "$1=$2 inválido: esperado inteiro >= $3"
}

gate_fraction() {
  [[ $2 =~ ^(1(\.0+)?|0?\.[0-9]+)$ ]] || gate_fail "$1=$2 inválido: esperado número entre 0 e 1"
}

gate_sudo() {
  [[ $EUID -ne 0 ]] || gate_fail "rode como usuário comum; o gate chama sudo -n só nos passos de root"
  SUDO=(sudo -n)
  "${SUDO[@]}" true 2>/dev/null || gate_fail "sudo sem senha indisponível: o gate precisa de root para o netns"
}

gate_need() {
  local cmd
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || gate_fail "comando ausente: $cmd"
  done
}

# $1 = caminho do calibration.env; imprime o ATTACK_RATE e falha quando o arquivo não o define.
gate_calibration_rate() {
  local value
  [[ -r $1 ]] || return 1
  value=$(sed -n 's/^ATTACK_RATE=//p' "$1" | tail -n 1 | tr -d '[:space:]')
  [[ $value =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$value"
}

gate_build() {
  mkdir -p "$GATE_TMP/bin"
  (cd "$GATE_ROOT/target" && go build -o "$GATE_TMP/bin/target" .) || gate_fail "build de target falhou"
  (cd "$GATE_ROOT/gen/attacker" && go build -o "$GATE_TMP/bin/attacker" .) || gate_fail "build de gen/attacker falhou"
}

free_port() {
  "$PYTHON" -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

port_in_use() { [[ -n $(ss -H -ltn "sport = :$1") ]]; }

# As suítes partilham portas fixas: outra pode estar no ar.
wait_free_port() {
  local waited=0 port=$1
  while port_in_use "$port"; do
    (( waited < PORT_WAIT )) || gate_fail "porta $port ocupada há mais de ${PORT_WAIT}s"
    sleep "$PORT_WAIT_STEP"
    waited=$((waited + PORT_WAIT_STEP))
  done
}

wait_ready() {
  local url=$1 i
  for ((i = 0; i < READY_ATTEMPTS; i++)); do
    kill -0 "$NGINX_PID" 2>/dev/null || gate_fail "Nginx saiu antes de responder em $url"
    curl -fsS -m 2 -o /dev/null "$url" 2>/dev/null && return 0
    sleep "$READY_STEP"
  done
  return 1
}

gate_kill_netns_pids() {
  local pid
  while IFS= read -r pid; do
    [[ -n $pid ]] || continue
    "${SUDO[@]}" kill -KILL "$pid" 2>/dev/null
  done < <("${SUDO[@]}" ip netns pids "$NETNS" 2>/dev/null)
}

# A rede só é considerada limpa se o teardown fechar; falhar nele derruba o gate mesmo depois de um PASS.
gate_shutdown() {
  local rc=$? teardown_rc=0
  trap - EXIT
  [[ -z $SAMPLE_JOB ]] || { kill "$SAMPLE_JOB" 2>/dev/null; wait "$SAMPLE_JOB" 2>/dev/null; }
  [[ -z $ATTACK_PID ]] || { kill -TERM "$ATTACK_PID" 2>/dev/null; wait "$ATTACK_PID" 2>/dev/null; }
  [[ -z $NGINX_PID ]] || { kill -TERM "$NGINX_PID" 2>/dev/null; wait "$NGINX_PID" 2>/dev/null; }
  [[ -z $TARGET_PID ]] || { kill "$TARGET_PID" 2>/dev/null; wait "$TARGET_PID" 2>/dev/null; }
  if [[ ${#SUDO[@]} -ne 0 ]]; then
    gate_kill_netns_pids
    "${SUDO[@]}" bash "$GATE_ROOT/net/netns-teardown.sh" || teardown_rc=$?
  fi
  rm -rf "$GATE_TMP"
  if (( teardown_rc != 0 )); then
    echo "FAIL $GATE_NAME: netns-teardown.sh saiu com $teardown_rc; a rede $NETNS pode ter sobrado"
    exit 1
  fi
  exit "$rc"
}

# $1 = cenário da rede, $2 = chave do Nginx, $3 = porta de escuta (>= 18000)
gate_stack() {
  local upstream port=$3
  wait_free_port "$port"
  trap gate_shutdown EXIT
  "${SUDO[@]}" bash "$GATE_ROOT/net/netns-setup.sh" "$1" || gate_fail "rede sintética ($1) não subiu"
  upstream=$(free_port) || gate_fail "sem porta livre para o alvo"
  cat > "$GATE_TMP/params.env" <<EOF
LISTEN_HOST=$HOST_V4
LISTEN6_HOST=$HOST_V6
PORT=$port
R=$NO_LIMIT_RATE
BURST=$NO_LIMIT_RATE
QUEUE_MODE=nodelay
UPSTREAM_ADDR=127.0.0.1:$upstream
EOF
  bash "$GATE_ROOT/nginx/render.sh" "$2" "$GATE_TMP/params.env" "$GATE_TMP/run" ||
    gate_fail "render do Nginx com a chave $2 falhou"
  "$GATE_TMP/bin/target" -addr "127.0.0.1:$upstream" >"$GATE_TMP/target.log" 2>&1 &
  TARGET_PID=$!
  "$NGINX" -c "$GATE_TMP/run/nginx.conf" -p "$GATE_TMP/run" -e "$GATE_TMP/run/nginx.error.log" &
  NGINX_PID=$!
  LOAD_PATTERNS=("$GATE_TMP/bin/attacker " "$GATE_TMP/bin/target " "^nginx: master process .*$GATE_TMP/run/nginx.conf")
  wait_ready "http://$HOST_V4:$port/api" || gate_fail "Nginx não atendeu em http://$HOST_V4:$port"
  wait_ready "http://[$HOST_V6]:$port/api" || gate_fail "Nginx não atendeu em http://[$HOST_V6]:$port"
  : > "$ACCESS_LOG"
}

# $1 = teto de espera em s, $2.. = flags do atacante; devolve ATTACK_RC (124 = estourou o teto) e
# ATTACK_ERR, a primeira linha do stderr dele.
gate_attack() {
  timeout --signal=INT "$1" "${SUDO[@]}" ip netns exec "$NETNS" "$GATE_TMP/bin/attacker" "${@:2}" \
    2>"$GATE_TMP/attacker.err" &
  ATTACK_PID=$!
  wait "$ATTACK_PID"
  ATTACK_RC=$?
  ATTACK_PID=
  ATTACK_ERR=$(head -n 1 "$GATE_TMP/attacker.err")
}

# $1 = jsonl do atacante; imprime `ok=N fail_partial=N ...`
gate_outcomes() {
  [[ -s $1 ]] || { echo "sem registros"; return; }
  jq -r .outcome "$1" | sort | uniq -c | awk '{ printf "%s%s=%d", sep, $2, $1; sep = " " }'
}

# $1 = gen ou raiz; imprime `drop=N insert_failed=N early_drop=N`
gate_conntrack() {
  local counters
  if [[ $1 == gen ]]; then
    counters=$("${SUDO[@]}" ip netns exec "$NETNS" conntrack -S)
  else
    counters=$("${SUDO[@]}" conntrack -S)
  fi
  awk '{
    for (i = 2; i <= NF; i++) { split($i, kv, "="); s[kv[1]] += kv[2] }
  } END { printf "drop=%d insert_failed=%d early_drop=%d\n", s["drop"], s["insert_failed"], s["early_drop"] }' <<<"$counters"
}

gate_conntrack_delta() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    n = split(a, x, " "); split(b, y, " "); bad = 0; out = ""
    for (i = 1; i <= n; i++) {
      split(x[i], p, "="); split(y[i], q, "="); d = q[2] - p[2]
      out = out (out ? " " : "") sprintf("%s +%d", p[1], d); if (d > 0) bad = 1
    }
    print out; exit bad
  }'
}

cpu_times() {
  awk '/^cpu /{ t = 0; for (i = 2; i <= NF; i++) t += $i; print t, $5 + $6; exit }' /proc/stat
}

# Soma utime+stime (em ticks) dos processos que casam com LOAD_PATTERNS e de seus descendentes.
load_ticks() {
  local pattern pid i
  local pids=() stats=()
  for pattern in "${LOAD_PATTERNS[@]}"; do
    while IFS= read -r pid; do pids+=("$pid"); done < <(pgrep -f -- "$pattern" || true)
  done
  for ((i = 0; i < ${#pids[@]}; i++)); do
    while IFS= read -r pid; do pids+=("$pid"); done < <(pgrep -P "${pids[i]}" || true)
  done
  for pid in "${pids[@]}"; do stats+=(/proc/"$pid"/task/*/stat); done
  awk '{ n = split($0, a, ") "); split(a[2], f, " "); s += f[12] + f[13] } END { print s + 0 }' \
    "${stats[@]}" 2>/dev/null
}

gen_counters() {
  local gen=$("${SUDO[@]}" ip netns exec "$NETNS" cat /proc/net/sockstat \
    /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null)
  GEN_TW=$(sed -n 's/^TCP:.* tw \([0-9]*\).*/\1/p' <<<"$gen")
  GEN_CT=${gen##*$'\n'}
}

# Uma amostra por segundo em samples.tsv: instante, TIME_WAIT (raiz, gen), entradas de conntrack
# (raiz, gen), CPU do sistema (%) e CPU da carga (% de um núcleo).
gate_sampler() {
  local total idle ticks prev_total prev_idle prev_ticks
  read -r prev_total prev_idle < <(cpu_times)
  prev_ticks=$(load_ticks)
  while :; do
    sleep "$SAMPLE_STEP"
    read -r total idle < <(cpu_times)
    ticks=$(load_ticks)
    gen_counters
    awk -v t="$(date +%s.%N)" -v tw_root="$(awk '/^TCP:/{print $5}' /proc/net/sockstat)" \
      -v tw_gen="${GEN_TW:-0}" -v ct_root="$(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null)" \
      -v ct_gen="${GEN_CT:-0}" -v dt="$((total - prev_total))" -v di="$((idle - prev_idle))" \
      -v dk="$((ticks - prev_ticks))" -v step="$SAMPLE_STEP" -v ticks="$CLOCK_TICKS" \
      'BEGIN {
        if (dt == 0) dt = 1
        printf "%.2f %.0f %.0f %.0f %.0f %.1f %.1f\n", t, tw_root + 0, tw_gen, ct_root + 0, ct_gen, \
          100 * (dt - di) / dt, 100 * dk / (step * ticks)
      }' >>"$GATE_TMP/samples.tsv"
    prev_total=$total prev_idle=$idle prev_ticks=$ticks
  done
}

gate_sample_start() {
  rm -f "$GATE_TMP/samples.tsv"
  gate_sampler &
  SAMPLE_JOB=$!
}

gate_sample_stop() {
  kill "$SAMPLE_JOB" 2>/dev/null
  wait "$SAMPLE_JOB" 2>/dev/null
  SAMPLE_JOB=
}

gate_sample_summary() {
  [[ -s $GATE_TMP/samples.tsv ]] || { echo "sem amostras de carga"; return; }
  awk '{
    if ($2 > tr) tr = $2; if ($3 > tg) tg = $3; if ($4 > cr) cr = $4; if ($5 > cg) cg = $5
    sys += $6; load += $7; n++
  } END {
    printf "TIME_WAIT pico gen=%d raiz=%d; conntrack pico gen=%d raiz=%d; CPU media do sistema %.0f%%, da carga %.0f%% de um nucleo (%d amostras)", \
      tg, tr, cg, cr, sys / n, load / n, n
  }' "$GATE_TMP/samples.tsv"
}
