#!/usr/bin/env bash
set -euo pipefail

readonly HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly ALWAYS_REQUIRED=(PORT QUEUE_MODE)
readonly SUBST_VARS=(
  WORKERS WORKER_CONNECTIONS RUN_DIR
  UPSTREAM_NAME UPSTREAM_ADDR UPSTREAM_KEEPALIVE
  LISTEN_HOST LISTEN6_LINE LAN_LISTEN_LINE PORT
  ZONE_SIZE R BURST QUEUE_FLAG
)

fail() { echo "render.sh: $*" >&2; exit 1; }

apply_defaults() {
  : "${LISTEN_HOST:=10.77.255.1}"
  LISTEN6_HOST=${LISTEN6_HOST-fd77:255::1}
  : "${UPSTREAM_ADDR:=127.0.0.1:8080}"
  : "${UPSTREAM_NAME:=app}"
  : "${UPSTREAM_KEEPALIVE:=64}"
  : "${ZONE_SIZE:=16m}"
  : "${WORKERS:=1}"
  : "${WORKER_CONNECTIONS:=4096}"
}

check_always_required() {
  local var
  for var in "${ALWAYS_REQUIRED[@]}"; do
    [[ -n ${!var-} ]] || fail "variável obrigatória ausente em $PARAMS: $var"
  done
}

derive_lines() {
  LISTEN6_LINE=
  [[ -z $LISTEN6_HOST ]] || LISTEN6_LINE="listen [$LISTEN6_HOST]:$PORT;"
  LAN_LISTEN_LINE=
  if [[ ${PHYS-} == 1 ]]; then
    [[ -n ${HOST_LAN_IP-} ]] || fail "PHYS=1 exige HOST_LAN_IP"
    LAN_LISTEN_LINE="listen $HOST_LAN_IP:$PORT;"
  fi
  case $QUEUE_MODE in
    nodelay) QUEUE_FLAG=nodelay ;;
    delay) QUEUE_FLAG= ;;
    *) fail "QUEUE_MODE deve ser nodelay ou delay, recebido '$QUEUE_MODE'" ;;
  esac
}

check_template_vars() {
  local var
  for var in $(grep -ho '\${[A-Za-z0-9_]*}' "$@" | tr -d '${}' | sort -u); do
    [[ " ${SUBST_VARS[*]} " == *" $var "* ]] || fail "template usa variável fora da lista de substituição: $var"
    [[ -v $var ]] || fail "variável obrigatória ausente em $PARAMS: $var"
  done
}

render() {
  envsubst "$(printf '${%s} ' "${SUBST_VARS[@]}")" <"$1" >"$2"
}

main() {
  [[ $# -eq 3 ]] || fail "uso: render.sh <chave> <params.env> <out_dir>"
  local key_tmpl=$HERE/keys/$1.conf.tmpl
  PARAMS=$2
  [[ -f $key_tmpl ]] || fail "chave '$1' sem template em $HERE/keys"
  [[ -f $PARAMS ]] || fail "params.env não existe: $PARAMS"
  mkdir -p "$3"

  set -a
  source "$PARAMS"
  RUN_DIR=$(cd "$3" && pwd)
  apply_defaults
  check_always_required
  derive_lines
  check_template_vars "$HERE/nginx.conf.tmpl" "$key_tmpl"
  set +a

  render "$HERE/nginx.conf.tmpl" "$RUN_DIR/nginx.conf"
  render "$key_tmpl" "$RUN_DIR/run.key.conf"
}

main "$@"
