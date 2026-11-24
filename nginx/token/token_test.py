import string
import time

import pytest
from hypothesis import assume, given, settings, strategies as st

import token_ref
from k3_harness import emite_token, novo_segredo, requisita, servidor_k3, usa_token

PORTA = 18097
PORTA_SEM_SEGREDO = 18096
ORIGEM = "127.0.0.1"
EXEMPLOS_PURO = 200
EXEMPLOS_NJS = 100
MARGEM_EXPIRACAO = 10
DELTA_MAXIMO = 100000
SEGREDO = novo_segredo()
VISIVEIS = [chr(c) for c in range(0x21, 0x7F)]

cids = st.from_regex(r"[A-Za-z0-9_-]{1,64}", fullmatch=True)
segredos = st.text(min_size=1, max_size=64)
instantes = st.integers(1_000_000_000, 4_000_000_000)
modos = st.sampled_from(
    [
        "intacto",
        "um_byte",
        "sem_segmento",
        "segmento_extra",
        "vazio",
        "outro_segredo",
        "cid_com_lf",
        "trunca_assinatura",
        "estende_assinatura",
    ]
)


def corrompe(token, modo, posicao, troca, segredo, cid, exp):
    if modo == "um_byte":
        indice = posicao % len(token)
        assume(token[indice] != troca)
        return token[:indice] + troca + token[indice + 1:]
    if modo == "sem_segmento":
        return ".".join(token.split(".")[:2])
    if modo == "segmento_extra":
        return token + ".x"
    if modo == "vazio":
        return ""
    if modo == "outro_segredo":
        return token_ref.emite_com_exp(segredo + "x", cid, exp)
    if modo == "cid_com_lf":
        return token_ref.emite_com_exp(segredo, cid + "\n", exp)
    if modo == "trunca_assinatura":
        cabeca, assinatura = token.rsplit(".", 1)
        return cabeca + "." + assinatura[: posicao % len(assinatura)]
    if modo == "estende_assinatura":
        return token + "A"
    return token


@given(segredo=segredos, cid=cids, agora=instantes, decorrido=st.integers(0, token_ref.TTL_SEGUNDOS - 1))
@settings(max_examples=EXEMPLOS_PURO, deadline=None)
def test_token_emitido_vale_ate_expirar(segredo, cid, agora, decorrido):
    token = token_ref.emite(segredo, cid, agora)
    assert token_ref.valida(segredo, token, agora + decorrido) == cid


@given(
    segredo=segredos,
    cid=cids,
    agora=instantes,
    atraso=st.integers(token_ref.TTL_SEGUNDOS, 10 * token_ref.TTL_SEGUNDOS),
)
@settings(max_examples=EXEMPLOS_PURO, deadline=None)
def test_token_expirado_invalida(segredo, cid, agora, atraso):
    token = token_ref.emite(segredo, cid, agora)
    assert token_ref.valida(segredo, token, agora + atraso) == ""


@given(
    segredo=segredos,
    cid=cids,
    agora=instantes,
    posicao=st.integers(0),
    troca=st.characters(),
)
@settings(max_examples=EXEMPLOS_PURO, deadline=None)
def test_alterar_um_byte_invalida(segredo, cid, agora, posicao, troca):
    token = token_ref.emite(segredo, cid, agora)
    indice = posicao % len(token)
    assume(token[indice] != troca)
    alterado = token[:indice] + troca + token[indice + 1:]
    assert token_ref.valida(segredo, alterado, agora) == ""


@given(segredo=segredos, cid=cids, agora=instantes, outro=segredos)
@settings(max_examples=EXEMPLOS_PURO, deadline=None)
def test_segredo_diferente_invalida(segredo, cid, agora, outro):
    assume(outro != segredo)
    assert token_ref.valida(outro, token_ref.emite(segredo, cid, agora), agora) == ""


@given(segredo=segredos, cid=cids, agora=instantes, partes=st.lists(st.text(), max_size=6))
@settings(max_examples=EXEMPLOS_PURO, deadline=None)
def test_estrutura_quebrada_invalida(segredo, cid, agora, partes):
    assume(len(partes) != token_ref.SEGMENTOS)
    assert token_ref.valida(segredo, ".".join(partes), agora) == ""


@pytest.fixture(scope="module")
def nginx_k3(tmp_path_factory):
    with servidor_k3(tmp_path_factory.mktemp("k3"), PORTA, SEGREDO, BURST=1000) as servidor:
        yield servidor


