TESTBED_VENV=${TESTBED_VENV:-$HOME/.venvs/testbed}
GO_BIN_DIR=${GO_BIN_DIR:-/usr/local/go/bin}

testbed_add_go_to_path() {
  if [[ -d $GO_BIN_DIR ]]; then
    PATH=$PATH:$GO_BIN_DIR
  fi
}
