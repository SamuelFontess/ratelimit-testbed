# Contratos congelados do testbed

Fonte única de verdade para as tasks seguintes. Mudanças só pela seção "Mudança de contrato".

## C-LOG: `log_format testbed`

TSV com exatamente 8 campos, nesta ordem:

```
$msec \t $remote_addr \t $server_name \t $status \t $request_time \t $upstream_response_time \t $limit_req_status \t $testbed_key
```

- `$server_name` identifica a chave (`k1`/`k2`/`k3`).
- `$testbed_key` é a chave efetiva calculada, definida por `set` no `location /` de cada template de chave (o eco via header `X-Key` foi removido: bytes binários da chave não cabem em um header HTTP).
- `$limit_req_status` é `PASSED|DELAYED|REJECTED` (variável do módulo).
- `$testbed_key` fica vazio em 400 (requisição malformada que não chega a `location /`) e em 401 do K3 (sem chave efetiva: o token não identifica cliente).
- No K3, `$testbed_key` é o `cid` em `/api` (aceita ou rejeitada pelos limites) e a chave de emissão (prefixo K2 ou `$remote_addr`, conforme `EMIT_KEY`) em `/token`.

## C-CLI: geradores

Flags idênticas em atacante e legítimo:

```
--seed <int> --scenario <s1|s2a|s3> --target <url> --duration <s> --warmup <s>
--rate <reqps>            # atacante: taxa agregada; legítimo: ignorado (usa agenda)
--schedule <path>         # legítimo: agenda pré-computada; atacante: pool de IPs/params
--out <path.jsonl>        # uma linha JSON por transação: {t_start,t_end,status,outcome}
--start-at <unix_ms>      # instante absoluto do tempo zero; 0 = o gerador define o zero ao terminar o prefetch de tokens
```

- `outcome ∈ {ok, fail_partial, fail_total, fail_refused}`.
- `--start-at` é o tempo zero comum de atacante e legítimo: `t_start`/`t_end` são segundos desde esse instante. O runner o define como agora + margem que cobre o prefetch de tokens (K3). Se o prefetch não terminar antes dele, o atacante aborta com exit != 0; o legítimo segue com os clientes sem token (401 conta falha) e o atraso aparece em `late_ms`.
- O `outcome` do atacante mede acesso concedido × contido e não é comparável ao do legítimo; o pft usa só o do legítimo.
- Saída determinística dada a seed.

## C-NAME: diretório de uma corrida

```
<scenario>__<key>__<cond>__r<rate>__burst<b>__seed<seed>__<delay|nodelay>/
  nginx.access.log  nginx.error.log  attacker.jsonl  legit.jsonl  meta.json
```

- `cond ∈ {c1,c2,c3,c4}`; `key ∈ {k1,k2,k3,none}` (`none` em c1/c2).
- `meta.json` traz versões, params, checksum das configs e status.

## Modelo de dados

**`attacker.jsonl` / `legit.jsonl`**: uma linha JSON por transação.

| Campo | Tipo | Regra |
|---|---|---|
| `t_start`, `t_end` | float (s desde o início da corrida) | `t_end ≥ t_start` |
| `status` | int | código HTTP ou 0 se não houve resposta |
| `outcome` | enum | `ok`, `fail_partial`, `fail_total`, `fail_refused` |
| `cid` | int | só no legítimo |
| `group` | enum | `same_ip`, `same_prefix`, `control` (só no legítimo) |
| `t_plan`, `late_ms` | float | só no legítimo |

**`meta.json`**: `{scenario, key, cond, rate, burst, seed, queue_mode, params{}, versions[], config_sha256, medium, started_at, ended_at, status: done|failed, error?}`.
`status: done` exige os 5 artefatos do C-NAME.

**`calibration.env`**: `LAMBDA`, `ATTACK_RATE`, `S3_IPS`, `E`, `F_COLOC`, `RCLI`, `HOST_LAN_IP` (opcional).

**`results.csv`**: uma linha por corrida: chaves de `meta.json` + `containment_rps_refused`, `containment_rps_upstream`, `pft`, `pft_same_ip`, `pft_same_prefix`, `pft_control`, `goodput`, `p95`, `p99`, `cpu_pct`, `zone_states`, `late_events`, `computable`.
`computable = false` quando falta o C1 pareado.

## C-NET: rede sintética (valores fixos)

