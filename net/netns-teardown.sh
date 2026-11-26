#!/usr/bin/env bash
set -uo pipefail

readonly NS=gen
readonly HOST_IF=veth-h
readonly FW_COMMENT=testbed-veth-h
readonly IPTABLES=${IPTABLES:-iptables}
readonly IP6TABLES=${IP6TABLES:-ip6tables}
readonly ATTACK_V4=10.66.0.0/16
readonly CONTROL_V4=10.67.0.0/16
readonly ATTACK_V6=fd12:3456:789a::/48
readonly CONTROL_V6=fd12:3456:789b::/48
readonly NETNS_GONE_ATTEMPTS=50
readonly NETNS_GONE_STEP=0.1

fail() { echo "netns-teardown.sh: $*" >&2; exit 1; }

# Remove só a regra que o setup inseriu, pelo mesmo especificador; outras regras da cadeia ficam.
# $1 = comando do firewall; não faz nada quando ele não existe.
remove_input_rule() {
  local cmd=$1
  command -v "$cmd" >/dev/null 2>&1 || return 0
  while "$cmd" -C INPUT -i "$HOST_IF" -m comment --comment "$FW_COMMENT" -j ACCEPT 2>/dev/null; do
    "$cmd" -D INPUT -i "$HOST_IF" -m comment --comment "$FW_COMMENT" -j ACCEPT ||
      fail "não removeu a regra de INPUT da $HOST_IF em $cmd"
  done
}

remove_input_rules() { remove_input_rule "$IPTABLES" && remove_input_rule "$IP6TABLES"; }

# $1 = o que é removido, $2 = comando que responde 0 quando existe, $3.. = remoção
remove_if_present() {
  local what=$1; shift
  local exists=$1; shift
  "$exists" || return 0
  "$@" || fail "não removeu $what"
}

netns_exists() { ip netns list | awk '{print $1}' | grep -qx "$NS"; }
link_exists() { ip link show "$HOST_IF" >/dev/null 2>&1; }

# A exclusão do namespace é assíncrona: o veth-h some um instante depois do comando voltar.
netns_gone() {
  local i
  for ((i = 0; i < NETNS_GONE_ATTEMPTS; i++)); do
    link_exists || return 0
    sleep "$NETNS_GONE_STEP"
  done
  return 1
}

remove_netns() { ip netns del "$NS" && netns_gone; }
remove_link() { ip link del "$HOST_IF"; }

remove_host_route() {
  local family=$1 prefix=$2
  local routes
  routes=$(ip "$family" route show exact "$prefix") || fail "não leu a rota raiz $prefix"
  [[ -z $routes ]] && return 0
  ip "$family" route del "$prefix" >/dev/null 2>&1
  routes=$(ip "$family" route show exact "$prefix")
  [[ -z $routes ]] || fail "não removeu a rota raiz $prefix"
}

remove_host_routes() {
  local prefix
  for prefix in "$ATTACK_V4" "$CONTROL_V4"; do
    remove_host_route -4 "$prefix"
  done
  for prefix in "$ATTACK_V6" "$CONTROL_V6"; do
    remove_host_route -6 "$prefix"
  done
}

main() {
  [[ $# -eq 0 ]] || fail "uso: netns-teardown.sh"
  remove_input_rules
  remove_if_present "o namespace $NS" netns_exists remove_netns
  remove_if_present "a interface $HOST_IF" link_exists remove_link
  remove_host_routes
}

main "$@"
