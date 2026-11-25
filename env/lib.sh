TESTBED_VENV=${TESTBED_VENV:-$HOME/.venvs/testbed}
GO_BIN_DIR=${GO_BIN_DIR:-/usr/local/go/bin}

NGINX_VERSION=1.28.3
NGINX_KEY_URL=https://nginx.org/keys/nginx_signing.key
NGINX_KEY_FINGERPRINT=573BFD6B3D8FBC641079A6ABABF5BD827BD9BF62
NGINX_ORG_REPO=http://nginx.org/packages/ubuntu
GO_VERSION=go1.26.7
GO_JSON_URL='https://go.dev/dl/?mode=json&include=all'
GO_DL_BASE=https://go.dev/dl
MIN_NGINX=1.28
MIN_GO=1.22
CURL_MAX_TIME=60
GO_DOWNLOAD_MAX_TIME=600

testbed_add_go_to_path() {
  if [[ -d $GO_BIN_DIR ]]; then
    PATH=$PATH:$GO_BIN_DIR
  fi
}

version_at_least() {
  local have=$1 want=$2
  [[ -n $have && $(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1) == "$want" ]]
}

nginx_version() {
  nginx -v 2>&1 | sed -n 's|.*nginx/\([0-9][0-9.]*\).*|\1|p'
}

nginx_modules_path() {
  nginx -V 2>&1 | tr ' ' '\n' | sed -n 's|^--modules-path=||p'
}

nginx_has_njs() {
  local modules_path
  modules_path=$(nginx_modules_path)
  [[ -n $modules_path && -f $modules_path/ngx_http_js_module.so ]]
}

go_version() {
  go version | sed -n 's|.* go\([0-9][0-9.]*\).*|\1|p'
}

go_arch() {
  case $1 in
    x86_64) echo amd64 ;;
    aarch64) echo arm64 ;;
    *)
      echo "arquitetura sem build do Go: $1" >&2
      return 1
      ;;
  esac
}

go_archive_field() {
  local json=$1 version=$2 arch=$3 field=$4
  jq -r --arg v "$version" --arg a "$arch" --arg f "$field" '
    limit(1; .[] | select(.version == $v) | .files[]
      | select(.kind == "archive" and .os == "linux" and .arch == $a)
      | .[$f])' "$json"
}

key_fingerprint_present() {
  [[ $1 == *"$NGINX_KEY_FINGERPRINT"* ]]
}
