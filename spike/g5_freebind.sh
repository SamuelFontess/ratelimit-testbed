#!/usr/bin/env bash
# Gate G5: o atacante usa origens não locais em gen e o Nginx registra no access.log pelo menos
# G5_MIN_DISTINCT IPv4 e outros tantos IPv6 distintos. Roda uma fase IPv4 e uma fase IPv6 e repete o
# par até atingir a contagem ou esgotar G5_ROUNDS. Parâmetros: G5_RATE, G5_DURATION,
# G5_MIN_DISTINCT, G5_ROUNDS, G5_SCENARIO (cenário da rede; em s1 o SNAT reduz o IPv4 a um) e G5_PORT.

set -uo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$HERE/gate_lib.sh"

GATE_NAME=G5
RATE=${G5_RATE:-60}
DURATION=${G5_DURATION:-30}
MIN_DISTINCT=${G5_MIN_DISTINCT:-1000}
ROUNDS=${G5_ROUNDS:-3}
SCENARIO=${G5_SCENARIO-s2a}
PORT=${G5_PORT:-18090}
readonly ATTACK_GRACE=90
readonly MIN_PORT=18000
readonly V4_POOL_SCENARIO=s3
readonly V6_POOL_SCENARIO=s2a
FASES=()

# A flag --scenario do atacante só escolhe o pool de origens; o cenário da rede decide se há SNAT.
v4_pool() {
  [[ $SCENARIO == s1 ]] && echo s1 || echo "$V4_POOL_SCENARIO"
}

# $1 = rótulo, $2 = cenário do atacante, $3 = alvo; anexa código e outcomes a FASES
run_phase() {
  local out=$GATE_TMP/attacker_$1.jsonl
  gate_attack $((DURATION + ATTACK_GRACE)) --scenario "$2" --target "$3" --seed 1 \
    --duration "$DURATION" --rate "$RATE" --heavy-ratio 0 --post-ratio 0 --out "$out"
  FASES+=("$1 (código $ATTACK_RC${ATTACK_ERR:+, $ATTACK_ERR}): $(gate_outcomes "$out")")
  return "$ATTACK_RC"
}

# imprime `IPv4 IPv6`
distinct_counts() {
  cut -f2 "$ACCESS_LOG" | awk '{ if (index($0, ":")) v6[$0] = 1; else v4[$0] = 1 }
    END { for (a in v4) n4++; for (a in v6) n6++; print n4 + 0, n6 + 0 }'
}

main() {
  local round distinct4 distinct6
  local problems=() numbers=()

  gate_int G5_RATE "$RATE" 1
  gate_int G5_DURATION "$DURATION" 1
  gate_int G5_MIN_DISTINCT "$MIN_DISTINCT" 1
  gate_int G5_ROUNDS "$ROUNDS" 1
  gate_int G5_PORT "$PORT" $MIN_PORT
  gate_sudo
  gate_need go jq curl "$NGINX"
  gate_build
  gate_stack "$SCENARIO" k1 "$PORT"

  gate_sample_start
  for ((round = 1; round <= ROUNDS; round++)); do
    run_phase IPv4 "$(v4_pool)" "http://$HOST_V4:$PORT" || problems+=("fase IPv4 falhou")
    run_phase IPv6 "$V6_POOL_SCENARIO" "http://[$HOST_V6]:$PORT" || problems+=("fase IPv6 falhou")
    read -r distinct4 distinct6 < <(distinct_counts)
    (( distinct4 >= MIN_DISTINCT && distinct6 >= MIN_DISTINCT )) && break
  done
  gate_sample_stop
  (( distinct4 >= MIN_DISTINCT )) || problems+=("só $distinct4 IPv4 distintos, mínimo $MIN_DISTINCT")
  (( distinct6 >= MIN_DISTINCT )) || problems+=("só $distinct6 IPv6 distintos, mínimo $MIN_DISTINCT")

  numbers+=("$distinct4 IPv4 e $distinct6 IPv6 distintos no access.log (mínimo $MIN_DISTINCT de cada) em $round rodada(s) de ${RATE}/s por ${DURATION}s por fase")
  numbers+=("fases: $(gate_join "${FASES[@]}")")
  numbers+=("$(gate_sample_summary)")
  if (( ${#problems[@]} == 0 )); then gate_pass "$(gate_join "${numbers[@]}")"; fi
  gate_fail "$(gate_join "${problems[@]}") | $(gate_join "${numbers[@]}")"
}

main
