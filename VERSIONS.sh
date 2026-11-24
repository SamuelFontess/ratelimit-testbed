#!/usr/bin/env bash
set -euo pipefail
PATH=$PATH:/usr/local/go/bin
{
  uname -a
  nginx -v 2>&1
  go version
  python3 --version
  if command -v cloudflared >/dev/null; then cloudflared --version 2>&1 | head -n1; fi
} | sed 's/^/version: /'
