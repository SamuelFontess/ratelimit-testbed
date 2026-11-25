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

**`meta.json`**: `{scenario, key, cond, rate, burst, seed, queue_mode, warmup, duration, params{}, versions[], config_sha256, medium, started_at, ended_at, status: done|failed, error?, cpu_pct?, zone_states?}`.
`status: done` exige os 5 artefatos do C-NAME. `warmup` e `duration` (s, no nível do objeto) definem a janela do ataque; `cpu_pct` e `zone_states` são opcionais (ver "Métricas do consolidador").

**`calibration.env`**: `LAMBDA`, `ATTACK_RATE`, `S3_IPS`, `E`, `F_COLOC`, `RCLI`, `HOST_LAN_IP` (opcional).

**`results.csv`**: uma linha por corrida `status: done`: `run` (nome do diretório C-NAME), as chaves de `meta.json` (`scenario`, `key`, `cond`, `rate`, `burst`, `seed`, `queue_mode`) + `containment_rps_refused`, `containment_rps_upstream`, `pft`, `pft_same_ip`, `pft_same_prefix`, `pft_control`, `pft_threshold_1s`, `pft_threshold_2s`, `goodput`, `p95`, `p99`, `cpu_pct`, `zone_states`, `late_events`, `computable`.
`computable = false` quando falta o C1 pareado. Coluna sem dado fica vazia, nunca zero. Gravado por `analysis/consolidate.py <corridas> [--out results/results.csv]`.

## Métricas do consolidador

Definições vigentes desde 2026-11-25 (ver "Mudança de contrato"). `analysis/consolidate.py` é o consumidor; o Runner grava os campos de `meta.json`.

- **Janela do ataque:** `[warmup, warmup + duration)`, em segundos desde o tempo zero. Vale também para C1 e C3, que não têm ataque, para que o denominador seja o mesmo conjunto em todas as condições.
- **Sobreposição:** a transação legítima `[t_start, t_end]` conta quando `t_start < fim` e `t_end > início`. A que começa antes do ataque e só termina dentro dele foi atendida sob ataque; a que termina exatamente quando ele começa não o enfrentou; a que começa no instante final não chegou a concorrer. O legítimo grava todos os eventos; os anteriores a `warmup` ficam fora por esta regra.
- **Casamento:** `ordem` é a posição da transação na agenda do cliente, obtida ordenando os registros de cada `cid` por `t_plan`; a ordem das linhas do arquivo não importa. O par C1 ↔ Cx é (`scenario`, `seed`) e só pode haver um C1 `done` por par. `t_plan` diferente num par é agenda diferente e transação de C1 ausente em Cx é corrida incompleta: os dois casos abortam com erro, nunca viram pft.
- **Denominador:** quantidade de transações de C1 dentro da janela, pelos tempos de C1; atraso ou deslocamento em Cx não muda o conjunto. **Numerador:** quantidade dessas que falharam em Cx. `pft = numerador ÷ denominador`. Janela sem transação em C1 deixa `pft` vazio.
- **Falha de transação:** `outcome ≠ ok` do `legit.jsonl` (espera parcial, transação total, nenhum dado, 429, reset e 401 no K3) **ou** `t_end − t_start ≥ limiar`. O limiar nominal é 4 s (`pft`, `pft_same_ip`, `pft_same_prefix`, `pft_control`); `pft_threshold_1s` e `pft_threshold_2s` repetem a conta com 1 s e 2 s sobre os mesmos pares. O `outcome` do atacante nunca entra no pft.
- **Linha de C1:** pareada com ela mesma; o pft dela é a própria fração de falhas (é o número que o G9 exige ≤ 1%), e é 0 quando nenhuma transação falhou.
- **Sem C1 pareado:** `computable = false` e as colunas `pft*` vazias; o denominador nunca é estimado. Corrida `status: failed` não vira linha e não serve de C1.
- **`containment_rps_refused`:** transações do atacante com `outcome = fail_refused` (401, 429 ou falha de conexão) e `t_start` na janela, divididas por `duration`.
- **`containment_rps_upstream`:** linhas do `nginx.access.log` com `$upstream_response_time ≠ "-"`, divididas por `duration`. Cobre o log inteiro e inclui as requisições do legítimo, que o C-LOG não distingue das do atacante.
- **`goodput`:** transações legítimas da própria corrida dentro da janela que não falharam no limiar de 4 s, divididas por `duration`.
- **`p95`, `p99`:** latência (`t_end − t_start`) das transações legítimas da janela, percentil linear do numpy. Em `nodelay` só entram as aceitas (`outcome ≠ fail_refused`); a recusa fica de fora e segue contando no pft. Em `delay` entram todas. Sem transação elegível ficam vazios.
- **`late_events`:** transações legítimas da janela com `late_ms > 50` (o mesmo limite do Req 7.5). São contadas e informadas, não excluídas do pft.
- **`cpu_pct`** (opcional em `meta.json`, número): uso médio de CPU do processo do Nginx durante a janela, em % (100 = um núcleo inteiro). Ausente: coluna vazia.
- **`zone_states`** (opcional em `meta.json`): objeto `{zona: fração}` com a fração de estados ocupados no fim da janela, cada valor em `[0, 1]`. Zonas: `k1`, `k2` e, no K3, `k3_ceiling` (teto por prefixo), `k3_client` (por cliente) e `k3_emit` (emissão em `/token`). Vai para o CSV como JSON ordenado por chave. Ausente: coluna vazia.

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

