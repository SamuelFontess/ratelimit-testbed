#!/usr/bin/env bash
set -uo pipefail

[[ -d /usr/local/go/bin ]] && PATH=$PATH:/usr/local/go/bin

HERE=$(cd "$(dirname "$0")" && pwd)
SCHEDULE_PY=$HERE/../gen/legit/schedule.py
PY=${PYTHON:-${TESTBED_VENV:-$HOME/.venvs/testbed}/bin/python}
SCENARIOS=${G4_SCENARIOS:-"s1 s2a s3"}
SEED_A=${G4_SEED_A:-1}
SEED_B=${G4_SEED_B:-2}
LAM=${G4_LAMBDA:-50}
CLIENTS=${G4_CLIENTS:-50}
DUR=${G4_DUR:-150}
LEGIT_LAM=${G4_LEGIT_LAMBDA:-30}
LEGIT_CLIENTS=${G4_LEGIT_CLIENTS:-5}
LEGIT_DUR=${G4_LEGIT_DUR:-2}
LOOPBACK=127.0.0.1
READY_ATTEMPTS=100

TMP=$(mktemp -d)
TARGET_PID=
cleanup() {
  [[ -n $TARGET_PID ]] && kill "$TARGET_PID" 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL $*"; exit 1; }

gen() {
  "$PY" "$SCHEDULE_PY" --seed "$1" --scenario "$2" --lam "$3" --clients "$4" --dur "$5" --out "$6"
}

normalize() {
  awk '{ printf "%.17g %d\n", $1, $2 }'
}

agenda_pairs() {
  grep -o '{"cid":[0-9]*,"t":[^}]*}' "$1" | sed -E 's/^\{"cid":([0-9]+),"t":([^}]*)\}$/\2 \1/' | normalize
}

legit_pairs() {
  sed -E 's/^\{"t_plan":([^,]*),.*"cid":([0-9]+),.*$/\1 \2/' "$1" | normalize
}

wait_for_target() {
  local i
  for ((i = 0; i < READY_ATTEMPTS; i++)); do
    (exec 3<>"/dev/tcp/$LOOPBACK/$PORT") 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

[[ -x $PY ]] || fail "python do venv ausente em $PY"

for scenario in $SCENARIOS; do
  gen "$SEED_A" "$scenario" "$LAM" "$CLIENTS" "$DUR" "$TMP/c1.json" || fail "$scenario: schedule.py falhou na corrida C1"
  gen "$SEED_A" "$scenario" "$LAM" "$CLIENTS" "$DUR" "$TMP/c4.json" || fail "$scenario: schedule.py falhou na corrida C4"
  gen "$SEED_B" "$scenario" "$LAM" "$CLIENTS" "$DUR" "$TMP/outra.json" || fail "$scenario: schedule.py falhou com outra seed"
  cmp -s "$TMP/c1.json" "$TMP/c4.json" || fail "$scenario: mesma seed gerou agendas diferentes entre C1 e C4"
  cmp -s "$TMP/c1.json" "$TMP/outra.json" && fail "$scenario: seeds $SEED_A e $SEED_B geraram a mesma agenda"
  n=$(grep -o '"t":' "$TMP/c1.json" | wc -l)
  (( n > 0 )) || fail "$scenario: agenda sem eventos"
done

(cd "$HERE/../gen/legit" && go build -o "$TMP/legit" .) || fail "build de gen/legit falhou"
(cd "$HERE/../target" && go build -o "$TMP/target" .) || fail "build de target falhou"
PORT=$("$PY" -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])') || fail "sem porta livre"
"$TMP/target" --addr "$LOOPBACK:$PORT" &
TARGET_PID=$!
wait_for_target || fail "alvo local nao respondeu em $LOOPBACK:$PORT"

for scenario in $SCENARIOS; do
  gen "$SEED_A" "$scenario" "$LEGIT_LAM" "$LEGIT_CLIENTS" "$LEGIT_DUR" "$TMP/agenda.json" || fail "$scenario: schedule.py falhou na agenda curta"
  sed -E 's/"src":"[^"]*"/"src":"'"$LOOPBACK"'"/g' "$TMP/agenda.json" > "$TMP/local.json"
  for run in c1 c4; do
    "$TMP/legit" --target "http://$LOOPBACK:$PORT" --schedule "$TMP/local.json" --out "$TMP/legit_$run.jsonl" || fail "$scenario: legit falhou na corrida $run"
  done
  agenda_pairs "$TMP/local.json" > "$TMP/agenda.pairs"
  legit_pairs "$TMP/legit_c1.jsonl" > "$TMP/c1.pairs"
  legit_pairs "$TMP/legit_c4.jsonl" > "$TMP/c4.pairs"
  [[ -s $TMP/agenda.pairs ]] || fail "$scenario: agenda curta sem eventos"
  cmp -s "$TMP/c1.pairs" "$TMP/c4.pairs" || fail "$scenario: (cid, t_plan) diferem entre os legit.jsonl de C1 e C4"
  cmp -s "$TMP/agenda.pairs" "$TMP/c1.pairs" || fail "$scenario: legit.jsonl nao reproduz (cid, t_plan) da agenda"
done

echo PASS
