# Unidade III — Guia de validade do experimento

Para explicar **como sabemos que um teste é legítimo**. Fonte dos números: a spec do experimento (v2.1, fora deste repositório) e Mirkovic et al. (2009), IEEE TDSC 6(2). Valores marcados **[G9]** só existem depois da calibração.

A legitimidade tem três camadas. Se uma falha, o resultado não vale.

## Camada A — o ambiente é confiável? (gates do spike)

Nada é medido antes de G1 a G10 passarem (`testbed/spike/REPORT.md`).

| Gate | Condição para passar | O que protege se falhar |
|---|---|---|
| G1 | ~8 mil conexões novas/s por 60 s em S1: Nginx vê 1 IP estável, sem `EADDRNOTAVAIL`, sem descarte de conntrack | S1 deixaria de representar CGNAT, ou perderia conexões e culparia o limitador |
| G2 | K2 gera chave de 3 B (IPv4) e 8 B (IPv6), nunca vazia; IPv6 de 16 B não cai no ramo `/24`; bytes `0x0A` e `0x00`, `::1` e IPv6 comprimido ok | Chave vazia não é limitada: K2 passaria como defesa sem limitar nada |
| G3 | Sem token e token adulterado dão 401; `/token` é limitado à taxa `E`; 1 token reusado em N IPs é contido pelo teto | K3 pareceria melhor por ser forjável ou ter emissão ilimitada |
| G4 | Agenda de chegadas idêntica em C1 e C4 (mesma seed), com casamento transação a transação | O denominador do pft deixaria de valer |
| G5 | ≥ 1.000 IPs IPv4 e ≥ 1.000 IPv6 distintos no `access.log` | O atacante não estaria rotacionando de fato |
| G6 | Expulsão de zona observável (sentinela) e CPU do host não satura | Perda por saturação do host seria confundida com efeito da chave |
| G7 | O Nginx registra o IP real de aparelho físico em 8081, 8082 e 8083 | A camada de realismo não seria real |
| G8 | Projetor em navegador local (não Puffin) aparece com o IP dele e sustenta 40 `fetch` | Sonda do projetor inválida |
| G9 | Em S1, C1 com pft ≤ 1% e C2 com pft ≥ 50% | Se o ataque não machuca, não há o que defender |
| G10 | Os 7 testes de aceitação de `pesquisa/unidade3/nginx-rate-limit.md` | Comportamento do Nginx diferente do documentado |
| G11 | `cloudflared` saudável, `CF-Connecting-IP` restaurado (stretch, Fase B) | Fase B inválida |

Se o IPv6 sintético falhar (G5 ou G2 em v6), S2 vira modelo analítico, sem cabeçalho falso.

## Camada B — a medição está certa?

| Parâmetro | Valor | Origem | O que garante |
|---|---|---|---|
| pft | `1 − sucesso ÷ transações que teriam sido iniciadas sem ataque`; só transações sobrepostas à janela `[warmup, warmup + duration)`; denominador e conjunto vêm do C1 da mesma seed (regra em `CONTRACTS.md`, "Métricas do consolidador") | Mirkovic 2009, §III-C | Cliente que espera a resposta não é favorecido por fazer menos transações |
| Falha de transação | espera parcial > 4 s, transação > 60 s, nenhum dado | Mirkovic 2009, Tabela I | Referência citável |
| 429 conta como falha | sim | **extensão nossa** (Mirkovic não conta) | Recusar legítimo é o dano que medimos |
| Limiar rígido | sensibilidade com 1–2 s | nossa | Mostra se a ordem das chaves depende do limiar |
| Legítimo open-loop | dispara no instante da agenda sem esperar a resposta | exigência do pft | Evita encolher o tráfego legítimo sob atraso |
| `late_ms` > 50 ms | registrado por transação e contado em `late_events` | nossa | Detecta o gerador falhando em cumprir a agenda |
| Reiniciar o Nginx a cada corrida | sim | spec §5: estado sobrevive a `reload` | Nenhum contador herdado |
| `nodelay` | sim; 1 ponto de S1 com `delay` | cenário API | Recusa vira falha explícita |
| Uma chave por corrida | sim | design | CPU e memória por chave não se misturam |
| Seed | toda aleatoriedade vem de seed gravada | spec §11 | Reprodutibilidade |

## Camada C — a comparação é justa?

| Parâmetro | Valor | Por quê |
|---|---|---|
| Quatro condições | C1 sem limitador/sem ataque; C2 sem limitador/com ataque; C3 limitador/sem ataque; C4 limitador/com ataque | C1 dá o denominador; C2 prova dano; C3 mede o piso do dano colateral |
| Taxa `r` | 1×, 2× e 4× λ, `burst = r × 1 s`; varrida só na chave que diferencia cada cenário | Qualquer chave "ganha" num `r` certo; comparamos curvas de Pareto |
| Teto do K3 | idêntico ao do K2 | Isola o valor do token |
| Orçamento do atacante | fixo entre chaves (`--token-budget`) | O ataque é o mesmo para todas |
| Grupo controle | legítimos fora dos prefixos do ataque, em todo cenário | Separa dano colateral de dano geral |
| Seeds | 5, pareadas por seed | Mesma agenda para todas as chaves |
| Intervalo de confiança | 95%, diferença pareada por seed | Se cruza 0, não declaramos diferença |

## Valores ainda a definir (calibração G9)

`LAMBDA` (taxa legítima), `ATTACK_RATE` (taxa agregada de ataque), `E` (emissão de token), `F_COLOC` (fração de co-localização), `S3_IPS` (proposta ~2.000 IPs em ~250 `/24`), `RCLI`. Gravados em `testbed/calibration.env` e citados no artigo.

## O que o artigo precisa declarar

- O 429 como falha é extensão nossa; Mirkovic mede por requisitos de QoS.
- Os limiares de 4 s e 60 s são de navegação humana em 2007; uma API tolera menos (daí a sensibilidade de 1–2 s).
- A taxa por IP em S3 é **derivada**; nenhuma fonte a publica.
- O gerador não reproduz TLS/JA4, CGNAT real, custo por GB, IPv6 residencial nem Rapid Reset.
- A Fase B roda sob a proteção DDoS L7 da Cloudflare, sempre ligada; se agir, a corrida vira observação.
- É um testbed isolado: o resultado é **comparativo** entre chaves, não o número da internet real.