## Formato do pool do atacante

`--schedule` do atacante aponta um arquivo JSON com o pool de origens e os parâmetros de tráfego. Todos os campos são opcionais; os ausentes usam o padrão e uma flag passada na linha de comando vence o arquivo.

| Campo | Tipo | Padrão | Significado |
|---|---|---|---|
| `per_24` | int | 8 | IPs por `/24` (v4) ou por `/64` (v6), com tolerância ±2 |
| `pool_size` | int | 2000 | tamanho-alvo do pool de IPs de origem |
| `client_24_count` | int | 5 | em S3, quantos `/24` de clientes (`10.66.1.0/24`…) pular; o atacante inclui `10.66.0.0/24` |
| `p_repeat` | float | 0,02 | probabilidade de repetir um IP já usado (faixa 0,002–0,06) |
| `post_ratio` | float | 0,3 | fração de requisições POST |
| `heavy_ratio` | float | 0,5 | fração de requisições para `/heavy` |
| `heavy_ms` | int | 200 | trabalho pedido em `/heavy?ms=` |
| `waveform` | enum | `steady` | `steady`, `ramp`, `pulse` ou `waves` da taxa agregada |
| `churn` | float | 0 | fração do pool rotacionada ao longo da corrida (0–1) |
| `sticky_s` | float | 0 | tempo que um IP é mantido antes de rotacionar (0 = por requisição) |
| `token_mode` | enum | `none` | `none`, `valid-reuse` ou `farm` para a chave K3 |
| `token_budget` | int | 0 | teto de chamadas a `/token` na corrida inteira |
| `token_path` | string | `/token` | rota de emissão de token |
| `user_agents` | []string | pool interno | pool de User-Agent sorteado por requisição |
| `headers` | [{name,value}] | pool interno | cabeçalhos extras sorteados por requisição |

- Prefixos por cenário: S1 e S3 sorteiam de `10.66.0.0/16`; S2a rotaciona dentro de um único `/64` de `fd12:3456:789a::/48`. O atacante nunca usa os prefixos de controle `10.67.0.0/16` / `fd12:3456:789b::/48`.
- Determinismo: dada `--seed`, o pool e a sequência (IP, método, UA, instante planejado) são fixos. Pipelining HTTP/1.1 real não existe em `net/http`; `--pipeline` apenas liga keep-alive e concentra requisições por conexão (limitação de realismo declarada no artigo).

## Flags do atacante

Toda flag abaixo também existe como campo no `--schedule` quando listada em "Formato do pool do atacante"; a flag na linha de comando vence o arquivo. As flags operacionais só existem na linha de comando.

