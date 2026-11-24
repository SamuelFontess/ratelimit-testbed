# Contratos congelados do testbed

Fonte única de verdade para as tasks seguintes. Mudanças só pela seção "Mudança de contrato".

## C-LOG: `log_format testbed`

TSV com exatamente 8 campos, nesta ordem:

```
$msec \t $remote_addr \t $server_name \t $status \t $request_time \t $upstream_response_time \t $limit_req_status \t $sent_http_x_key
```

- `$server_name` identifica a chave (`k1`/`k2`/`k3`).
- `$sent_http_x_key` é a chave efetiva calculada (eco via `add_header X-Key`).
- `$limit_req_status` é `PASSED|DELAYED|REJECTED` (variável do módulo).

## C-CLI: geradores

Flags idênticas em atacante e legítimo:

```
--seed <int> --scenario <s1|s2a|s3> --target <url> --duration <s> --warmup <s>
--rate <reqps>            # atacante: taxa agregada; legítimo: ignorado (usa agenda)
--schedule <path>         # legítimo: agenda pré-computada; atacante: pool de IPs/params
--out <path.jsonl>        # uma linha JSON por transação: {t_start,t_end,status,outcome}
```

- `outcome ∈ {ok, fail_partial, fail_total, fail_refused}`.
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

## Mudança de contrato

Qualquer alteração depois do congelamento é registrada neste arquivo, com data, e todos os consumidores são atualizados no mesmo commit.

| Data | Contrato | Mudança | Consumidores atualizados |
|---|---|---|---|
| (nenhuma) | | | |