@pytest.mark.parametrize(
    "consulta",
    [
        "",
        "?cid=",
        "?cid=a%20b",
        "?cid=a.b",
        "?cid=" + "x" * 65,
        "?cid=%00",
        "?cid=a&cid=b",
        "?outro=abc",
        "?cid=abc%0A",
        "?cid=abc%0D%0A",
        "?cid=" + "x" * 64 + "%0A",
    ],
)
def test_emissao_rejeita_cid_ausente_ou_invalido(nginx_k3, consulta):
    status, _ = requisita(PORTA, ORIGEM, "/token" + consulta)
    assert status == 400


@pytest.mark.parametrize("cid", ["a", "Abc_-09", "x" * 64])
def test_emissao_vale_300s_e_e_aceita_em_api(nginx_k3, cid):
    antes = int(time.time())
    token = emite_token(PORTA, ORIGEM, cid)
    depois = int(time.time())
    exp = int(token_ref.de_b64url(token.split(".")[1]))
    assert antes + token_ref.TTL_SEGUNDOS <= exp <= depois + token_ref.TTL_SEGUNDOS
    assert token_ref.valida(SEGREDO, token, antes) == cid
    assert usa_token(PORTA, ORIGEM, token) == 200


ALFABETO = string.ascii_letters + string.digits + "_-"
ASSINADOS_INVALIDOS = [
    ("abc\n", "futuro"),
    ("abc\r\n", "futuro"),
    ("x" * 64 + "\n", "futuro"),
    ("x" * 65, "futuro"),
    ("a.b", "futuro"),
    ("", "futuro"),
    ("abc", "abc"),
    ("abc", ""),
    ("abc", "9999999999\n"),
    ("abc", "9" * 16),
]


@pytest.mark.parametrize("posicao", range(token_ref.SEGMENTOS))
def test_ultimo_caractere_de_cada_segmento_nao_tem_variante_aceita(nginx_k3, posicao):
    token = emite_token(PORTA, ORIGEM, "bits-ociosos")
    partes = token.split(".")
    for troca in ALFABETO:
        if troca == partes[posicao][-1]:
            continue
        variante = partes.copy()
        variante[posicao] = variante[posicao][:-1] + troca
        assert token_ref.valida(SEGREDO, ".".join(variante), time.time()) == ""
        assert usa_token(PORTA, ORIGEM, ".".join(variante)) == 401


@pytest.mark.parametrize(("cid", "exp"), ASSINADOS_INVALIDOS)
def test_token_assinado_com_cid_ou_exp_invalido_e_rejeitado(nginx_k3, cid, exp):
    exp = int(time.time()) + token_ref.TTL_SEGUNDOS if exp == "futuro" else exp
    token = token_ref.emite_com_exp(SEGREDO, cid, exp)
    assert token_ref.valida(SEGREDO, token, time.time()) == ""
    assert usa_token(PORTA, ORIGEM, token) == 401


@given(
    cid=cids,
    delta=st.integers(MARGEM_EXPIRACAO, DELTA_MAXIMO) | st.integers(-DELTA_MAXIMO, -MARGEM_EXPIRACAO),
    modo=modos,
    posicao=st.integers(0),
    troca=st.sampled_from(VISIVEIS),
)
@settings(max_examples=EXEMPLOS_NJS, deadline=None)
def test_njs_concorda_com_oraculo(nginx_k3, cid, delta, modo, posicao, troca):
    exp = int(time.time()) + delta
    token = corrompe(token_ref.emite_com_exp(SEGREDO, cid, exp), modo, posicao, troca, SEGREDO, cid, exp)
    esperado = token_ref.valida(SEGREDO, token, time.time())
    if modo != "intacto" or delta < 0:
        assert esperado == ""
    assert usa_token(PORTA, ORIGEM, token) == (200 if esperado else 401)


def test_sem_segredo_emissao_falha_com_500_e_token_nao_vale(tmp_path):
    with servidor_k3(tmp_path, PORTA_SEM_SEGREDO, None) as servidor:
        status, _ = requisita(PORTA_SEM_SEGREDO, ORIGEM, "/token?cid=abc")
        assert status == 500
        token = token_ref.emite(SEGREDO, "abc", time.time())
        assert usa_token(PORTA_SEM_SEGREDO, ORIGEM, token) == 401
        forjado = token_ref.emite("", "abc", time.time())
        assert usa_token(PORTA_SEM_SEGREDO, ORIGEM, forjado) == 401
    assert "TOKEN_SECRET ausente" in servidor.log_erro.read_text()


def test_config_renderizada_nao_contem_o_segredo(nginx_k3):
    for arquivo in nginx_k3.dir.iterdir():
        if arquivo.is_file():
            assert SEGREDO.encode() not in arquivo.read_bytes()