| Flag | Campo do schedule | Padrão | Significado |
|---|---|---|---|
| `--seed` | — | 1 | semente determinística; pool e planner usam streams PCG distintos derivados dela |
| `--scenario` | — | `s1` | `s1`, `s2a` ou `s3` |
| `--target` | — | (obrigatório) | URL do Nginx do grupo; só esquema e host (loopback, veth ou `$HOST_LAN_IP`), sem caminho, query, credenciais ou fragmento |
| `--duration` | — | 60 | duração do ataque em s |
| `--warmup` | — | 0 | aquecimento em s antes da onda (>= 0) |
| `--rate` | — | 1000 | taxa média agregada em req/s |
| `--out` | — | `attacker.jsonl` | arquivo JSONL de saída |
| `--per-24` | `per_24` | 8 | IPs por `/24` (v4) ou `/64` (v6), tolerância ±2 |
| `--p-repeat` | `p_repeat` | 0,02 | probabilidade de repetir um IP já usado (0,002–0,06) |
| `--pool-size` | `pool_size` | 2000 | tamanho-alvo do pool de origens |
| `--client-24` | `client_24_count` | 5 | em S3, quantos `/24` de clientes pular (>= 0) |
| `--post-ratio` | `post_ratio` | 0,3 | fração de POST (0–1) |
| `--heavy-ratio` | `heavy_ratio` | 0,5 | fração para `/heavy` (0–1) |
| `--heavy-ms` | `heavy_ms` | 200 | trabalho pedido em `/heavy?ms=` (>= 0) |
| `--waveform` | `waveform` | `steady` | `steady`, `ramp`, `pulse` ou `waves` |
| `--churn` | `churn` | 0 | fração do pool rotacionada ao longo da corrida (0–1) |
| `--sticky` | `sticky_s` | 0 | tempo que um IP é mantido antes de rotacionar em s (>= 0) |
| `--token-mode` | `token_mode` | `none` | `none`, `valid-reuse` ou `farm` |
| `--token-budget` | `token_budget` | 0 | tokens emitidos no `farm` e teto de chamadas a `/token` (>= 1 em `farm`, <= 1000) |
| `--token-path` | `token_path` | `/token` | rota de emissão; deve começar com `/` e não conter `@`, `//`, `?` nem `#` |
| `--keepalive` | — | off | reusar conexão TCP (padrão: uma conexão nova por requisição) |
| `--pipeline` | — | off | concentra requisições por conexão; implica `--keepalive` |
| `--partial-timeout` | — | 4s | limiar de falha parcial |
| `--total-timeout` | — | 60s | limiar de falha total; também é o deadline de cada requisição (`--partial-timeout` <= `--total-timeout`) |
| `--start-at` | — | 0 | instante Unix em ms do tempo zero comum a atacante e legítimo (no máximo 1 h à frente); `t_start`/`t_end` saem em s desde ele; 0 = o zero é o fim do prefetch de tokens |

