import crypto from 'crypto';

const TTL_SECONDS = 300;
const OUTSIDE_ALPHABET = /[^A-Za-z0-9_-]/;
const OUTSIDE_DIGITS = /[^0-9]/;
const CID_MAX_LENGTH = 64;
const EXP_MAX_DIGITS = 15;
const TOKEN_HEADER = 'X-Token';
const SEPARATOR = '.';
const SEGMENT_COUNT = 3;
const HMAC_ALGORITHM = 'sha256';
const ENCODING = 'base64url';

function secretOrLog(r) {
    const secret = process.env.TOKEN_SECRET;
    if (!secret) {
        r.error('token: TOKEN_SECRET ausente no ambiente do nginx');
        return '';
    }
    return secret;
}

function isMadeOf(text, outside, maxLength) {
    return text.length >= 1 && text.length <= maxLength && !outside.test(text);
}

function isCid(text) {
    return isMadeOf(text, OUTSIDE_ALPHABET, CID_MAX_LENGTH);
}

function encode(text) {
    return Buffer.from(text).toString(ENCODING);
}

function decode(segment) {
    return Buffer.from(segment, ENCODING).toString();
}

function sign(secret, head) {
    return crypto.createHmac(HMAC_ALGORITHM, secret).update(head).digest(ENCODING);
}

function equalConstantTime(a, b) {
    if (a.length !== b.length) {
        return false;
    }
    let diff = 0;
    for (let i = 0; i < a.length; i++) {
        diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
    }
    return diff === 0;
}

function nowSeconds() {
    return Math.floor(Date.now() / 1000);
}

function clientKey(r) {
    const secret = secretOrLog(r);
    const token = r.headersIn[TOKEN_HEADER];
    if (!secret || !token) {
        return '';
    }
    const parts = token.split(SEPARATOR);
    if (parts.length !== SEGMENT_COUNT || !parts.every((part) => isMadeOf(part, OUTSIDE_ALPHABET, Infinity))) {
        return '';
    }
    const cidSegment = parts[0];
    const expSegment = parts[1];
    const signature = parts[2];
    if (!equalConstantTime(signature, sign(secret, cidSegment + SEPARATOR + expSegment))) {
        return '';
    }
    const cid = decode(cidSegment);
    const exp = decode(expSegment);
    if (!isCid(cid) || !isMadeOf(exp, OUTSIDE_DIGITS, EXP_MAX_DIGITS) || Number(exp) <= nowSeconds()) {
        return '';
    }
    return cid;
}

function issue(r) {
    const secret = secretOrLog(r);
    if (!secret) {
        r.return(500, 'TOKEN_SECRET ausente\n');
        return;
    }
    const cid = r.args.cid;
    if (typeof cid !== 'string' || !isCid(cid)) {
        r.return(400, 'cid ausente ou inválido\n');
        return;
    }
    const head = encode(cid) + SEPARATOR + encode(String(nowSeconds() + TTL_SECONDS));
    r.headersOut['Content-Type'] = 'text/plain';
    r.return(200, head + SEPARATOR + sign(secret, head));
}

export default { clientKey, issue };
