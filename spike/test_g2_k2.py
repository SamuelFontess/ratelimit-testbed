import http.client
import http.server
import ipaddress
import os
import re
import shutil
import socket
import subprocess
import threading
import time
from pathlib import Path

import pytest
from hypothesis import assume, given, settings, strategies as st

RAIZ = Path(__file__).resolve().parents[1]
PORTA = 18099
PORTA_REALIP = 18098
LOG_FIELDS = 8
COL_ADDR = 2
COL_SERVER = 3
COL_KEY = 8
READY_ATTEMPTS = 100
READY_STEP = 0.1
CONNECT_TIMEOUT = 0.2
LOG_WAIT_ATTEMPTS = 200
LOG_WAIT_STEP = 0.05
EXEMPLOS_HIPOTESE = 30
CASAS_REAIS = [
    "127.0.0.5",
    "127.0.10.5",
    "127.10.0.5",
    "127.200.255.1",
    "127.128.129.130",
    "127.1.2.3",
    "127.1.2.4",
    "127.2.2.3",
    "::1",
]
ENDERECOS_REALIP = ["::a", "a00::1", "fd77:255:0:a::1"]

ESCAPADO = re.compile(rb"\\x([0-9A-Fa-f]{2})")


def chave_esperada(endereco):
    return endereco[:3] if len(endereco) == 4 else endereco[:8]


def decodifica_log(campo):
    return ESCAPADO.sub(lambda m: bytes([int(m.group(1), 16)]), campo)


def requisita(origem, porta, cabecalhos=None):
    alvo = ipaddress.ip_address("127.0.0.1" if ":" not in origem else "::1")
    with socket.socket(socket.AF_INET6 if ":" in origem else socket.AF_INET) as s:
        s.bind((origem, 0))
        s.settimeout(5)
        s.connect((str(alvo), porta))
        conn = http.client.HTTPConnection(alvo.compressed, porta)
        conn.sock = s
        try:
            conn.request("GET", "/", headers=cabecalhos or {})
            resp = conn.getresponse()
            resp.read()
            status = resp.status
        finally:
            conn.close()
    return status


def conta_linhas(log):
    if not log.exists():
        return 0
    return len(log.read_bytes().splitlines())


def le_linha(log, numero):
    campos = log.read_bytes().splitlines()[numero - 1].split(b"\t")
    assert len(campos) == LOG_FIELDS
    assert campos[COL_SERVER - 1] == b"k2"
    return campos[COL_ADDR - 1], decodifica_log(campos[COL_KEY - 1])


def espera_registro(log, proc, linhas_antes):
    for _ in range(LOG_WAIT_ATTEMPTS):
        if proc.poll() is not None:
            raise RuntimeError(f"nginx morreu: {log.with_name('nginx.error.log').read_text()}")
        if conta_linhas(log) > linhas_antes:
            return le_linha(log, linhas_antes + 1)
        time.sleep(LOG_WAIT_STEP)
    raise RuntimeError("log não cresceu após a requisição")


def registra(nginx, origem, porta, cabecalhos=None):
    log, proc = nginx
    linhas_antes = conta_linhas(log)
    status = requisita(origem, porta, cabecalhos)
    return status, espera_registro(log, proc, linhas_antes)


