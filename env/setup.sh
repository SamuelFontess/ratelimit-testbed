#!/usr/bin/env bash
set -euo pipefail

ENV_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$ENV_DIR/lib.sh"

BASE_PACKAGES=(
  nftables conntrack jq gettext-base curl ca-certificates
  gnupg2 lsb-release python3-venv python3-pip
)
NGINX_KEYRING=/usr/share/keyrings/nginx-archive-keyring.gpg
NGINX_ORG_SOURCE=/etc/apt/sources.list.d/nginx.list
NGINX_ORG_PREFS=/etc/apt/preferences.d/99nginx

APT_UPDATED=0
TMP_DIR=

apt_update() {
  [[ $APT_UPDATED == 1 ]] && return
  sudo apt-get update -qq
  APT_UPDATED=1
}

install_base_packages() {
  local pkg missing=()
  for pkg in "${BASE_PACKAGES[@]}"; do
    dpkg-query -W -f='${db:Status-Status}' "$pkg" 2>/dev/null | grep -qx installed ||
      missing+=("$pkg")
  done
  [[ ${#missing[@]} -eq 0 ]] && return
  apt_update
  sudo apt-get install -y "${missing[@]}"
}

nginx_ready() {
  command -v nginx >/dev/null 2>&1 &&
    version_at_least "$(nginx_version)" "$MIN_NGINX" &&
    nginx_has_njs
}

nginx_distro_candidate() {
  apt-cache policy nginx | sed -n 's/^  Candidate: //p'
}

njs_version_for() {
  apt-cache madison nginx-module-njs | awk -v v="$1" 'index($3, v "+") == 1 {print $3; exit}'
}

install_nginx_from_distro() {
  apt_update
  sudo apt-get install -y nginx libnginx-mod-http-js
}

install_nginx_from_nginx_org() {
  local codename njs_version listing
  codename=$(lsb_release -cs)
  curl --max-time "$CURL_MAX_TIME" -fsSL "$NGINX_KEY_URL" -o "$TMP_DIR/nginx_signing.key"
  gpg --dearmor <"$TMP_DIR/nginx_signing.key" >"$TMP_DIR/nginx-archive-keyring.gpg"
  mkdir -p -m 700 "$TMP_DIR/gnupg"
  listing=$(GNUPGHOME=$TMP_DIR/gnupg gpg --dry-run --quiet --no-keyring --import \
    --import-options import-show "$TMP_DIR/nginx-archive-keyring.gpg" 2>/dev/null)
  key_fingerprint_present "$listing" || {
    echo "setup: chave do nginx.org sem o fingerprint $NGINX_KEY_FINGERPRINT" >&2
    exit 1
  }
  sudo install -m 0644 "$TMP_DIR/nginx-archive-keyring.gpg" "$NGINX_KEYRING"
  printf 'deb [signed-by=%s] %s %s nginx\n' "$NGINX_KEYRING" "$NGINX_ORG_REPO" "$codename" |
    sudo tee "$NGINX_ORG_SOURCE" >/dev/null
  printf 'Package: *\nPin: origin nginx.org\nPin: release o=nginx\nPin-Priority: 900\n' |
    sudo tee "$NGINX_ORG_PREFS" >/dev/null
  sudo apt-get update -qq
  APT_UPDATED=1
  njs_version=$(njs_version_for "$NGINX_VERSION")
  [[ -n $njs_version ]] || {
    echo "setup: nginx-module-njs para $NGINX_VERSION fora do repositório nginx.org" >&2
    exit 1
  }
  sudo apt-get install -y "nginx=$NGINX_VERSION-1~$codename" "nginx-module-njs=$njs_version"
  sudo apt-mark hold nginx nginx-module-njs
  sudo systemctl disable --now nginx
}

go_ready() {
  command -v go >/dev/null 2>&1 && version_at_least "$(go_version)" "$MIN_GO"
}

install_go() {
  local arch name sha
  arch=$(go_arch "$(uname -m)")
  curl --max-time "$CURL_MAX_TIME" -fsSL "$GO_JSON_URL" -o "$TMP_DIR/go.json"
  name=$(go_archive_field "$TMP_DIR/go.json" "$GO_VERSION" "$arch" filename)
  sha=$(go_archive_field "$TMP_DIR/go.json" "$GO_VERSION" "$arch" sha256)
  [[ -n $name && -n $sha ]] || {
    echo "setup: $GO_VERSION/$arch ausente no JSON do go.dev" >&2
    exit 1
  }
  curl --max-time "$GO_DOWNLOAD_MAX_TIME" -fsSL "$GO_DL_BASE/$name" -o "$TMP_DIR/$name"
  printf '%s  %s\n' "$sha" "$name" | (cd "$TMP_DIR" && sha256sum -c --strict -)
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$TMP_DIR/$name"
}

install_nginx() {
  if version_at_least "$(nginx_distro_candidate)" "$MIN_NGINX"; then
    install_nginx_from_distro
  else
    install_nginx_from_nginx_org
  fi
}

main() {
  testbed_add_go_to_path
  TMP_DIR=$(mktemp -d)
  trap 'rm -rf "$TMP_DIR"' EXIT

  install_base_packages
  if ! nginx_ready; then
    install_nginx
  fi
  if ! go_ready; then
    install_go
  fi

  if [[ ! -x $TESTBED_VENV/bin/python ]]; then
    python3 -m venv "$TESTBED_VENV"
  fi
  "$TESTBED_VENV/bin/python" -m pip install --disable-pip-version-check -r "$ENV_DIR/requirements.txt"

  rm -rf "$TMP_DIR"
  trap - EXIT
  exec bash "$ENV_DIR/check_env.sh"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
