import ipaddress
import os
import pty
import re
import select
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

RAIZ = Path(__file__).resolve().parents[1]
NET = RAIZ / "net"
SETUP = NET / "netns-setup.sh"
TEARDOWN = NET / "netns-teardown.sh"
GATE_LIB = RAIZ / "spike" / "gate_lib.sh"
FW_WINDOWS = NET / "fw-windows.ps1"
PREFIXOS_RAIZ = ["10.66.0.0/16", "10.67.0.0/16", "fd12:3456:789a::/48", "fd12:3456:789b::/48"]
TABELA_NAT = ["nft", "list", "table", "ip", "testbed_nat"]
PORTA_ALVO = 18095
TEMPO_COMANDO = 60
TEMPO_CONEXAO = 5
FAIXA_GEN = "1024 65535"
PROC_FAIXA_PORTAS = "/proc/sys/net/ipv4/ip_local_port_range"
PROC_CT_TIME_WAIT = "/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_time_wait"
CT_TIME_WAIT_GEN = "2"
PROIBIDOS = ["10.0.0.0/24", "100.64.0.0/10", "26.0.0.0/8", "192.168.56.0/24"]
REGRA_INPUT = "-I INPUT 1 -i veth-h -m comment --comment testbed-veth-h -j ACCEPT"
REMOCAO_INPUT = "-D INPUT -i veth-h -m comment --comment testbed-veth-h -j ACCEPT"
CADEIA_FECHADA = [
    "-A INPUT -p tcp --dport 22 -j ACCEPT",
    "-A INPUT -j REJECT --reject-with icmp-host-prohibited",
]
CADEIA_ABERTA = ["-A INPUT -p tcp --dport 22 -j ACCEPT"]
# Valor de IP_FREEBIND no Linux: o módulo socket do Python 3.12 não exporta a constante.
IP_FREEBIND = 15
TOKEN_ENDERECO = re.compile(
    r"(?<![\w.])(?:\d{1,3}(?:\.\d{1,3}){3}|[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){2,7})(?:/\d{1,3})?(?![\w.])"
)

# O cliente roda em gen com a origem fora do veth, como o atacante faz, e avisa o alvo.
CLIENTE = """
import socket, sys
origem, alvo, porta, freebind = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
familia = socket.AF_INET6 if ":" in origem else socket.AF_INET
with socket.socket(familia, socket.SOCK_STREAM) as s:
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, b"veth-g")
    s.setsockopt(socket.IPPROTO_IP, freebind, 1)
    s.settimeout(5)
    s.bind((origem, 0))
    s.connect((alvo, porta))
    s.sendall(b"aviso")
"""


def sudo_ok():
    if shutil.which("sudo") is None:
        return False
    try:
        return subprocess.run(["sudo", "-n", "true"], capture_output=True, timeout=TEMPO_COMANDO).returncode == 0
    except subprocess.TimeoutExpired:
        return False


pytestmark = pytest.mark.skipif(not sudo_ok(), reason="testes de rede precisam de sudo -n")


def sudo(*args):
    return subprocess.run(["sudo", "-n", *args], capture_output=True, text=True, timeout=TEMPO_COMANDO)


def rede(*args):
    return sudo("bash", str(SETUP), *args)


def em_gen(*args):
    return sudo("ip", "netns", "exec", "gen", *args)


def rotas_raiz():
    rotas = []
    for prefixo in PREFIXOS_RAIZ:
        familia = "-6" if ":" in prefixo else "-4"
        rotas.append(sudo("ip", familia, "route", "show", "exact", prefixo).stdout.strip())
    return rotas


def tabelas_raiz():
    return sorted(linha.strip() for linha in sudo("nft", "list", "tables").stdout.splitlines())


def nada_existe():
    assert "gen" not in sudo("ip", "netns", "list").stdout.split()
    assert sudo("ip", "link", "show", "veth-h").returncode != 0
    assert not any(rotas_raiz())


