#!/usr/bin/env bash
set -euo pipefail

readonly HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly NGINX=${NGINX:-nginx}
readonly LOOPBACK_LAN_IP=127.0.0.3
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

nginx_test() {
  local key=$1 dir=$2
  "$NGINX" -t -c "$dir/nginx.conf" -p "$dir" -e "$dir/nginx.error.log" ||
    { echo "check.sh: nginx -t falhou para a chave $key" >&2; return 1; }
}

check_key() {
  local tmpl=$1 key
  key=$(basename "$tmpl" .conf.tmpl)
  bash "$HERE/render.sh" "$key" "$HERE/sample.params.env" "$TMP/$key"
  nginx_test "$key" "$TMP/$key"
}

check_lan_listen() {
  local dir=$TMP/lan
  HOST_LAN_IP=$LOOPBACK_LAN_IP bash "$HERE/render.sh" k1 "$HERE/sample.params.env" "$dir"
  if grep -qF "listen $LOOPBACK_LAN_IP" "$dir/run.key.conf"; then
    echo "check.sh: HOST_LAN_IP sem PHYS=1 vazou para a config" >&2
    return 1
  fi
  PHYS=1 HOST_LAN_IP=$LOOPBACK_LAN_IP bash "$HERE/render.sh" k1 "$HERE/sample.params.env" "$dir"
  grep -qF "listen $LOOPBACK_LAN_IP:" "$dir/run.key.conf" ||
    { echo "check.sh: PHYS=1 não gerou listen na LAN" >&2; return 1; }
  nginx_test k1-phys "$dir"
}

main() {
  local tmpl
  for tmpl in "$HERE"/keys/*.conf.tmpl; do
    check_key "$tmpl"
  done
  check_lan_listen
}

main
