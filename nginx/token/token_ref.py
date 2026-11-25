import base64
import hashlib
import hmac
import re

TTL_SEGUNDOS = 300
SEGMENTO = re.compile(r"[A-Za-z0-9_-]+")
CID = re.compile(r"[A-Za-z0-9_-]{1,64}")
EXPIRACAO = re.compile(r"[0-9]{1,15}")
SEGMENTOS = 3


def b64url(dados):
    return base64.urlsafe_b64encode(dados).rstrip(b"=").decode()


def de_b64url(segmento):
    return base64.urlsafe_b64decode(segmento + "=" * (-len(segmento) % 4)).decode()


def assina(segredo, cabeca):
    return b64url(hmac.new(segredo.encode(), cabeca.encode(), hashlib.sha256).digest())


def emite_com_exp(segredo, cid, exp):
    cabeca = b64url(cid.encode()) + "." + b64url(str(exp).encode())
    return cabeca + "." + assina(segredo, cabeca)


def emite(segredo, cid, agora):
    return emite_com_exp(segredo, cid, int(agora) + TTL_SEGUNDOS)


def valida(segredo, token, agora):
    partes = token.split(".")
    if len(partes) != SEGMENTOS or not all(SEGMENTO.fullmatch(p) for p in partes):
        return ""
    if not hmac.compare_digest(partes[2], assina(segredo, partes[0] + "." + partes[1])):
        return ""
    cid = de_b64url(partes[0])
    exp = de_b64url(partes[1])
    if not CID.fullmatch(cid) or not EXPIRACAO.fullmatch(exp) or int(exp) <= int(agora):
        return ""
    return cid
