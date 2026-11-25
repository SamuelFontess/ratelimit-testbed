#!/usr/bin/env bash
set -euo pipefail

ENV_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$ENV_DIR/lib.sh"
testbed_add_go_to_path

MIN_NGINX=1.28
MIN_GO=1.22
NETNS_PROBE=_chk_$$
BINARIES=(ip nft conntrack envsubst curl jq go python3)
PY_IMPORTS="import numpy, scipy, matplotlib, hypothesis, pytest, pandas, jinja2"

FAILED=0
WORK_DIR=$(mktemp -d)

cleanup() {
  sudo -n ip netns del "$NETNS_PROBE" 2>/dev/null || true
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

report() {
  local item=$1; shift
  if "$@" >/dev/null; then
    echo "OK $item"
  else
    echo "FALTA $item"
    FAILED=1
  fi
}

version_at_least() {
  local have=$1 want=$2
  [[ -n $have && $(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1) == "$want" ]]
}

nginx_version() {
  nginx -v 2>&1 | sed -n 's|.*nginx/\([0-9][0-9.]*\).*|\1|p'
}

go_version() {
  go version | sed -n 's|.* go\([0-9][0-9.]*\).*|\1|p'
}

nginx_recent() { version_at_least "$(nginx_version)" "$MIN_NGINX"; }
go_recent() { version_at_least "$(go_version)" "$MIN_GO"; }

njs_loadable() {
  local modules_path module conf
  modules_path=$(nginx -V 2>&1 | tr ' ' '\n' | sed -n 's|^--modules-path=||p')
  module=$modules_path/ngx_http_js_module.so
  [[ -n $modules_path && -f $module ]] || return 1
  conf=$WORK_DIR/njs.conf
  printf 'pid %s/nginx.pid;\nload_module %s;\nevents {}\nhttp {}\n' "$WORK_DIR" "$module" >"$conf"
  nginx -t -c "$conf" -e "$WORK_DIR/error.log" -p "$WORK_DIR"
}

venv_imports() {
  "$TESTBED_VENV/bin/python" -c "$PY_IMPORTS"
}

netns_creatable() {
  sudo -n ip netns add "$NETNS_PROBE" && sudo -n ip netns del "$NETNS_PROBE"
}

for bin in "${BINARIES[@]}"; do
  report "binario:$bin" command -v "$bin"
done
report "nginx>=$MIN_NGINX" nginx_recent
report "go>=$MIN_GO" go_recent
report "njs:ngx_http_js_module" njs_loadable
report "venv:$TESTBED_VENV" venv_imports
report "netns:sudo-ip-netns" netns_creatable

exit "$FAILED"