def firewall_falso(tmp_path, nome, policy="ACCEPT", regras=()):
    log = tmp_path / f"{nome}.log"
    estado = tmp_path / f"{nome}.regra"
    linhas = [f"-P INPUT {policy}", *regras]
    script = tmp_path / nome
    script.write_text(
        "#!/bin/sh\n"
        f'printf "%s\\n" "$*" >> {shlex.quote(str(log))}\n'
        'case "$1" in\n'
        f"  -S) printf '%s\\n' {' '.join(shlex.quote(linha) for linha in linhas)} ;;\n"
        f"  -C) [ -e {shlex.quote(str(estado))} ] ;;\n"
        f"  -I) : > {shlex.quote(str(estado))} ;;\n"
        f"  -D) rm -f {shlex.quote(str(estado))} ;;\n"
        "esac\n"
    )
    script.chmod(0o755)
    return script, log, estado


def ambiente_firewall(iptables, ip6tables):
    return ["env", f"IPTABLES={iptables}", f"IP6TABLES={ip6tables}"]


def chamadas(log, prefixo):
    return [linha for linha in log.read_text().splitlines() if linha.startswith(prefixo)]


def esperar_marcador(fd, marcador, limite=30):
    fim = time.monotonic() + limite
    while time.monotonic() < fim:
        if marcador.exists():
            return
        legivel, _, _ = select.select([fd], [], [], 0.05)
        if legivel:
            try:
                if not os.read(fd, 4096):
                    break
            except OSError:
                break
    pytest.fail(f"o setup não chegou ao sysctl em {limite}s")


def ler_ate_fechar(fd, limite=TEMPO_COMANDO):
    saida = b""
    fim = time.monotonic() + limite
    while time.monotonic() < fim:
        legivel, _, _ = select.select([fd], [], [], 0.5)
        if not legivel:
            continue
        try:
            dados = os.read(fd, 4096)
        except OSError:
            return saida
        if not dados:
            return saida
        saida += dados
    return None


@pytest.fixture
def limpo():
    resultado = sudo("bash", str(TEARDOWN))
    assert resultado.returncode == 0, resultado.stderr
    nada_existe()
    yield
    sudo("bash", str(TEARDOWN))


@pytest.fixture
def rede_s1(limpo):
    resultado = rede("s1")
    assert resultado.returncode == 0, resultado.stderr


def origem_vista_pelo_alvo(origem, alvo, familia):
    """Sobe um alvo TCP em alvo, conecta a ele a partir de gen com a origem dada e devolve o par visto."""
    vista = {}

    def servir():
        with socket.socket(familia) as escuta:
            escuta.settimeout(TEMPO_CONEXAO)
            escuta.bind((alvo, PORTA_ALVO))
            escuta.listen(1)
            conexao, par = escuta.accept()
            with conexao:
                vista["par"] = par[0]

    tarefa = threading.Thread(target=servir, daemon=True)
    tarefa.start()
    cliente = em_gen(sys.executable, "-c", CLIENTE, origem, alvo, str(PORTA_ALVO), str(IP_FREEBIND))
    tarefa.join(TEMPO_CONEXAO * 2)
    assert not tarefa.is_alive(), f"nenhuma conexão chegou a {alvo} a partir de {origem}: {cliente.stderr}"
    return vista["par"]


def test_teardown_sem_nada_retorna_zero(limpo):
    primeiro = sudo("bash", str(TEARDOWN))
    segundo = sudo("bash", str(TEARDOWN))
    assert (primeiro.returncode, segundo.returncode) == (0, 0), primeiro.stderr + segundo.stderr
    nada_existe()


def enderecos_veth():
    return [linha.split(" ", 1)[1] for linha in sudo("ip", "-o", "addr", "show", "dev", "veth-h").stdout.splitlines()]


def test_setup_idempotente(rede_s1):
    enderecos = enderecos_veth()
    tabela = em_gen(*TABELA_NAT).stdout
    rotas = rotas_raiz()
    assert "snat to 10.77.255.2" in tabela
    assert rede("s1").returncode == 0
    assert enderecos_veth() == enderecos
    assert em_gen(*TABELA_NAT).stdout == tabela
    assert rotas_raiz() == rotas


def test_rotas_da_raiz_apontam_para_o_veth(rede_s1):
    rotas = rotas_raiz()
    assert all("dev veth-h" in rota for rota in rotas)
    assert [rota.split(" via ")[1].split()[0] for rota in rotas] == ["10.77.255.2"] * 2 + ["fd77:255::2"] * 2


