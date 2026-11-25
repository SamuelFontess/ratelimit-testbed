#!/usr/bin/env bash
# Gate G1: em S1, G1_RATE conexões novas/s por G1_DURATION s chegam ao Nginx todas vindas do IPv4 do
# SNAT, sem EADDRNOTAVAIL e sem descarte de conntrack. Sem G1_RATE, vale o ATTACK_RATE de
# calibration.env e, sem ele, o padrão de 2000/s. Parâmetros: G1_RATE, CALIBRATION_ENV, G1_DURATION,
# G1_MIN_FRACTION (fração da taxa exigida) e G1_PORT.

set -uo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$HERE/gate_lib.sh"

GATE_NAME=G1
readonly DEFAULT_RATE=2000
readonly CALIBRATION_ENV=${CALIBRATION_ENV:-$GATE_ROOT/calibration.env}
DURATION=${G1_DURATION:-60}
MIN_FRACTION=${G1_MIN_FRACTION:-0.95}
PORT=${G1_PORT:-18091}
readonly ATTACK_GRACE=90
readonly GEN_V4=10.77.255.2
readonly MIN_PORT=18000
if [[ -n ${G1_RATE:-} ]]; then
  RATE=$G1_RATE
else
  RATE=$(gate_calibration_rate "$CALIBRATION_ENV") || RATE=$DEFAULT_RATE
fi

# A janela começa na primeira conexão que o Nginx registrou, não no lançamento do atacante: a
# conferência das origens dele antes do ataque leva tempo e não faz parte da taxa medida.
# imprime `na janela fora do IPv4 do SNAT conexões/s`
log_stats() {
  awk -F'\t' -v duration="$DURATION" -v gen_v4="$GEN_V4" '
    NR == 1 { start = $1 }
    { if ($2 != gen_v4) foreign++; if ($1 <= start + duration) in_window++ }
    END { printf "%d %d %.1f\n", in_window, foreign + 0, in_window / duration }' "$ACCESS_LOG"
}

main() {
  local ct_before ct_after ct_root_before ct_root_after ct_delta ct_root_delta
  local in_window foreign achieved min_rate falhas_bind
  local problems=() numbers=()

  gate_int G1_RATE "$RATE" 1
  gate_int G1_DURATION "$DURATION" 1
  gate_fraction G1_MIN_FRACTION "$MIN_FRACTION"
  gate_int G1_PORT "$PORT" $MIN_PORT
  gate_sudo
  gate_need go jq curl conntrack "$NGINX"
  gate_build
  gate_stack s1 none "$PORT"

  ct_before=$(gate_conntrack gen) || gate_fail "conntrack -S do namespace $NETNS"
  ct_root_before=$(gate_conntrack raiz)
  gate_sample_start
  gate_attack $((DURATION + ATTACK_GRACE)) --scenario s1 --target "http://$HOST_V4:$PORT" \
    --duration "$DURATION" --rate "$RATE" --heavy-ratio 0 --post-ratio 0 --out "$GATE_TMP/attacker.jsonl"
  gate_sample_stop
  ct_after=$(gate_conntrack gen) || gate_fail "conntrack -S do namespace $NETNS após o ataque"
  ct_root_after=$(gate_conntrack raiz)
  kill -TERM "$NGINX_PID" 2>/dev/null
  wait "$NGINX_PID" 2>/dev/null
  NGINX_PID=

  ct_delta=$(gate_conntrack_delta "$ct_before" "$ct_after") ||
    problems+=("descarte de conntrack em $NETNS: $ct_delta")
  ct_root_delta=$(gate_conntrack_delta "$ct_root_before" "$ct_root_after" || true)
  read -r in_window foreign achieved < <(log_stats)
  min_rate=$(awk -v r="$RATE" -v f="$MIN_FRACTION" 'BEGIN { printf "%.1f", r * f }')
  falhas_bind=$(grep -c EADDRNOTAVAIL "$GATE_TMP/attacker.err" || true)

  (( ATTACK_RC == 0 )) || problems+=("atacante terminou com código $ATTACK_RC: $ATTACK_ERR")
  (( falhas_bind == 0 )) || problems+=("$falhas_bind EADDRNOTAVAIL no atacante")
  (( in_window > 0 )) || problems+=("o Nginx não registrou conexão alguma")
  (( foreign == 0 )) || problems+=("$foreign linhas do access.log com origem fora de $GEN_V4")
  awk -v a="$achieved" -v m="$min_rate" 'BEGIN { exit !(a >= m) }' ||
    problems+=("$achieved conexões/s das $RATE/s exigidas, mínimo $min_rate/s")

  numbers+=("$achieved conexões/s de $RATE/s exigidas em ${DURATION}s ($in_window no access.log)")
  numbers+=("origem única: $GEN_V4, $foreign linhas fora dela")
  numbers+=("EADDRNOTAVAIL: $falhas_bind")
  numbers+=("conntrack em $NETNS: $ct_delta; na raiz: $ct_root_delta")
  numbers+=("outcomes do atacante: $(gate_outcomes "$GATE_TMP/attacker.jsonl")")
  numbers+=("$(gate_sample_summary)")
  if (( ${#problems[@]} == 0 )); then gate_pass "$(gate_join "${numbers[@]}")"; fi
  gate_fail "$(gate_join "${problems[@]}") | $(gate_join "${numbers[@]}")"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main
fi
