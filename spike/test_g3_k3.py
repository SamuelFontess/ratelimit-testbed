import os
import re
import time
from pathlib import Path

import pytest

import token_ref
from k3_harness import (
    COL_KEY, COL_LIMIT, COL_SERVER, COL_STATUS, LOG_FIELDS, RAIZ,
    decodifica_log, emite_token, inunda, limite_superior, novo_segredo, requisita,
    servidor_k3, usa_token,
)

PORTA_CLIENTE = 18090
PORTA_TETO = 18091
PORTA_EMISSAO_PREFIXO = 18092
PORTA_EMISSAO_ENDERECO = 18093
PORTA_LOG = 18094
PORTA_FLOOD = 18095
FLOOD = 30
BURST = 5
TAXA_BAIXA = 1
ENVIOS = 4 * (BURST + 1)
SEGREDO = novo_segredo()
ORIGEM = "127.0.0.10"
ARQUIVOS_VARREDURA = {".js", ".py", ".sh", ".tmpl", ".conf", ".env", ".md", ".ini"}
DIRETORIOS_IGNORADOS = {".git", ".venv", "__pycache__", "runs", "results"}
ATRIBUICAO_LITERAL = re.compile(
    r"""^\s*(?:export\s+)?TOKEN_SECRET=[A-Za-z0-9]|TOKEN_SECRET["']?\]?\s*[:=]\s*["'][^"'\s]"""
)


def fixture_k3(porta, **params):
    @pytest.fixture(scope="module")
    def fixture(tmp_path_factory):
        with servidor_k3(tmp_path_factory.mktemp(f"k3-{porta}"), porta, SEGREDO, BURST=BURST, **params) as servidor:
            yield servidor
    return fixture


k3_cliente = fixture_k3(PORTA_CLIENTE, RCLI=TAXA_BAIXA)
k3_teto = fixture_k3(PORTA_TETO, R=TAXA_BAIXA)
k3_emissao_prefixo = fixture_k3(PORTA_EMISSAO_PREFIXO, E=TAXA_BAIXA, EMIT_KEY="prefix")
k3_emissao_endereco = fixture_k3(PORTA_EMISSAO_ENDERECO, E=TAXA_BAIXA, EMIT_KEY="addr")


def uso(porta, origem, token):
    return lambda: usa_token(porta, origem, token)


def aceitos(statuses):
    return statuses.count(200)


def assert_contido(statuses, decorrido, taxa):
    assert set(statuses) <= {200, 429}
    assert BURST + 1 <= aceitos(statuses) <= limite_superior(BURST, taxa, decorrido)
    assert 429 in statuses


def limitada_apos(status, decorrido, taxa):
    return status == 429 or decorrido * taxa >= 1


def test_sem_token_401(k3_cliente):
    assert requisita(PORTA_CLIENTE, ORIGEM, "/api")[0] == 401
    assert requisita(PORTA_CLIENTE, ORIGEM, "/api", {"X-Token": ""})[0] == 401


def test_token_adulterado_401(k3_cliente):
    token = emite_token(PORTA_CLIENTE, ORIGEM, "adulterado")
    cabeca, assinatura = token.rsplit(".", 1)
    troca = "A" if assinatura[0] != "A" else "B"
    outro_segredo = token_ref.emite(SEGREDO + "x", "adulterado", time.time())
    for adulterado in (cabeca + "." + troca + assinatura[1:], outro_segredo, "x" + token):
        assert usa_token(PORTA_CLIENTE, ORIGEM, adulterado) == 401


def test_token_expirado_401(k3_cliente):
    expirado = token_ref.emite_com_exp(SEGREDO, "expirado", int(time.time()) - 1)
    assert usa_token(PORTA_CLIENTE, ORIGEM, expirado) == 401


def test_valido_limitado_por_cliente_sem_afetar_outro_cliente(k3_cliente):
    token = emite_token(PORTA_CLIENTE, ORIGEM, "cliente-a")
    statuses, decorrido = inunda([uso(PORTA_CLIENTE, ORIGEM, token)] * ENVIOS)
    assert_contido(statuses, decorrido, TAXA_BAIXA)
    outro = emite_token(PORTA_CLIENTE, ORIGEM, "cliente-b")
    assert usa_token(PORTA_CLIENTE, ORIGEM, outro) == 200


def test_token_reutilizado_em_prefixos_distintos_contido_por_cliente(k3_cliente):
    token = emite_token(PORTA_CLIENTE, ORIGEM, "reuso-prefixos")
    origens = [f"127.1.{i}.1" for i in range(ENVIOS)]
    statuses, decorrido = inunda([uso(PORTA_CLIENTE, o, token) for o in origens])
    assert_contido(statuses, decorrido, TAXA_BAIXA)


def test_token_reutilizado_em_n_enderecos_contido_pelo_teto_do_prefixo(k3_teto):
    token = emite_token(PORTA_TETO, ORIGEM, "atacante")
    origens = [f"127.2.3.{i}" for i in range(1, ENVIOS + 1)]
    statuses, decorrido = inunda([uso(PORTA_TETO, o, token) for o in origens])
    assert_contido(statuses, decorrido, TAXA_BAIXA)
    legitimo = emite_token(PORTA_TETO, "127.2.4.1", "legitimo")
    assert usa_token(PORTA_TETO, "127.2.4.1", legitimo) == 200


