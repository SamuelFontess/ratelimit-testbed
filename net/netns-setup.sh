#!/usr/bin/env bash
set -euo pipefail

readonly HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly TEARDOWN=$HERE/netns-teardown.sh
readonly NS=gen
readonly HOST_IF=veth-h
readonly GEN_IF=veth-g
readonly HOST_V4=10.77.255.1
readonly GEN_V4=10.77.255.2
readonly V4_PREFIX=30
readonly HOST_V6=fd77:255::1
readonly GEN_V6=fd77:255::2
readonly V6_PREFIX=126
readonly ATTACK_V4=10.66.0.0/16
readonly CONTROL_V4=10.67.0.0/16
readonly ATTACK_V6=fd12:3456:789a::/48
readonly CONTROL_V6=fd12:3456:789b::/48
readonly GEN_PORT_RANGE="1024 65535"
readonly NFT_TABLE=testbed_nat
readonly NFT_CHAIN=postrouting
# O endereço do SNAT tem 64512 portas para cada porta de destino: a 2000 conexões/s cada porta volta a
# ser usada a cada 32 s, e o padrão do conntrack guarda a conexão fechada por 120 s.
readonly GEN_CT_TIME_WAIT=2
readonly FW_COMMENT=testbed-veth-h
readonly IPTABLES=${IPTABLES:-iptables}
readonly IP6TABLES=${IP6TABLES:-ip6tables}
readonly SCENARIO_SNAT=s1
readonly SCENARIOS="s1 s2a s3"

fail() { echo "netns-setup.sh: $*" >&2; exit 1; }

# Sem trap EXIT: ele rodaria também depois do setup completo e derrubaria a rede recém-criada.
on_signal() {
  trap - INT TERM
  echo "netns-setup.sh: interrompido; desfazendo a rede" >&2
  bash "$TEARDOWN" || true
  exit 130
}

trap on_signal INT TERM

# $1 = o que o passo faz, $2.. = comando; a mensagem de erro nomeia o passo que falhou
step() {
  local what=$1; shift
  if "$@"; then return 0; fi
  echo "netns-setup.sh: não conseguiu $what" >&2
  return 1
}

in_gen() { ip netns exec "$NS" "$@"; }

create_links() {
  step "criar o namespace $NS" ip netns add "$NS" &&
    step "criar o par veth $HOST_IF/$GEN_IF" ip link add "$HOST_IF" type veth peer name "$GEN_IF" &&
    step "mover $GEN_IF para $NS" ip link set "$GEN_IF" netns "$NS" &&
    step "levantar $HOST_IF" ip link set "$HOST_IF" up &&
    step "subir o loopback de $NS" in_gen ip link set lo up &&
    step "levantar $GEN_IF" in_gen ip link set "$GEN_IF" up
}

# A imagem de nuvem termina a cadeia INPUT em REJECT: sem a regra o IPv4 de gen não chega ao Nginx da
# raiz. É a única alteração de tabela do sistema feita pelos scripts.
input_is_closed() {
  local cmd=$1 listing policy last
  listing=$("$cmd" -S INPUT 2>/dev/null) || return 1
  policy=$(sed -n 's/^-P INPUT //p' <<<"$listing")
  [[ $policy == DROP ]] && return 0
  last=$(grep '^-A INPUT ' <<<"$listing" | tail -n 1 || true)
  [[ $last =~ [[:space:]]-j[[:space:]]+(REJECT|DROP)([[:space:]]|$) ]]
}

# $1 = comando do firewall; não insere quando ele não existe ou quando a cadeia já aceita por padrão
open_input_for() {
  local cmd=$1
  command -v "$cmd" >/dev/null 2>&1 || return 0
  input_is_closed "$cmd" || return 0
  "$cmd" -C INPUT -i "$HOST_IF" -m comment --comment "$FW_COMMENT" -j ACCEPT 2>/dev/null && return 0
  "$cmd" -I INPUT 1 -i "$HOST_IF" -m comment --comment "$FW_COMMENT" -j ACCEPT
}

open_veth_in_firewall() { open_input_for "$IPTABLES" && open_input_for "$IP6TABLES"; }