@pytest.mark.parametrize("cenario", ["s1", "s2a", "s3"])
def test_snat_so_em_s1(limpo, cenario):
    tabelas_antes = tabelas_raiz()
    tw_raiz_antes = ler_arquivo(PROC_CT_TIME_WAIT)
    resultado = rede(cenario)
    assert resultado.returncode == 0, resultado.stderr
    tabela = em_gen(*TABELA_NAT)
    if cenario == "s1":
        assert tabela.returncode == 0, tabela.stderr
        assert "ip saddr 10.66.0.0/16" in tabela.stdout
        assert "snat to 10.77.255.2" in tabela.stdout
        assert "10.67.0.0/16" not in tabela.stdout
        assert em_gen("cat", PROC_CT_TIME_WAIT).stdout.strip() == CT_TIME_WAIT_GEN
    else:
        assert tabela.returncode != 0
    assert tabelas_raiz() == tabelas_antes
    assert ler_arquivo(PROC_CT_TIME_WAIT) == tw_raiz_antes


def test_setup_falha_quando_o_kernel_recusa_um_sysctl(limpo, tmp_path):
    # O sysctl real devolve 0 quando o kernel recusa a escrita; o setup não pode aceitar isso como sucesso.
    falho = tmp_path / "sysctl"
    falho.write_text('#!/bin/sh\necho "sysctl: setting key: Operation not permitted" >&2\nexit 0\n')
    falho.chmod(0o755)
    resultado = sudo("env", f"PATH={tmp_path}:{os.environ['PATH']}", "bash", str(SETUP), "s1")
    assert resultado.returncode != 0
    assert "não conseguiu liberar o bind não local em IPv4" in resultado.stderr
    nada_existe()


def ler_arquivo(caminho):
    try:
        return Path(caminho).read_text()
    except FileNotFoundError:
        return ""


def test_faixa_de_portas_so_no_gen(limpo):
    faixa_antes = Path(PROC_FAIXA_PORTAS).read_text()
    assert rede("s1").returncode == 0
    assert em_gen("cat", PROC_FAIXA_PORTAS).stdout.split() == FAIXA_GEN.split()
    assert Path(PROC_FAIXA_PORTAS).read_text() == faixa_antes


def test_teardown_remove_tudo(rede_s1):
    assert sudo("bash", str(TEARDOWN)).returncode == 0
    nada_existe()


def test_sem_prefixo_proibido():
    enderecos = [
        (caminho.name, achado)
        for caminho in sorted(NET.iterdir())
        if caminho.suffix in (".sh", ".ps1")
        for achado in TOKEN_ENDERECO.findall(caminho.read_text())
    ]
    assert enderecos
    for nome, achado in enderecos:
        rede_achada = ipaddress.ip_network(achado, strict=False)
        for proibido in PROIBIDOS:
            assert not rede_achada.overlaps(ipaddress.ip_network(proibido)), f"{nome}: {achado} invade {proibido}"


def test_ipv4_e_ipv6_funcionam(rede_s1):
    # O SNAT do S1 reescreve só 10.66.0.0/16 (C-NET): o resto chega com a origem que saiu de gen.
    casos = [
        ("10.66.3.7", "10.77.255.1", socket.AF_INET, "10.77.255.2"),
        ("10.67.0.9", "10.77.255.1", socket.AF_INET, "10.67.0.9"),
        ("fd12:3456:789a::7", "fd77:255::1", socket.AF_INET6, "fd12:3456:789a::7"),
        ("fd12:3456:789b::7", "fd77:255::1", socket.AF_INET6, "fd12:3456:789b::7"),
    ]
    for origem, alvo, familia, esperado in casos:
        assert origem_vista_pelo_alvo(origem, alvo, familia) == esperado


def test_setup_desfaz_o_que_criou_ao_falhar(limpo, tmp_path):
    falho = tmp_path / "sysctl"
    falho.write_text("#!/bin/sh\nexit 1\n")
    falho.chmod(0o755)
    resultado = sudo("env", f"PATH={tmp_path}:{os.environ['PATH']}", "bash", str(SETUP), "s1")
    assert resultado.returncode != 0
    assert "não conseguiu liberar o bind não local em IPv4" in resultado.stderr
    nada_existe()