class Alvo(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


def render_k2(tmp_path, porta, porta_alvo):
    dir_tmp = tmp_path / "k2"
    env = "\n".join([
        "LISTEN_HOST=127.0.0.1",
        "LISTEN6_HOST=::1",
        f"PORT={porta}",
        "R=50",
        "BURST=50",
        "QUEUE_MODE=nodelay",
        f"UPSTREAM_ADDR=127.0.0.1:{porta_alvo}",
    ])
    dir_env = tmp_path / "params.env"
    dir_env.write_text(env + "\n")
    subprocess.run([
        "bash", str(RAIZ / "nginx" / "render.sh"), "k2", str(dir_env), str(dir_tmp),
    ], check=True)
    return dir_tmp


def inicia_nginx(nginx, dir_tmp, porta):
    proc = subprocess.Popen([nginx, "-p", str(dir_tmp) + os.sep, "-c", str(dir_tmp / "nginx.conf")])
    log = dir_tmp / "nginx.access.log"
    for _ in range(READY_ATTEMPTS):
        if proc.poll() is not None:
            raise RuntimeError(f"nginx morreu: {log.with_name('nginx.error.log').read_text()}")
        try:
            with socket.create_connection(("127.0.0.1", porta), timeout=CONNECT_TIMEOUT):
                return proc, log
        except OSError:
            time.sleep(READY_STEP)
    raise RuntimeError("nginx não ficou pronto")


@pytest.fixture(scope="module")
def nginx_k2(tmp_path_factory):
    nginx = shutil.which(os.environ.get("NGINX", "nginx"))
    assert nginx, "nginx não encontrado no PATH"
    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Alvo)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        dir_tmp = render_k2(tmp_path_factory.mktemp("k2"), PORTA, upstream.server_address[1])
        proc, log = inicia_nginx(nginx, dir_tmp, PORTA)
        yield (log, proc)
    finally:
        proc.terminate()
        proc.wait()
        upstream.shutdown()
        upstream.server_close()


@pytest.fixture(scope="module")
def nginx_k2_realip(tmp_path_factory):
    nginx = shutil.which(os.environ.get("NGINX", "nginx"))
    assert nginx, "nginx não encontrado no PATH"
    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Alvo)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        dir_tmp = render_k2(tmp_path_factory.mktemp("k2-realip"), PORTA_REALIP, upstream.server_address[1])
        run_key = dir_tmp / "run.key.conf"
        run_key.write_text(run_key.read_text().replace(
            "server {",
            "server {\n    set_real_ip_from 127.0.0.1;\n    real_ip_header X-Real;",
            1,
        ))
        proc, log = inicia_nginx(nginx, dir_tmp, PORTA_REALIP)
        yield (log, proc)
    finally:
        proc.terminate()
        proc.wait()
        upstream.shutdown()
        upstream.server_close()


@pytest.mark.parametrize("origem", CASAS_REAIS)
def test_nginx_real_borda(nginx_k2, origem):
    status, (remote, chave) = registra(nginx_k2, origem, PORTA)
    assert status == 200
    assert remote == origem.encode()
    esperado = chave_esperada(ipaddress.ip_address(origem).packed)
    assert chave == esperado


@pytest.mark.parametrize("endereco", ENDERECOS_REALIP)
def test_ipv6_realip_borda(nginx_k2_realip, endereco):
    status, (remote, chave) = registra(nginx_k2_realip, "127.0.0.1", PORTA_REALIP, {"X-Real": endereco})
    assert status == 200
    assert remote == endereco.encode()
    esperado = chave_esperada(ipaddress.ip_address(endereco).packed)
    assert chave == esperado


def test_ramos_regex_com_dotall():
    ramos = [l.strip() for l in (RAIZ / "nginx" / "keys" / "k2.conf.tmpl").read_text().splitlines() if l.strip().startswith("~")]
    assert len(ramos) == 3
    for ramo in ramos:
        assert ramo.startswith("~(?s)")


def test_defaults_nao_vazios():
    linhas = [l.strip() for l in (RAIZ / "nginx" / "keys" / "k2.conf.tmpl").read_text().splitlines() if l.strip().startswith("default ")]
    assert linhas
    for linha in linhas:
        valor = linha.split(None, 1)[1].rstrip(";").strip()
        assert valor.strip("'\"")


@given(a=st.integers(0, 255), b=st.integers(0, 255), c=st.integers(0, 255))
@settings(max_examples=EXEMPLOS_HIPOTESE, deadline=None)
def test_hipotese_real_24(nginx_k2, a, b, c):
    origem = f"127.{a}.{b}.{c}"
    assume(origem != "127.255.255.255")
    status, (remote, chave) = registra(nginx_k2, origem, PORTA)
    assert status == 200
    assert remote == origem.encode()
    assert chave == chave_esperada(ipaddress.ip_address(origem).packed)
