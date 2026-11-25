#!/usr/bin/env bash
set -euo pipefail

readonly HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly NGINX=${NGINX:-nginx}
readonly LOCAL_HOST=127.0.0.1
readonly OTHER_HOST=127.0.0.2
readonly TARGET_PORT=${TARGET_PORT:-18080}
readonly NGINX_PORT=${NGINX_PORT:-18081}
readonly FLOOD_REQUESTS=20
readonly TOTAL_REQUESTS=$((FLOOD_REQUESTS + 1))
readonly READY_ATTEMPTS=100
readonly READY_STEP=0.1
readonly CONNECT_TIMEOUT=1
readonly CURL_MAX_TIME=5
readonly LOG_FIELDS=8
readonly COL_MSEC=1
readonly COL_ADDR=2
readonly COL_SERVER=3
readonly COL_STATUS=4
readonly COL_LIMIT=7
readonly COL_KEY=8
source "$HERE/../env/lib.sh"
testbed_add_go_to_path

TMP=$(mktemp -d)
PIDS=()
cleanup() {
  local pid
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "test_k1.sh: FALHOU: $*" >&2; exit 1; }

wait_port() {
  local pid=$1 port=$2 i
  for ((i = 0; i < READY_ATTEMPTS; i++)); do
    kill -0 "$pid" 2>/dev/null || return 1
    timeout "$CONNECT_TIMEOUT" bash -c "exec 3<>/dev/tcp/$LOCAL_HOST/$port" 2>/dev/null && return 0
    sleep "$READY_STEP"
  done
  return 1
}

start_target() {
  (cd "$HERE/../target" && go build -o "$TMP/target" .) || fail "go build do target"
  "$TMP/target" -addr "$LOCAL_HOST:$TARGET_PORT" &
  PIDS+=($!)
  wait_port $! "$TARGET_PORT" || fail "target não subiu em $LOCAL_HOST:$TARGET_PORT"
}

run_nginx() {
  local key=$1 dir=$TMP/$1 pid
  printf 'LISTEN_HOST=%s\nLISTEN6_HOST=\nPORT=%s\nR=2\nBURST=2\nQUEUE_MODE=nodelay\nUPSTREAM_ADDR=%s:%s\n' \
    "$LOCAL_HOST" "$NGINX_PORT" "$LOCAL_HOST" "$TARGET_PORT" >"$TMP/$key.env"
  bash "$HERE/render.sh" "$key" "$TMP/$key.env" "$dir" || fail "render.sh $key"
  "$NGINX" -c "$dir/nginx.conf" -p "$dir" -e "$dir/nginx.error.log" &
  pid=$!
  PIDS+=("$pid")
  wait_port "$pid" "$NGINX_PORT" || { cat "$dir/nginx.error.log" >&2; fail "nginx $key não subiu"; }
  curl -s -o /dev/null --max-time "$CURL_MAX_TIME" "http://$LOCAL_HOST:$NGINX_PORT/api?[1-$FLOOD_REQUESTS]" ||
    fail "curl contra nginx $key"
  curl -s -o /dev/null --max-time "$CURL_MAX_TIME" --interface "$OTHER_HOST" "http://$LOCAL_HOST:$NGINX_PORT/api" ||
    fail "curl de $OTHER_HOST contra nginx $key"
  kill -QUIT "$pid"
  wait "$pid" || fail "nginx $key saiu com erro ao encerrar"
}

check_log_shape() {
  local log=$1 key=$2 total bad
  total=$(wc -l <"$log")
  [[ $total -eq $TOTAL_REQUESTS ]] || fail "$log: esperava $TOTAL_REQUESTS linhas, achou $total"
  bad=$(awk -F'\t' -v n=$LOG_FIELDS -v m=$COL_MSEC -v a=$COL_ADDR -v s=$COL_SERVER -v x=$COL_KEY -v key="$key" \
    'NF != n || $m !~ /^[0-9]+\.[0-9][0-9][0-9]$/ || $s != key || $x == "" || $x != $a' "$log")
  [[ -z $bad ]] || fail "$log: linha fora do C-LOG (8 campos, \$msec, server_name=$key, chave=\$remote_addr): $(head -n1 <<<"$bad")"
}

check_other_origin() {
  local log=$1 key=$2 ok
  ok=$(tail -n1 "$log" | awk -F'\t' -v s=$COL_STATUS -v a=$COL_ADDR -v x=$COL_KEY -v alt=$OTHER_HOST \
    '$s == 200 && $a == alt && $x == alt' | wc -l)
  [[ $ok -eq 1 ]] || fail "$key: origem $OTHER_HOST não teve 200 com remote_addr e chave próprios: $(tail -n1 "$log")"
}

check_k1() {
  local key=$1 log=$TMP/$1/nginx.access.log ok rejected
  check_log_shape "$log" "$key"
  ok=$(awk -F'\t' -v c=$COL_STATUS '$c == 200' "$log" | wc -l)
  rejected=$(awk -F'\t' -v c=$COL_STATUS -v l=$COL_LIMIT '$c == 429 && $l == "REJECTED"' "$log" | wc -l)
  [[ $ok -ge 1 ]] || fail "$key: nenhuma resposta 200"
  [[ $rejected -ge 1 ]] || fail "$key: nenhuma 429 com limit_req_status=REJECTED"
  check_other_origin "$log" "$key"
}

check_none() {
  local key=$1 log=$TMP/$1/nginx.access.log limited
  check_log_shape "$log" "$key"
  limited=$(awk -F'\t' -v c=$COL_STATUS '$c == 429' "$log" | wc -l)
  [[ $limited -eq 0 ]] || fail "$key: $limited respostas 429 sem limite configurado"
  check_other_origin "$log" "$key"
}

main() {
  start_target
  run_nginx k1
  check_k1 k1
  run_nginx none
  check_none none
  echo "test_k1.sh: OK"
}

main