def test_fw_windows_tem_sintaxe_valida():
    powershell = shutil.which("powershell.exe") or shutil.which("pwsh.exe")
    if powershell is None:
        pytest.skip("sem PowerShell para validar a sintaxe")
    caminho = subprocess.run(
        ["wslpath", "-w", str(FW_WINDOWS)], capture_output=True, text=True, timeout=TEMPO_COMANDO
    ).stdout.strip()
    sintaxe = (
        "$t = $null; $e = $null; "
        f"[void][System.Management.Automation.Language.Parser]::ParseFile('{caminho}', [ref]$t, [ref]$e); $e"
    )
    resultado = subprocess.run([powershell, "-NoProfile", "-Command", sintaxe], capture_output=True, text=True, timeout=TEMPO_COMANDO)
    assert resultado.returncode == 0 and not resultado.stdout.strip(), resultado.stdout + resultado.stderr


SYSCTL_LENTO = """#!/bin/sh
: > {marcador}
sleep 120
"""


def test_sigint_no_meio_do_setup_desfaz_a_rede(limpo, tmp_path):
    sysctl = tmp_path / "sysctl"
    marcador = tmp_path / "chamou"
    sysctl.write_text(SYSCTL_LENTO.format(marcador=shlex.quote(str(marcador))))
    sysctl.chmod(0o755)
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp(
            "sudo",
            [
                "sudo",
                "-n",
                "env",
                f"PATH={tmp_path}:{os.environ['PATH']}",
                "bash",
                str(SETUP),
                "s1",
            ],
        )
    estado = None
    try:
        esperar_marcador(fd, marcador)
        os.write(fd, b"\x03")
        saida = ler_ate_fechar(fd)
        assert saida is not None, "o setup não terminou depois do Ctrl-C"
        _, estado = os.waitpid(pid, 0)
    finally:
        if estado is None:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(fd)
    assert os.waitstatus_to_exitcode(estado) == 130, saida
    nada_existe()


