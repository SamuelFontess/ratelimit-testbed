import contextlib
import http.client
import http.server
import math
import os
import re
import secrets
import shutil
import signal
import socket
import subprocess
import threading
import time
from pathlib import Path

RAIZ = Path(__file__).resolve().parents[2]
HOST = "127.0.0.1"
READY_ATTEMPTS = 100
READY_STEP = 0.1
CONNECT_TIMEOUT = 0.2
REQUEST_TIMEOUT = 5
LOG_FIELDS = 8
COL_SERVER = 3
COL_STATUS = 4
COL_LIMIT = 7
COL_KEY = 8
PARAMS_PADRAO = {
    "LISTEN_HOST": HOST,
    "LISTEN6_HOST": "",
    "QUEUE_MODE": "nodelay",
    "R": 100000,
    "BURST": 5,
    "RCLI": 100000,
    "E": 100000,
}
ESCAPADO = re.compile(rb"\\x([0-9A-Fa-f]{2})")


def novo_segredo():
    return secrets.token_hex(32)


def decodifica_log(campo):
    return ESCAPADO.sub(lambda m: bytes([int(m.group(1), 16)]), campo)


def limite_superior(burst, taxa, decorrido):
    return burst + 1 + math.ceil(decorrido * taxa)


class Alvo(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


class Servidor:
    def __init__(self, dir_tmp, proc):
        self.dir = dir_tmp
        self.proc = proc
        self.log = dir_tmp / "nginx.access.log"
        self.log_erro = dir_tmp / "nginx.error.log"

    def encerra(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGQUIT)
        self.proc.wait()

    def linhas_log(self):
        return [l.split(b"\t") for l in self.log.read_bytes().splitlines()]


def renderiza(tmp_path, chave, porta, porta_alvo, params):
    valores = {**PARAMS_PADRAO, **params, "PORT": porta, "UPSTREAM_ADDR": f"{HOST}:{porta_alvo}"}
    arquivo_env = tmp_path / "params.env"
    arquivo_env.write_text("".join(f"{k}={v}\n" for k, v in valores.items()))
    dir_tmp = tmp_path / chave
    subprocess.run(["bash", str(RAIZ / "nginx" / "render.sh"), chave, str(arquivo_env), str(dir_tmp)], check=True)
    return dir_tmp


def espera_pronto(proc, porta, log_erro):
    for _ in range(READY_ATTEMPTS):
        if proc.poll() is not None:
            raise RuntimeError(f"nginx morreu: {log_erro.read_text()}")
        try:
            with socket.create_connection((HOST, porta), timeout=CONNECT_TIMEOUT):
                return
        except OSError:
            time.sleep(READY_STEP)
    raise RuntimeError("nginx não ficou pronto")


@contextlib.contextmanager
def servidor_k3(tmp_path, porta, segredo, **params):
    nginx = shutil.which(os.environ.get("NGINX", "nginx"))
    assert nginx, "nginx não encontrado no PATH"
    ambiente = {k: v for k, v in os.environ.items() if k != "TOKEN_SECRET"}
    if segredo is not None:
        ambiente["TOKEN_SECRET"] = segredo
    upstream = http.server.ThreadingHTTPServer((HOST, 0), Alvo)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    servidor = None
    try:
        dir_tmp = renderiza(tmp_path, "k3", porta, upstream.server_address[1], params)
        proc = subprocess.Popen(
            [nginx, "-p", str(dir_tmp) + os.sep, "-c", str(dir_tmp / "nginx.conf"), "-e", str(dir_tmp / "nginx.error.log")],
            env=ambiente,
        )
        servidor = Servidor(dir_tmp, proc)
        espera_pronto(proc, porta, servidor.log_erro)
        yield servidor
    finally:
        if servidor:
            servidor.encerra()
        upstream.shutdown()
        upstream.server_close()


def requisita(porta, origem, caminho, cabecalhos=None):
    with socket.socket() as s:
        s.bind((origem, 0))
        s.settimeout(REQUEST_TIMEOUT)
        s.connect((HOST, porta))
        conn = http.client.HTTPConnection(HOST, porta)
        conn.sock = s
        try:
            conn.request("GET", caminho, headers=cabecalhos or {})
            resp = conn.getresponse()
            return resp.status, resp.read().decode()
        finally:
            conn.close()


def emite_token(porta, origem, cid):
    status, corpo = requisita(porta, origem, f"/token?cid={cid}")
    assert status == 200, f"/token?cid={cid} respondeu {status}"
    return corpo


def usa_token(porta, origem, token):
    return requisita(porta, origem, "/api", {"X-Token": token})[0]


def inunda(chamadas):
    inicio = time.monotonic()
    statuses = [chamada() for chamada in chamadas]
    return statuses, time.monotonic() - inicio