- Chave K3: em `valid-reuse` e `farm` o atacante emite tokens conforme o "Contrato do token K3" (deste arquivo, escrito pela tarefa da chave), mandando `?cid=<cid>` em `GET <token_path>` e o header `X-Token` nas requisições a `/api`. O `cid` é determinístico por semente (`atk-<seed>-<i>`, com `i` o índice do token); `valid-reuse` usa `atk-<seed>-0`. Os `N` tokens do `farm` são buscados antes do início do plano (não deslocam o `t_start`), cada um do IP `pool[i % len(pool)]`, e distribuídos pelas requisições por rodízio do índice; `/token` nunca é chamado mais de `N` vezes. A emissão usa `RoundTrip` direto (não segue redirect; status != 200 é erro) e qualquer falha de emissão aborta a corrida com erro e código de saída != 0 — nunca uma requisição sem token. Como o atacante não renova o token (validade 300 s), `--warmup + --duration` fica limitado a 270 s em `valid-reuse` e `farm`; um 429 em `/token` no prefetch aborta com a mensagem `emissão limitada: reduza --token-budget ou aumente E`.
- `--start-at` é o tempo zero comum de atacante e legítimo: `t_start`/`t_end` saem em s desde esse instante e `0` mantém o zero no fim do prefetch. O runner o define como agora + margem que cobre a execução do processo, `checkSources` e `planFromConfig` além do prefetch de tokens. Se o prefetch não terminar antes dele o atacante aborta com `prefetch de tokens excedeu a janela de --start-at`; se o instante já tiver vencido sem prefetch aborta com `--start-at já passou`; nos dois casos o código de saída é != 0. O valor é Unix em milissegundos e não pode passar de 1 h à frente.
- Origens: todo IP do pool é pré-checado como local e da família do alvo antes do disparo; erro de bind (`EADDRNOTAVAIL`, `EAFNOSUPPORT`), `EMFILE`/`ENFILE` e saturação do semáforo de requisições em voo (dimensionado a partir de `RLIMIT_NOFILE`, no máximo metade) são erros do próprio gerador: a corrida aborta com código != 0, sem virar registros de falha. Requisições em voo canceladas por `SIGINT`/`SIGTERM` são descartadas (sem registros fantasmas), os concluídos são gravados e a saída é != 0.

## Classificação de desfecho do atacante

Cada requisição que termina vira uma linha com `outcome`, decidido por `classify` em `gen/attacker/send.go`:

| Situação | `outcome` | `status` |
|---|---|---|
| erro do gerador (`EADDRNOTAVAIL`, `EAFNOSUPPORT`, `EMFILE`, `ENFILE`) | nenhum: a corrida aborta com erro e código != 0 | — |
| `ECONNREFUSED`, `ECONNRESET` ou `EPIPE`, qualquer que seja o tempo decorrido | `fail_refused` | 0 |
| qualquer outro erro (EOF, timeout, reset de stream) com tempo decorrido >= total | `fail_total` | 0 |
| qualquer outro erro com tempo decorrido >= parcial e < total | `fail_partial` | 0 |
| qualquer outro erro antes do parcial | `fail_total` | 0 |
| resposta 401 ou 429, qualquer que seja o tempo decorrido | `fail_refused` | código |
| resposta concluída antes do parcial | `ok` | código |
| resposta concluída no parcial e antes do total | `fail_partial` | código |
| resposta concluída no total ou além | `fail_total` | código |

- `3xx`, `4xx` diferentes de `401`/`429`, `405` e `5xx` contam como `ok` porque a requisição atravessou o limitador e chegou ao alvo: o `405` é só o alvo aceitar apenas `GET`, o `5xx` é o alvo respondendo e um `404` é o alvo não ter a rota; todos medem acesso concedido, não contenção. Só `401`/`429` (negado pelo limitador/chave) e as falhas de conexão/tempo medem contenção.
- A resposta preserva o `status` mesmo quando o desfecho é `fail_partial` ou `fail_total`; só erro sem resposta zera o `status`.
- O `outcome` do atacante mede acesso concedido × contido e não é comparável ao do legítimo; o `pft` usa só o do legítimo.

## Formato da agenda

`schedule.py --seed --scenario --lam --clients --dur --out` grava `{"clients":[{"cid","src","group"}],"events":[{"t","cid"}]}`; `events` ordenado por `t` (segundos desde o início da corrida) e tudo derivado só da seed. Cada cliente tem taxa de Poisson `lam / clients`. Opções: `--control-fraction` (padrão 0,1) e `--colocate-fraction` (padrão 0).