def test_sigterm_no_setup_sai_130(limpo):
    processo = subprocess.Popen(
        ["bash", "-c", f'source "{SETUP}"\n(echo pronto; exec sleep 30) & wait'],
        start_new_session=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        assert processo.stdout.readline().strip() == "pronto"
        os.killpg(processo.pid, signal.SIGTERM)
        _, erro = processo.communicate(timeout=TEMPO_COMANDO)
    finally:
        if processo.poll() is None:
            processo.kill()
            processo.communicate()
    assert processo.returncode == 130, erro
    nada_existe()


def test_setup_insere_e_teardown_remove_so_a_regra_da_veth(limpo, tmp_path):
    iptables, log, estado = firewall_falso(tmp_path, "iptables", regras=CADEIA_FECHADA)
    ip6tables, log6, _ = firewall_falso(tmp_path, "ip6tables", regras=CADEIA_ABERTA)
    ambiente = ambiente_firewall(iptables, ip6tables)

    resultado = sudo(*ambiente, "bash", str(SETUP), "s1")
    assert resultado.returncode == 0, resultado.stderr
    assert chamadas(log, "-I ") == [REGRA_INPUT]
    assert chamadas(log, "-D ") == []

    resultado = sudo(*ambiente, "bash", str(TEARDOWN))
    assert resultado.returncode == 0, resultado.stderr
    assert chamadas(log, "-I ") == [REGRA_INPUT]
    assert chamadas(log, "-D ") == [REMOCAO_INPUT]
    assert not estado.exists()
    assert chamadas(log6, "-I ") == [] and chamadas(log6, "-D ") == []
    nada_existe()


def test_insercao_da_regra_e_idempotente(tmp_path):
    iptables, log, _ = firewall_falso(tmp_path, "iptables", regras=CADEIA_FECHADA)
    ip6tables, _, _ = firewall_falso(tmp_path, "ip6tables", regras=CADEIA_ABERTA)
    resultado = subprocess.run(
        ["bash", "-c", f'source "{SETUP}"\nopen_veth_in_firewall\nopen_veth_in_firewall'],
        capture_output=True,
        text=True,
        timeout=TEMPO_COMANDO,
        env={**os.environ, "IPTABLES": str(iptables), "IP6TABLES": str(ip6tables)},
    )
    assert resultado.returncode == 0, resultado.stderr
    assert chamadas(log, "-I ") == [REGRA_INPUT]


def test_politica_drop_insere_a_regra(tmp_path):
    iptables, log, _ = firewall_falso(tmp_path, "iptables", policy="DROP")
    resultado = subprocess.run(
        ["bash", "-c", f'source "{SETUP}"\nopen_veth_in_firewall'],
        capture_output=True,
        text=True,
        timeout=TEMPO_COMANDO,
        env={**os.environ, "IPTABLES": str(iptables), "IP6TABLES": str(tmp_path / "sem-ip6tables")},
    )
    assert resultado.returncode == 0, resultado.stderr
    assert chamadas(log, "-I ") == [REGRA_INPUT]


def test_cadeia_aberta_nao_recebe_regra(limpo, tmp_path):
    iptables, log, _ = firewall_falso(tmp_path, "iptables", regras=CADEIA_ABERTA)
    ip6tables, log6, _ = firewall_falso(tmp_path, "ip6tables", regras=CADEIA_ABERTA)
    ambiente = ambiente_firewall(iptables, ip6tables)

    resultado = sudo(*ambiente, "bash", str(SETUP), "s1")
    assert resultado.returncode == 0, resultado.stderr
    assert chamadas(log, "-I ") == [] and chamadas(log, "-D ") == []
    assert chamadas(log6, "-I ") == [] and chamadas(log6, "-D ") == []
    assert sudo(*ambiente, "bash", str(TEARDOWN)).returncode == 0
    nada_existe()


def test_firewall_ausente_nao_impede_setup_nem_teardown(limpo, tmp_path):
    ausente = tmp_path / "sem-iptables"
    ambiente = ambiente_firewall(ausente, ausente)
    resultado = sudo(*ambiente, "bash", str(SETUP), "s1")
    assert resultado.returncode == 0, resultado.stderr
    resultado = sudo(*ambiente, "bash", str(TEARDOWN))
    assert resultado.returncode == 0, resultado.stderr
    nada_existe()


def test_rollback_remove_a_regra_de_input(limpo, tmp_path):
    iptables, log, estado = firewall_falso(tmp_path, "iptables", regras=CADEIA_FECHADA)
    ip6tables, _, _ = firewall_falso(tmp_path, "ip6tables", regras=CADEIA_ABERTA)
    sysctl = tmp_path / "sysctl"
    sysctl.write_text("#!/bin/sh\nexit 1\n")
    sysctl.chmod(0o755)

    resultado = sudo(
        "env",
        f"PATH={tmp_path}:{os.environ['PATH']}",
        f"IPTABLES={iptables}",
        f"IP6TABLES={ip6tables}",
        "bash",
        str(SETUP),
        "s1",
    )
    assert resultado.returncode != 0
    assert "não conseguiu liberar o bind não local em IPv4" in resultado.stderr
    assert chamadas(log, "-I ") == [REGRA_INPUT]
    assert chamadas(log, "-D ") == [REMOCAO_INPUT]
    assert not estado.exists()
    nada_existe()


HARNESS_SHUTDOWN = """
source "{lib}"
GATE_NAME=teste
SUDO=(sudo -n)
sudo -n ip netns exec gen sleep 300 >/dev/null 2>&1 &
ATTACK_PID=$!
sudo -n ip netns exec gen sleep 310 >/dev/null 2>&1 &
for _ in $(seq 200); do
  [[ $(sudo -n ip netns pids gen | wc -l) -ge 2 ]] && break
  sleep 0.02
done
trap gate_shutdown EXIT
exit 0
"""


def test_shutdown_mata_os_processos_de_gen_e_fecha_a_rede(rede_s1):
    resultado = subprocess.run(
        ["bash", "-c", HARNESS_SHUTDOWN.format(lib=GATE_LIB)],
        capture_output=True,
        text=True,
        timeout=TEMPO_COMANDO,
    )
    assert resultado.returncode == 0, resultado.stdout + resultado.stderr
    nada_existe()