def test_emissao_limitada_por_prefixo(k3_emissao_prefixo):
    chamadas = [lambda: requisita(PORTA_EMISSAO_PREFIXO, "127.3.0.1", "/token?cid=emissao")[0]] * ENVIOS
    statuses, decorrido = inunda(chamadas)
    assert_contido(statuses, decorrido, TAXA_BAIXA)
    vizinho = requisita(PORTA_EMISSAO_PREFIXO, "127.3.0.2", "/token?cid=emissao")[0]
    assert limitada_apos(vizinho, decorrido, TAXA_BAIXA)
    assert requisita(PORTA_EMISSAO_PREFIXO, "127.3.1.1", "/token?cid=emissao")[0] == 200


def test_emissao_por_endereco_separa_origens_do_mesmo_prefixo(k3_emissao_endereco):
    chamadas = [lambda: requisita(PORTA_EMISSAO_ENDERECO, "127.4.0.1", "/token?cid=emissao")[0]] * ENVIOS
    statuses, decorrido = inunda(chamadas)
    assert_contido(statuses, decorrido, TAXA_BAIXA)
    assert requisita(PORTA_EMISSAO_ENDERECO, "127.4.0.2", "/token?cid=emissao")[0] == 200


def test_log_registra_cid_em_200_e_429_e_vazio_em_401(tmp_path):
    cid = "cliente-log"
    with servidor_k3(tmp_path, PORTA_LOG, SEGREDO, BURST=BURST, RCLI=TAXA_BAIXA) as servidor:
        token = emite_token(PORTA_LOG, ORIGEM, cid)
        sem_token = requisita(PORTA_LOG, ORIGEM, "/api")[0]
        statuses, _ = inunda([uso(PORTA_LOG, ORIGEM, token)] * ENVIOS)
    assert sem_token == 401
    linhas = servidor.linhas_log()
    assert len(linhas) == 2 + ENVIOS
    assert all(len(l) == LOG_FIELDS and l[COL_SERVER - 1] == b"k3" for l in linhas)
    emissao, sem_token_linha, *api = linhas
    assert decodifica_log(emissao[COL_KEY - 1]) == bytes([127, 0, 0])
    assert (sem_token_linha[COL_STATUS - 1], sem_token_linha[COL_KEY - 1]) == (b"401", b"")
    assert [int(l[COL_STATUS - 1]) for l in api] == statuses
    assert {l[COL_KEY - 1] for l in api} == {cid.encode()}
    por_status = {int(l[COL_STATUS - 1]): l[COL_LIMIT - 1] for l in api}
    assert por_status[429] == b"REJECTED"
    assert por_status[200] == b"PASSED"


def test_flood_sem_token_ou_invalido_so_401_e_nao_consome_cota(tmp_path):
    with servidor_k3(tmp_path, PORTA_FLOOD, SEGREDO, BURST=BURST, R=TAXA_BAIXA) as servidor:
        valido = emite_token(PORTA_FLOOD, ORIGEM, "legitimo-flood")
        invalido = token_ref.emite(SEGREDO + "x", "invalido", time.time())
        sem_token = inunda([lambda: requisita(PORTA_FLOOD, ORIGEM, "/api")[0]] * FLOOD)[0]
        com_invalido = inunda([uso(PORTA_FLOOD, ORIGEM, invalido)] * FLOOD)[0]
        depois = usa_token(PORTA_FLOOD, ORIGEM, valido)
    assert set(sem_token + com_invalido) == {401}
    assert depois == 200
    rejeitadas = [l for l in servidor.linhas_log() if l[COL_STATUS - 1] == b"401"]
    assert len(rejeitadas) == 2 * FLOOD
    assert {l[COL_LIMIT - 1] for l in rejeitadas} == {b"-"}


def test_config_renderizada_sem_auth_request_e_com_ordem_dos_limites(k3_cliente):
    textos = [a.read_text() for a in k3_cliente.dir.iterdir() if a.suffix in {".conf"}]
    assert not any("auth_request" in t for t in textos)
    api = (k3_cliente.dir / "run.key.conf").read_text().split("location / {")[1]
    posicoes = [api.index(t) for t in ("return 401", "zone=k3_ceiling", "zone=k3_client")]
    assert posicoes == sorted(posicoes)


def test_nenhum_segredo_literal_versionado(k3_cliente):
    achados = []
    for pasta, subpastas, arquivos in os.walk(RAIZ):
        subpastas[:] = [d for d in subpastas if d not in DIRETORIOS_IGNORADOS]
        for nome in arquivos:
            caminho = Path(pasta) / nome
            if caminho.suffix in ARQUIVOS_VARREDURA and not caminho.samefile(__file__):
                achados += [str(caminho) for l in caminho.read_text(encoding="utf-8").splitlines() if ATRIBUICAO_LITERAL.search(l)]
    assert achados == []
    for arquivo in k3_cliente.dir.iterdir():
        if arquivo.is_file():
            assert SEGREDO.encode() not in arquivo.read_bytes()