- Namespace `gen`; veth `veth-h` (namespace raiz) ↔ `veth-g` (namespace `gen`).
- IPv4 do veth: `10.77.255.1/30` (raiz) e `10.77.255.2/30` (`gen`). IPv6 do veth: `fd77:255::1/126` (raiz) e `fd77:255::2/126` (`gen`).
- Nginx escuta em `LISTEN_HOST=10.77.255.1` e, para origens IPv6, em `LISTEN6_HOST=fd77:255::1`, nas portas `8081` (k1), `8082` (k2) e `8083` (k3). Testes automatizados usam portas ≥ 18000.
- Alvo em `127.0.0.1:8080` (upstream do Nginx; não é alcançável de `gen`).
- Prefixos de origem, com rota local e `IP_FREEBIND` em `gen`: atacante e legítimos sob ataque `10.66.0.0/16` e `fd12:3456:789a::/48`; Grupo_Controle `10.67.0.0/16` e `fd12:3456:789b::/48`, nunca usados pelo atacante.
- Em S1, o SNAT acontece no `postrouting` do namespace `gen` (saída por `veth-g`) e reescreve só `10.66.0.0/16` para `10.77.255.2`; `10.67.0.0/16` não sofre SNAT. Tabela `nft` dedicada `testbed_nat`, nunca as tabelas do sistema (`ip nat` já existe e pertence a outro serviço).
- Nenhum prefixo da rede sintética pode sobrepor `10.0.0.0/24` (LAN real), `100.64.0.0/10` (Tailscale), `26.0.0.0/8` (Radmin) ou `192.168.56.0/24` (VirtualBox).

## Contrato do token K3

- Formato: `base64url(cid).base64url(exp).base64url(HMAC-SHA256(secret, base64url(cid) + "." + base64url(exp)))`, sem preenchimento `=`; `exp` é o texto decimal (ASCII) da época em segundos, igual à emissão + 300.
- Emissão: `GET /token?cid=<cid>` com `cid` casando `[A-Za-z0-9_-]{1,64}`; ausente ou inválido responde 400; válido responde 200 com o token como corpo `text/plain`, sem quebra de linha. É um content handler njs (`js_content`) sob `limit_req` de zona própria: chave = prefixo (K2) por padrão ou endereço quando `EMIT_KEY=addr`, taxa `${E}`, burst `${BURST}`.
- Uso: cabeçalho `X-Token: <token>` em `/api`. Ausente, de estrutura inválida, com HMAC inválido ou expirado responde 401 antes de qualquer `limit_req` (nunca `auth_request`).
- O segredo vem da variável de ambiente `TOKEN_SECRET` do Runner (diretiva `env TOKEN_SECRET;` no contexto main); nunca em arquivo versionado. Sem segredo, `/token` responde 500 e `/api` responde 401, e o njs registra o erro no `error.log`.
- Ordem dos limites em `/api`: teto por prefixo (`$k2key_safe`, `${R}`, `${BURST}`, idênticos aos do K2), depois por cliente (`cid`, `${RCLI}`, `${BURST}`). Os maps do prefixo vêm do mesmo arquivo `nginx/maps/k2_prefix.conf` no K2 e no K3.
- Um 401 não passa por nenhuma zona, e uma recusa 429 de uma zona não consome a cota da outra: teto por prefixo e limite por cliente não se acumulam (medido no nginx 1.28.3).

## Mudança de contrato

Qualquer alteração depois do congelamento é registrada neste arquivo, com data, e todos os consumidores são atualizados no mesmo commit.

| Data | Contrato | Mudança | Consumidores atualizados |
|---|---|---|---|
| 2026-11-24 | C-NET | Seção acrescentada: endereços do veth (v4 e v6), portas, prefixos de origem e de controle, local do SNAT | nginx (T4), rede (T7), atacante (T8), legítimo (T9) |
| 2026-11-24 | C-LOG | 8º campo passa de `$sent_http_x_key` para `$testbed_key`, definida por `set` no `location /` de cada template; `add_header X-Key` removido porque bytes binários da chave (0x00, 0x0A) não são válidos em header HTTP | nginx (T5), nginx/test_k1.sh, spike/test_g2_k2.py |
| 2026-11-24 | Token K3 | Seção acrescentada: formato do token, emissão `/token`, cabeçalho `X-Token`, segredo em `TOKEN_SECRET`; C-LOG ganha a chave do K3 (`cid`; vazia em 401) e params `E`, `RCLI`, `EMIT_KEY` | nginx (T6), atacante (T8), legítimo (T9) |
