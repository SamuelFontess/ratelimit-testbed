#!/usr/bin/env bash
set -euo pipefail

ENV_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$ENV_DIR/lib.sh"

sudo apt-get update -qq
sudo apt-get install -y nftables conntrack jq libnginx-mod-http-js python3-venv

if [[ ! -x $TESTBED_VENV/bin/python ]]; then
  python3 -m venv "$TESTBED_VENV"
fi
"$TESTBED_VENV/bin/python" -m pip install -r "$ENV_DIR/requirements.txt"

exec bash "$ENV_DIR/check_env.sh"