- Grupo controle: os primeiros `max(1, round(control_fraction × N))` clientes, `group=control`, em `10.67.0.0/16` (`fd12:3456:789b::/48` em `s2a`).
- `s1`: demais em `10.66.0.0/16`, `group=same_ip` (o SNAT os reduz a um IPv4).
- `s2a`: demais em `fd12:3456:789a:1::/64`, `group=same_prefix`.
- `s3`: cada cliente em um `/24` próprio (`10.66.<1+i>.1`, controle em `10.67.<i>.1`), `group=control`. Os `round(colocate_fraction × demais)` primeiros dos demais ficam no `/24` do atacante `10.66.0.0/24` (`10.66.0.<1+j>`), `group=same_prefix`. O pool do atacante em S3 deve incluir `10.66.0.0/24` e evitar os `/24` dos clientes de controle.
- IPv4 e IPv6 nunca se misturam numa corrida; o `--target` do legítimo precisa ser alcançável pela família da agenda (`LISTEN_HOST` em `s1`/`s3`, `LISTEN6_HOST` em `s2a`).

O legítimo (`legit`) ignora `--seed --scenario --duration --warmup --rate`, grava todos os eventos (o consolidador descarta o warmup lido de `meta.json`) e aceita `--partial-timeout` (padrão 4 s), `--total-timeout` (padrão 60 s), `--token-path` e `--token-concurrency`. `outcome`:

| Situação observada | `outcome` |
|---|---|
| Resposta 2xx completa dentro dos limites | `ok` |
| Timeout de conexão (dial); servidor que aceita e não envia nada, ou para de enviar, por mais que o limite parcial | `fail_partial` |
| Transação maior que o limite total; fechamento sem nenhum dado (EOF) | `fail_total` |
| Status fora de 2xx (429, 401, 500, ...); `RST`, `ECONNRESET`, `EPIPE` ou `ECONNREFUSED` | `fail_refused` |

Antes de disparar o primeiro evento o legítimo confere cada `src` da agenda: IP que o namespace não consegue usar como origem, ou família diferente da do `--target` quando ele é IP literal, aborta a corrida com erro (código 1) citando o IP, em vez de virar `fail_total`. O `--out` é criado antes da corrida, mas `legit.jsonl` só é gravado ao fim: uma corrida interrompida (SIGINT ou kill) é descartada e o runner a marca como inválida.

Token (K3, ver "Contrato do token K3"): com `--token-path` (padrão vazio = sem token; K1, K2, C1 e C4 não mudam), o token é adquirido antes do tempo zero, fora do caminho medido: para cada cliente da agenda, `GET <target><token-path>?cid=<cid decimal>` pelo mesmo bind de origem, transporte e limites do cliente, com no máximo `--token-concurrency` buscas simultâneas (padrão 16, para não formar rajada). O prefetch tem um único deadline global: o `--start-at` quando ele é diferente de zero, senão `agora + --total-timeout`; assim o atraso do prefetch nunca passa do tempo zero combinado com o atacante. Em falha (429, erro, corpo que não seja um token: vazio, com espaço ou byte de controle, ou acima de 4096 bytes) a busca é repetida com intervalo de 250 ms até esse deadline; esgotado o prazo o cliente fica sem token, o `/api` dele responde 401 e conta como falha do cliente, e os eventos atrasados aparecem em `late_ms`, sem abortar a corrida. Com `--start-at` já vencido ao fim do prefetch, a busca nem chega a ser tentada e todos os clientes ficam sem token. A emissão limitada por E afeta quem consegue token, não a latência dos eventos, e `t_start`, `t_end` e `late_ms` nunca incluem a busca. O tempo zero do legítimo é o `--start-at` quando definido (o prefetch termina antes dele e o gerador espera até ele) e, quando `--start-at` é 0, o instante em que o prefetch termina; ele nunca é fixado antes do prefetch. `t_start`/`t_end` são segundos desde esse tempo zero, e atacante e legítimo ficam sincronizados recebendo o mesmo `--start-at` (o runner passa `agora + margem` que cobre o prefetch). Os eventos enviam `X-Token: <token>` em `/api` sem buscar nada; só a renovação de um token com mais de 270 s (margem sob os 300 s de validade) ocorre durante a corrida, em segundo plano, uma por cliente, sem segurar evento nenhum (os demais eventos seguem com o token anterior) e, se falhar, não é repetida antes de `--partial-timeout`. A busca não é transação do `pft` (não gera linha em `legit.jsonl`) e a corrida registra no stderr quantas buscas falharam.

## Mudança de contrato

