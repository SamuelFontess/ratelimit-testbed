#!/usr/bin/env bash
# Gate das tarefas do testbed. Uso: gate.sh rapido|completo
#   rapido   : Go e shell só do que mudou contra $GATE_BASE (padrão main); Python sempre.
#   completo : tudo, mais env/check_env.sh.
# Regra: se um componente existe, o passo dele é obrigatório. Passo ausente por falta do
# componente aparece como SKIP e nunca esconde falha de componente existente.
set -uo pipefail

# Go fica em /usr/local/go/bin, fora do PATH de shell não interativo.
[[ -d /usr/local/go/bin ]] && PATH=$PATH:/usr/local/go/bin

MODE=${1:-rapido}
BASE=${GATE_BASE:-main}
cd "$(dirname "$0")"
ROOT=$PWD
LOG=${GATE_LOG:-$ROOT/check.tempos.log}
: > "$LOG"
FAIL=0

# git: nativo; em worktree criado pelo Windows o .git aponta para um caminho C:/ que o git do
# WSL não resolve, então cai para git.exe. Sem git funcional o gate falha (guarda nunca fica vazia).
if git rev-parse --git-dir >/dev/null 2>&1; then
  g() { git "$@"; }
elif command -v git.exe >/dev/null 2>&1 && WINROOT=$(wslpath -w "$ROOT") && git.exe -C "$WINROOT" rev-parse --git-dir >/dev/null 2>&1; then
  g() { git.exe -C "$WINROOT" "$@"; }
else
  echo "FAIL git inacessível: nem git nem git.exe enxergam o repositório" | tee -a "$LOG"; exit 1
fi

step() {
  local name=$1; shift
  local t0=$SECONDS
  if "$@"; then echo "PASS $name $((SECONDS - t0))s" | tee -a "$LOG"
  else echo "FAIL $name $((SECONDS - t0))s" | tee -a "$LOG"; FAIL=1; fi
}
skip() { echo "SKIP $1 ($2)" | tee -a "$LOG"; }

if [[ $MODE == rapido ]] && ! g rev-parse --verify -q "$BASE" >/dev/null; then
  echo "base '$BASE' não existe; usando modo completo" | tee -a "$LOG"
  MODE=completo
fi

changed() {
  { g diff --name-only "$BASE"...HEAD; g diff --name-only HEAD; g ls-files -o --exclude-standard; } 2>/dev/null | tr -d '\r' | sort -u
}
CHANGED=""
[[ $MODE == rapido ]] && CHANGED=$(changed)

touched() { # dir -> 0 se o modo é completo ou algo mudou sob dir
  [[ $MODE == completo ]] && return 0
  grep -q "^${1#./}/" <<<"$CHANGED"
}

# 1. Guarda de segredos: nenhum arquivo rastreado pode ser config de roteador, snapshot ou credencial.
no_secrets() {
  local files bad
  files=$(g ls-files) || { echo "git ls-files falhou"; return 1; }
  bad=$(tr -d '\r' <<<"$files" | grep -Ei '\.(cfg|dpapi)$|credenciais|recuperacao|(^|/)\.env$' || true)
  [[ -z $bad ]] || { echo "arquivos proibidos no git:"; echo "$bad"; return 1; }
}
step segredos no_secrets

# 2. Shell: sintaxe de todo .sh (rápido: só os alterados).
sh_syntax() {
  local f rc=0
  while IFS= read -r f; do
    [[ -n $f ]] || continue
    if [[ $MODE == rapido ]] && ! grep -qx "${f#./}" <<<"$CHANGED"; then continue; fi
    bash -n "$f" || rc=1
  done < <(find . -name '*.sh' -not -path './.venv/*' -not -path './runs/*')
  return $rc
}
step shell-sintaxe sh_syntax

# 3. Go: vet e test em cada módulo.
while IFS= read -r mod; do
  d=$(dirname "$mod")
  if touched "$d"; then step "go:$d" bash -c "cd '$d' && go vet ./... && go test -count=1 ./..."
  else skip "go:$d" "sem alteração"; fi
done < <(find . -name go.mod -not -path './.venv/*')

# 4. Python: se há testes, o venv é obrigatório. Fica fora do repo e é compartilhado entre
#    worktrees: $TESTBED_VENV, padrão ~/.venvs/testbed (criado por env/setup.sh).
VENV=${TESTBED_VENV:-$HOME/.venvs/testbed}
if find . \( -name '*_test.py' -o -name 'test_*.py' \) -not -path './.venv/*' | grep -q .; then
  if [[ -x $VENV/bin/python ]]; then
    step pytest "$VENV/bin/python" -m pytest -q -p no:cacheprovider
  else
    echo "FAIL pytest (testes existem, mas o venv $VENV não existe: rode env/setup.sh)" | tee -a "$LOG"; FAIL=1
  fi
else
  skip pytest "sem testes"
fi

if [[ -f spike/g4_schedule.sh ]]; then
  if touched gen/legit || touched spike; then step g4-agenda bash spike/g4_schedule.sh; else skip g4-agenda "sem alteração"; fi
fi

# 5. Nginx: config renderizada de cada chave precisa passar em nginx -t (entregue pela task do Nginx).
if [[ -f nginx/check.sh ]]; then step nginx-config bash nginx/check.sh; else skip nginx-config "nginx/check.sh ainda não existe"; fi

# 6. Completo: ambiente.
if [[ $MODE == completo ]]; then
  if [[ -f env/check_env.sh ]]; then step ambiente bash env/check_env.sh; else skip ambiente "env/check_env.sh ainda não existe"; fi
fi

echo "gate $MODE: $([[ $FAIL -eq 0 ]] && echo VERDE || echo VERMELHO)" | tee -a "$LOG"
exit $FAIL