add_addresses() {
  step "endereçar $HOST_IF" ip addr add "$HOST_V4/$V4_PREFIX" dev "$HOST_IF" &&
    step "endereçar $HOST_IF em IPv6" ip addr add "$HOST_V6/$V6_PREFIX" dev "$HOST_IF" nodad &&
    step "endereçar $GEN_IF" in_gen ip addr add "$GEN_V4/$V4_PREFIX" dev "$GEN_IF" &&
    step "endereçar $GEN_IF em IPv6" in_gen ip addr add "$GEN_V6/$V6_PREFIX" dev "$GEN_IF" nodad
}

add_routes() {
  step "rotear $ATTACK_V4 pela raiz" ip route add "$ATTACK_V4" via "$GEN_V4" dev "$HOST_IF" &&
    step "rotear $CONTROL_V4 pela raiz" ip route add "$CONTROL_V4" via "$GEN_V4" dev "$HOST_IF" &&
    step "rotear $ATTACK_V6 pela raiz" ip -6 route add "$ATTACK_V6" via "$GEN_V6" dev "$HOST_IF" &&
    step "rotear $CONTROL_V6 pela raiz" ip -6 route add "$CONTROL_V6" via "$GEN_V6" dev "$HOST_IF" &&
    step "tornar $ATTACK_V4 local em $NS" in_gen ip route add local "$ATTACK_V4" dev lo &&
    step "tornar $CONTROL_V4 local em $NS" in_gen ip route add local "$CONTROL_V4" dev lo &&
    step "tornar $ATTACK_V6 local em $NS" in_gen ip -6 route add local "$ATTACK_V6" dev lo &&
    step "tornar $CONTROL_V6 local em $NS" in_gen ip -6 route add local "$CONTROL_V6" dev lo
}

set_gen_sysctls() {
  gen_sysctl "liberar o bind não local em IPv4 em $NS" net.ipv4.ip_nonlocal_bind 1 &&
    gen_sysctl "liberar o bind não local em IPv6 em $NS" net.ipv6.ip_nonlocal_bind 1 &&
    gen_sysctl "definir a faixa de portas de $NS" net.ipv4.ip_local_port_range "$GEN_PORT_RANGE"
}

# O sysctl devolve 0 mesmo quando o kernel recusa a escrita, então o valor é lido de volta.
# $1 = o que o passo faz, $2 = chave, $3 = valor
gen_sysctl() {
  step "$1" gen_sysctl_aplicado "$2" "$3"
}

gen_sysctl_aplicado() {
  local read_back
  in_gen sysctl -qw "$1=$2" &&
    read_back=$(in_gen cat "/proc/sys/${1//.//}") &&
    [[ ${read_back//[$'\t']/ } == "$2" ]]
}

add_snat() {
  step "criar a tabela $NFT_TABLE" in_gen nft add table ip "$NFT_TABLE" &&
    step "criar a cadeia $NFT_CHAIN" in_gen nft add chain ip "$NFT_TABLE" "$NFT_CHAIN" \
      '{ type nat hook postrouting priority srcnat; policy accept; }' &&
    step "criar a regra de SNAT de $ATTACK_V4" in_gen nft add rule ip "$NFT_TABLE" "$NFT_CHAIN" \
      oifname "$GEN_IF" ip saddr "$ATTACK_V4" snat to "$GEN_V4" &&
    gen_sysctl "encurtar o TIME_WAIT do conntrack de $NS" net.netfilter.nf_conntrack_tcp_timeout_time_wait \
      "$GEN_CT_TIME_WAIT"
}

main() {
  [[ $# -le 1 ]] || fail "uso: netns-setup.sh [s1|s2a|s3]"
  local scenario=${1:-s1}
  [[ " $SCENARIOS " == *" $scenario "* ]] || fail "cenário '$scenario' inválido: use um de $SCENARIOS"
  [[ $EUID -eq 0 ]] || fail "precisa de root: sudo -n $0 $scenario"
  bash "$TEARDOWN" || fail "a limpeza anterior à criação da rede"

  create_links && open_veth_in_firewall && add_addresses && add_routes && set_gen_sysctls ||
    rollback_and_fail
  if [[ $scenario == "$SCENARIO_SNAT" ]]; then
    add_snat || rollback_and_fail
  fi
  trap - INT TERM
}

rollback_and_fail() {
  trap - INT TERM
  bash "$TEARDOWN"
  fail "falhou ao montar a rede sintética ($scenario); o que foi criado foi desfeito"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