Qualquer alteração depois do congelamento é registrada neste arquivo, com data, e todos os consumidores são atualizados no mesmo commit.

| Data | Contrato | Mudança | Consumidores atualizados |
|---|---|---|---|
| 2026-11-24 | C-NET | Seção acrescentada: endereços do veth (v4 e v6), portas, prefixos de origem e de controle, local do SNAT | nginx (T4), rede (T7), atacante (T8), legítimo (T9) |
| 2026-11-24 | C-LOG | 8º campo passa de `$sent_http_x_key` para `$testbed_key`, definida por `set` no `location /` de cada template; `add_header X-Key` removido porque bytes binários da chave (0x00, 0x0A) não são válidos em header HTTP | nginx (T5), nginx/test_k1.sh, spike/test_g2_k2.py |
| 2026-11-24 | Token K3 | Seção acrescentada: formato do token, emissão `/token`, cabeçalho `X-Token`, segredo em `TOKEN_SECRET`; C-LOG ganha a chave do K3 (`cid`; vazia em 401) e params `E`, `RCLI`, `EMIT_KEY` | nginx (T6), atacante (T8), legítimo (T9) |
| 2026-11-24 | C-CLI (atacante) | Seção "Formato do pool do atacante" acrescentada: arquivo de `--schedule` com pool e parâmetros de tráfego | atacante (T8) |
| 2026-11-24 | C-CLI (atacante) | Seções "Flags do atacante" (todas as flags, chave K3, origens) e "Classificação de desfecho do atacante" acrescentadas | atacante (T8) |
| 2026-11-25 | C-CLI (atacante) | Flag `--start-at` (tempo zero comum), teto de `--token-budget` (1000) e janela de 270 s do token registrados na seção "Flags do atacante"; `--total-timeout` deixa de citar `ResponseHeaderTimeout` (o deadline do ctx cobre) | atacante (T8), legítimo (T9) |
| 2026-11-25 | C-CLI (atacante) | Tabela "Classificação de desfecho do atacante" alinhada ao `classify` de `send.go`; declarado que o `outcome` do atacante não é comparável ao do legítimo | atacante (T8), legítimo (T9), análise |
| 2026-11-24 | Formato da agenda | Seção acrescentada: JSON da agenda do legítimo, regra de origem e grupo por cenário, flags extras `--partial-timeout`/`--total-timeout` e classificação de `outcome` do legítimo | legítimo (T9), consolidador, runner |
| 2026-11-24 | Formato da agenda | `outcome` do legítimo fixado por situação (dial/servidor mudo = `fail_partial`, EOF sem dados = `fail_total`, `RST`/`EPIPE`/`ECONNRESET`/`ECONNREFUSED` = `fail_refused`); verificação de `src` antes da corrida; flags `--token-path`, `--token-concurrency` e `--token-deadline` (token adquirido antes do tempo zero, `X-Token` em `/api`, renovação aos 270 s), referenciando o contrato do token K3; corrida interrompida descartada | legítimo (T9), consolidador, runner |
| 2026-11-25 | Formato da agenda (legítimo) | `--token-deadline` por cliente substituído por um deadline global do prefetch (o `--start-at`, ou `agora + --total-timeout` quando ele é 0), mantendo `--token-concurrency`; corpo de token limitado a 4096 bytes; `--start-at` já vencido deixa os clientes sem token e o atraso sai em `late_ms` | legítimo (T9), runner, consolidador |
| 2026-11-25 | `meta.json`, `results.csv` | Seção "Métricas do consolidador" acrescentada: janela `[warmup, warmup + duration)`, regra de sobreposição, casamento por (`scenario`, `seed`) e (`cid`, ordem), falha por `outcome` ou limiar, contenção, goodput, p95/p99, `late_events`; `meta.json` passa a exigir `warmup` e `duration` e aceita `cpu_pct` e `zone_states`; `results.csv` ganha `run`, `pft_threshold_1s` e `pft_threshold_2s` | consolidador (`analysis/consolidate.py`), runner (a gravar os campos) |
