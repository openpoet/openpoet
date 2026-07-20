# Manual do Usuário — OpenPoet, Plataforma de Orquestração

> 🔮 **Este documento é uma PROJEÇÃO DE PRODUTO.**
> Ele descreve o OpenPoet como ele **será** quando a plataforma de orquestração
> estiver completa — escrito no presente, como um manual normal, para que você
> consiga avaliar o produto final antes de ele existir. Só uma base enxuta já
> funciona hoje (seção 2); **quase tudo nas seções seguintes é projeção de
> produto**. Nomes de capabilities, parâmetros, eventos, endpoints e formatos de
> configuração citados adiante são **sintaxe prevista** pelos documentos de
> design do produto e **podem mudar até a entrega**. Cada seção de projeção
> carrega o selo **🔮 Projeção de produto — sintaxe prevista, sujeita a
> mudança**, e os exemplos aparecem marcados como *(sintaxe prevista)*. Nada
> aqui é um compromisso de API estável.

---

## Sumário

1. [O que o OpenPoet está se tornando](#1-o-que-o-openpoet-está-se-tornando)
2. [O que já funciona hoje — a base](#2-o-que-já-funciona-hoje--a-base)
3. [Workspaces — N sessões no mesmo repositório sem conflito](#3-workspaces--n-sessões-no-mesmo-repositório-sem-conflito)
4. [Coordenação entre sessões — esperar, enviar, confirmar, vetar](#4-coordenação-entre-sessões--esperar-enviar-confirmar-vetar)
5. [O Coordenador IA](#5-o-coordenador-ia)
6. [Radar de conflitos com gate síncrono — bloquear em tempo real](#6-radar-de-conflitos-com-gate-síncrono--bloquear-em-tempo-real)
7. [Alcance — mais backends, streaming SSE, grupos e blackboard](#7-alcance--mais-backends-streaming-sse-grupos-e-blackboard)
8. [Ambientes por projeto — environment.yaml](#8-ambientes-por-projeto--environmentyaml)
9. [Segurança e limites — o contrato que vale em tudo](#9-segurança-e-limites--o-contrato-que-vale-em-tudo)
10. [Como este manual evolui](#10-como-este-manual-evolui)

Convenções dos exemplos: `BASE=http://localhost:8080` (a porta padrão do
OpenPoet); `AV1=$BASE/api/automation/v1`. A API de automação é restrita a
loopback e exige um bearer `opav1_` — veja a seção 9.

---

## 1. O que o OpenPoet está se tornando

O OpenPoet hoje já orquestra sessões de agentes de IA (Claude Code, Codex,
Copilot, ACP e OpenCode) sobre múltiplos projetos — locais e remotos via SSH —
a partir de uma interface web única, com tarefas, skills, MCP injetado por
sessão e uma API de automação com ~150+ capabilities auditadas.

A plataforma de orquestração transforma isso em algo maior: **uma frota de
sessões de IA trabalhando em paralelo no mesmo projeto, com segurança**. Em vez
de você abrir uma sessão de cada vez e torcer para duas não editarem o mesmo
arquivo, o OpenPoet passa a:

- **Isolar** cada sessão em um *workspace* (git worktree com branch própria),
  para que N sessões avancem no mesmo repo sem se pisar;
- **Coordenar**: sessões e orquestradores podem esperar eventos (fim de turno,
  sessão pedindo input), mandar instruções entre sessões com confirmação de
  entrega, e vetar em tempo real uma escrita que colide com outra sessão;
- **Decidir com IA quando vale a pena**: um coordenador IA opcional, de custo
  controlado, é consultado por incidente (nunca residente) e pode delegar
  trabalho spawnando sessões-operárias com briefing;
- **Bloquear de verdade quando você pedir**: um gate síncrono de PreToolUse
  que funciona até em sessões criadas com `--dangerously-skip-permissions` —
  exatamente as sessões que um orquestrador spawna em frota;
- **Provisionar ambiente por projeto**: `environment.yaml` declara setup,
  portas e serviços; cada workspace ganha seu dev server na sua porta, sem
  colidir com nada — inclusive com portas reservadas que o alocador nunca
  entrega.

O princípio de projeto que atravessa tudo: **detecção e enforcement são código
determinístico dentro do servidor; a IA nunca fica entre uma chamada de
ferramenta e seu veredito**. LLMs consomem eventos e propõem ações; regras e
grants decidem. É esse princípio que torna a autonomia da frota tolerável — e é
por isso que este manual insiste tanto, adiante, em *quem* pode fazer *o quê*.

---

## 2. O que já funciona hoje — a base

Esta é a única parte do manual que descreve comportamento **já entregue**. Tudo
o mais (seções 3–8) é projeção.

**Orquestração multi-projeto e multi-backend.** Hoje o OpenPoet já roda sessões
de IA sobre cinco backends (Claude Code, Codex, Copilot, ACP, OpenCode), em
projetos locais e remotos por SSH, com tarefas, skills, documentos, MCP injetado
por sessão e uma API de automação com ~150+ capabilities auditadas. Essa é a
fundação madura sobre a qual a orquestração é construída.

**Identidade verificada e aprovações.** A base de segurança já está de pé: toda
mutação REST exige uma credencial verificada — uma de quatro (o cookie de UI da
instalação, o bearer por sessão `opst1_`, o bearer de automação `opav1_`, ou a
credencial de dispositivo pareado do túnel). Os hooks das sessões autenticam com
token por sessão (`X-Hook-Token`, prefixo `opht1_`); o MCP injetado executa como
o **ator real** da sessão pelo pipeline de capabilities (com erro estruturado
`approval_required` para verbos destrutivos); e o broker de aprovações proíbe
auto-concessão (`deny_self_grant`, salvo escopo explícito `approvals:self`). É
esse alicerce — **identidade verificada + sinal durável** — que torna seguro
tudo o que vem a seguir.

**Radar de conflitos, em observação.** A primeira geração do radar já está sendo
assentada: um coordenador em processo observa o firehose de hooks e começa a
manter um índice de claims — duas sessões vivas escrevendo o mesmo arquivo geram
um incidente, que vira evento durável no feed de automação e badge na UI. **Nesta
base, nada é bloqueado** — o radar só observa e avisa. O salto de *observar* para
*bloquear em tempo real* é a projeção da seção 6.

---

## 3. Workspaces — N sessões no mesmo repositório sem conflito

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

### O que você poderá fazer

Rodar várias sessões de IA **no mesmo projeto ao mesmo tempo**, cada uma em
sua própria *lane*: um git worktree com branch própria, diretório próprio e
camada `.claude/` própria (skills, hooks, MCP), materializada pelo config sync.
O checkout principal do projeto fica intocado enquanto as sessões trabalham.

- Um **workspace** é um worktree + branch + registro no banco (tabela
  `workspaces`, com `kind`, `status`, lease e metadados de ambiente — o schema
  já nasce com o formato final que os ambientes por projeto (seção 8) exigem).
- Workspaces vivem em `<projeto>/.openpoet/worktrees/<nome>`, escondidos do
  git via `.git/info/exclude`.
- Branches seguem o padrão **`openpoet/…`** — por exemplo
  `openpoet/task-42-login` para lanes ligadas a uma task.
- A sessão criada dentro do workspace herda o diretório via `work_dir`:
  terminal, hooks, MCP e structured view funcionam normalmente dentro da lane,
  e a sessão sobrevive a um restart do OpenPoet de volta à sua lane.

### Como usar

**Criar uma sessão já dentro de um workspace** — o parâmetro unificado
`workspace{}` existe nas três superfícies: `POST /api/sessions` (UI/REST),
capability `sessions.create` (automação) e ferramenta MCP
`openpoet_start_session`:

```bash
# (sintaxe prevista) REST, autenticado pelo cookie de UI:
curl -X POST "$BASE/api/sessions" -b cookies.txt -H 'Content-Type: application/json' \
  -d '{"project_id": 1, "task_id": 42,
       "workspace": {"mode": "auto"}}'
```

Modos previstos do objeto `workspace`:

| Modo | Comportamento |
|---|---|
| `auto` | Reusa uma lane livre da mesma task, ou cria uma nova; com o pooling de ambientes (seção 8), faz lease atômico de um workspace ocioso |
| `named` | Cria/usa uma lane com nome explícito (`name`); erro tipado em colisão |
| `existing` | Entra em um workspace já existente (`id`) |

Campos adicionais previstos: `base_ref` (default: branch atual do checkout
principal) e `keep` (pina a lane contra remoção automática).

**Criar e gerenciar workspaces pela API de automação** *(sintaxe prevista;
o envelope de comando abaixo já é o formato real do `POST /commands` hoje)*:

```bash
# criar
curl -s -X POST "$AV1/commands" \
  -H "Authorization: Bearer $OPAV1_TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{"command_id": "'"$(uuidgen)"'",
       "capability": "workspaces.create",
       "target": {"project_id": 1},
       "payload": {"name": "task-42-login", "base_ref": "main"},
       "correlation_id": "task:42"}'

# acompanhar até ficar pronto: espere o evento workspace.ready no feed
curl -s "$AV1/events?consumer=meu-orquestrador" -H "Authorization: Bearer $OPAV1_TOKEN"
```

Capabilities previstas: `workspaces.create` (escrita), `workspaces.list` /
`workspaces.get` (leitura), `workspaces.remove` e `workspaces.merge`
(**destrutivas — exigem grant de aprovação**, como `sessions.stop` já hoje).
Escopos novos: `workspaces:read` / `workspaces:write`. Eventos previstos no
outbox: `workspace.created`, `workspace.ready`, `workspace.needs_merge`,
`workspace.removed`, além dos resultados de merge.

**Merge-back — trazendo o trabalho de volta.** O merge é deliberadamente
simples e assistido por sessão, não mágico:

1. Ao fim de cada sessão, o OpenPoet calcula `{dirty_files, ahead, behind}` da
   lane e publica um resumo (evento `workspace.needs_merge` quando há trabalho
   pendente).
2. `workspaces.merge` *(destrutiva, requer aprovação)* pré-condiciona o
   checkout principal limpo e faz `git merge --no-ff openpoet/<nome>` **no
   worktree principal**.
3. Em conflito, o merge é abortado (o principal fica limpo, sem `MERGE_HEAD`)
   e a resposta traz **a lista de arquivos conflitados + um payload pronto de
   `sessions.create` com `custom_prompt`** para você spawnar, em um clique/um
   comando, uma sessão normal no worktree principal encarregada de resolver o
   conflito e rodar os testes.

**Ciclo de vida e limpeza (GC).** Sem daemon novo: ao fim da sessão, lanes
*provadamente inertes* (árvore limpa E zero commits à frente da base, ou branch
já mergeada) são removidas automaticamente — a remoção usa `git branch -d`
minúsculo, que o próprio git recusa em branch não mergeada, uma rede de
segurança gratuita. Todo o resto é mantido. Um GC no boot + a cada 6 horas
reconcilia banco × `git worktree list`, e lanes sujas paradas há dias viram
**notificação para você decidir** — nunca são apagadas à força.

### Limites e garantias

- **O checkout principal é inviolável**: criar/usar/derrubar lanes nunca deixa
  o worktree principal sujo nem muda sua branch.
- Workspaces MVP são para **projetos locais**; em projetos remotos SSH a
  criação retorna erro tipado de não suportado (chega depois, atrás de flag por
  projeto, quando o hardening SSH permitir).
- Superfícies "spoofáveis" (MCP com identidade de sessão, REST de sessão) só
  alcançam verbos **aditivos** (criar, listar). `workspaces.remove` e
  `workspaces.merge` existem apenas na automação com bearer + aprovação
  explícita. Pior caso de uma sessão confusa: bagunça de disco, nunca
  destruição do trabalho de outra lane.
- Nomes de workspace com escape de path (`../…`) são rejeitados; todo `path`
  registrado fica sob a raiz gerenciada.
- Lanes **compartilham serviços globais** até chegarem os ambientes por projeto
  (seção 8): duas lanes rodando `npm run dev` brigam pela mesma porta. Tarefas
  que precisam de ambiente vivo devem rodar no worktree principal (ou esperar o
  `environment.yaml`).
- Cada lane precisa de seu próprio `node_modules`/venv (custo de disco); a
  sessão instala o que precisar pelo próprio shell.

---

## 4. Coordenação entre sessões — esperar, enviar, confirmar, vetar

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

### O que você poderá fazer

Parar de fazer *polling* de histórico de sessão. A plataforma ganha primitivas
temporais e "mãos" determinísticas:

- **Eventos de turno**: `session.turn_completed {session_id, files_touched}`
  quando uma sessão termina um turno (hook `Stop`), e
  `session.awaiting_input` quando o sentinela de PTY detecta uma pergunta ou
  uma permissão parqueada. (Esses dois tipos de evento já começam a existir na
  base atual; a projeção os enriquece — p.ex. `files_touched` no
  `turn_completed`.)
- **`await_events`** — long-poll HTTP sobre o outbox durável: um orquestrador
  parqueia em uma chamada com filtros (tipo de evento, projeto, sessão) e
  cursor; é **acordado pelo commit** do evento, não por polling; retoma de onde
  parou via `next_cursor`. Timeout ≤ 55s por chamada, retomável.
- **`wait_for_session`** — bloqueia até a sessão-alvo atingir
  `turn_complete` / `awaiting_input` / `idle` / `stopped`. O resultado informa
  `signal_quality: exact|heuristic` — exato para codex (fase estruturada do
  app-server), heurístico (hook Stop + janela de idle) para os backends PTY.
- **`send_to_session` com ack** — enviar texto a outra sessão deixa de ser
  "torcer para chegar": o input é serializado por um mutex por sessão (que a
  base já garante) e o retorno traz `{submitted: true, acknowledged: true|false}`,
  correlacionado com o hook `UserPromptSubmit` da sessão-alvo (para codex,
  `turn_id` exato).
- **Veto síncrono de conflito** — a primeira "mão" de verdade: quando uma
  sessão pede permissão para escrever um arquivo que **outra sessão viva já
  reivindicou**, o servidor responde o pedido de permissão com **deny + uma
  mensagem que o modelo lê** ("este arquivo está sendo editado pela sessão X"),
  e o agente re-roteia sozinho. Determinístico, sub-segundo, custo zero de LLM.
- **Linhagem e auditoria de spawn** — sessões criadas por um orquestrador
  registram `parent_session_id`/`spawned_by`, e cada spawn cria um `work_run`
  com alvo de execução `type=session`: você sempre sabe quem criou o quê.

### Como usar

**Parquear num evento e agir** *(sintaxe prevista — a mecânica de
`consumer`/cursor/ack já é a real do feed de eventos de hoje)*:

```bash
# long-poll: retorna assim que alguma sessão terminar um turno
curl -s "$AV1/events?consumer=orq&await=1&types=session.turn_completed&timeout=20" \
  -H "Authorization: Bearer $OPAV1_TOKEN"

# confirme o processamento (cursor durável — sobrevive a restart):
curl -s -X POST "$AV1/events/ack?consumer=orq" \
  -H "Authorization: Bearer $OPAV1_TOKEN" -H 'Content-Type: application/json' \
  -d '{"consumer":"orq","through":"12345"}'
```

**Mandar instrução com confirmação** *(sintaxe prevista, via MCP dentro de uma
sessão ou capability equivalente na automação)*:

```
openpoet_send_to_session {"session_id": "<alvo>", "text": "rode os testes e reporte"}
→ {"submitted": true, "acknowledged": true}
```

**Parar a sessão de outra pessoa exige grant** (fluxo do broker de aprovações,
que já existe hoje):

```bash
# 1) o orquestrador tenta sessions.stop → recebe erro estruturado approval_required
# 2) um cliente "warden" (com escopo approvals:grant) emite o grant one-shot:
curl -s -X POST "$AV1/approvals" \
  -H "Authorization: Bearer $OPAV1_WARDEN" -H 'Content-Type: application/json' \
  -d '{"target_client_id": "<id-do-orquestrador>",
       "capability": "sessions.stop",
       "expires_in_seconds": 120}'
# 3) o orquestrador repete o comando com "approval_token": "<token>" — consumo único
```

O grant tem TTL curto (padrão 2 min, máx. 5 min) e **consome-se uma vez**:
repetir o token falha.

### Limites e garantias

- O veto síncrono só nega **escrita contestada de verdade** (write×write no
  mesmo arquivo entre sessões vivas): pedir permissão para um arquivo não
  contestado passa normal. A precisão do veto é testada com controles
  negativos — ele não pode taxar trabalho inocente.
- `await_events`/`wait_for_session` são **HTTP-only** (automação/MCP sobre
  HTTP); um turno de sessão nunca fica bloqueado por um await de terceiro.
- O cliente de automação do coordenador **não possui `approvals:grant`**: toda
  ação destrutiva dele passa pelo warden (ou por você). Auto-grant continua
  bloqueado pela base de segurança.
- Sinal heurístico é rotulado como tal (`signal_quality`) — um orquestrador
  não deve tratar `idle` heurístico como verdade absoluta em backends PTY.
- Todas as ações do coordenador aparecem no ledger de comandos da automação:
  mãos auditadas, não backdoors.

---

## 5. O Coordenador IA

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

### O que você poderá fazer

Ligar um "cérebro" opcional em cima do radar + das mãos: o slot de provider
**`ai_coordinator`** (configurável como os demais slots de IA do OpenPoet, com
sua persona "Coordinator"). Ele **não é residente**: custo zero com a frota
saudável. Quando um incidente abre (ex.: conflito crítico), o OpenPoet faz
**uma consulta one-shot** ao modelo, com orçamento máximo por consulta
(budget em USD) e teto diário de consultas — e o resultado é um **vocabulário
fechado de ações** que a camada determinística revalida antes de executar:

`message_session, spawn_session, answer_permission, set_model, reassign_task,
escalate_human, dismiss` *(vocabulário previsto)*.

O quanto ele pode agir é um **dial por projeto** — `coordinator_mode`:

| Modo | Comportamento |
|---|---|
| `off` | Incidentes abrem, mas o cérebro nunca é consultado para este projeto |
| `observe` | Consulta acontece, mas ações mutantes são recusadas pela política; tudo vira escalação para você |
| `assist` | Ações brandas (mensagem para sessão, escalar) executam; o resto pede você |
| `delegate` | O coordenador pode spawnar sessões-operárias com briefing e propor ações — destruição continua atrás de grant |

Em modo **delegate**, `spawn_session` cria uma sessão real com briefing
(`custom_prompt`), linhagem (`parent_session_id`) e `work_run` registrados —
operárias instruídas, não sessões soltas.

### Como funciona por dentro (e por que você pode confiar)

- **Uma consulta por incidente** — histerese e o dedupe de incidentes do radar
  garantem que 5 novas evidências do mesmo conflito não geram 5 consultas.
- **Revalidação fechada**: verbo desconhecido, ID de sessão alucinado ou
  resposta malformada ⇒ nenhuma mutação acontece; o incidente escala para
  você como notificação, e o processo segue saudável. O modelo **propõe**;
  política e grants **dispõem**.
- **Budget é lei**: com teto diário 1, o segundo incidente do dia não consulta
  — gera evento/nota de budget.
- **Higiene do prompt**: o briefing enviado ao modelo não carrega ANSI cru nem
  segredos (tokens `opav1_`/chaves) sem redação.
- **O cérebro é um módulo, não um compromisso**: a interface é dado (eventos
  no outbox de um lado, capabilities auditadas do outro). Se o one-shot ficar
  pequeno, um agente externo (Agent SDK) pode substituí-lo consumindo os
  mesmos eventos com o próprio token com escopo — sem mudar a camada de
  enforcement.

### O que o coordenador IA **nunca** pode fazer

- Auto-conceder aprovações (não tem `approvals:grant`; `deny_self_grant`
  continua valendo para ele como para qualquer cliente).
- Executar verbo destrutivo sem grant — mesmo em `delegate`, um
  `stop_session` proposto pelo cérebro aterrissa como `approval_required`.
- Ficar residente consumindo tokens: sem incidente, sem consulta.
- Agir em projeto com dial `off`.

---

## 6. Radar de conflitos com gate síncrono — bloquear em tempo real

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

Este é o salto que transforma o radar de *observação* (seção 2) em *enforcement*:
o servidor passa a poder **negar, em tempo real, uma escrita conflitante** —
sub-segundo, sem LLM no caminho, com verdicto explicável ("a sessão A escreveu
`internal/handlers/api.go` há 40s; você está prestes a escrevê-lo").

### A escada de intervenção

O comportamento diante de conflito é um **dial por projeto**,
`conflict_policy`, com quatro degraus (escada L0–L3 do design):

| Política | O que acontece |
|---|---|
| `observe` | Só observação: incidentes, eventos, badges (é a base de hoje) |
| `warn` | + o pedido de permissão em arquivo contestado é anotado ou **negado com mensagem explicativa** que o modelo lê e re-roteia (o veto síncrono da seção 4) |
| `gate` | + **PreToolUse síncrono**: o bridge de hooks consulta o servidor **antes** de cada ferramenta de escrita; o servidor responde em sub-segundo, da memória, com o deny nativo do Claude Code (`hookSpecificOutput.permissionDecision`) e uma razão nomeando a outra sessão |
| `enforce` | + pausa determinística do "perdedor" (toque mais tardio; desempate por task não vinculada / sessão mais nova): todas as escritas dele são negadas com "pausado pelo conflito C-x; aguardando sessão Y" até resolver |

### Por que o gate importa: skip-permissions

Sessões criadas com `dangerously_skip_permissions` — exatamente o default de
uma frota spawnada por orquestrador — **não têm diálogo de permissão**, então
`warn` não as alcança. Mas o Claude Code dispara hooks PreToolUse em qualquer
modo de permissão. O gate síncrono é, por isso, **o único mecanismo que governa
sessões skip-permissions**, e ele se auto-implanta: o config sync regenera o
`bridge.sh` antes de toda criação de sessão.

Garantias do gate:

- **Fail-open com timeout**: se o servidor não responder no prazo, a ferramenta
  executa. O gate pode degradar para observação; nunca pode "brickar" sessão.
- **Kill switch global**: `conflict_enforcement_enabled=false` desliga o
  enforcement instantaneamente (e religa igual).
- **Ferramentas de leitura nunca bloqueiam** (write-class only); os arquivos
  independentes de cada sessão nunca conflitam.
- **Opt-in real**: projeto que não subiu o dial não é governado pelo gate.
- **Proteção de substrato**: escrita no workspace de outra sessão ou nos
  internos `.openpoet/` é negada com razão própria, distinta de conflito — uma
  sessão não corrompe a lane da outra nem os metadados de orquestração.
- **Sem token de hook, sem gate**: o endpoint do gate exige o `X-Hook-Token`
  da sessão (base de segurança) — um gate bloqueante sobre entrada forjável
  seria uma arma de negação de trabalho.

**Recomendação de adoção.** Como qualquer dial de enforcement, suba um degrau
por vez: rode em `observe` por alguns dias de uso real para calibrar a taxa de
falso-positivo, então promova para `warn`, e só ligue `gate`/`enforce` quando
tiver confiança na precisão do radar naquele projeto. O kill switch está sempre
a um comando de distância.

---

## 7. Alcance — mais backends, streaming SSE, grupos e blackboard

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

Com o radar e as mãos de pé, a plataforma estende o alcance: mais backends sob o
mesmo guarda-chuva, push de eventos em tempo real, escopo por grupo de projetos
e uma memória compartilhada para múltiplos agentes se coordenarem.

### Mais backends sob um só guarda-chuva

- **Adapter codex + ACP/Copilot**: o codex app-server não tem hooks; um adapter
  tapa o stream JSON-RPC (itens de patch/exec carregam paths) e alimenta o mesmo
  índice de claims; eventos ACP sintetizam PreToolUse. Fidelidade por backend é
  explícita e honesta: **claude/acp > copilot > codex > opencode**, com o
  reconciliador git como piso universal — nenhum backend fica cego, mesmo que
  alguns reportem com menos precisão. O objetivo é que a mesma coordenação —
  esperar, vetar, gate — valha para toda a frota, não só para Claude Code.

### SSE — push de eventos em tempo real

*(sintaxe prevista)* Além do long-poll, o feed de eventos ganha **push de
verdade** por Server-Sent Events, ideal para dashboards ao vivo e integrações
externas:

```bash
# (sintaxe prevista) stream contínuo de eventos, retomável por Last-Event-ID:
curl -N "$AV1/events/stream" \
  -H "Authorization: Bearer $OPAV1_TOKEN" \
  -H "Accept: text/event-stream" \
  -H "Last-Event-ID: 12345"
```

O `Last-Event-ID` é mapeado ao cursor durável do outbox, então uma reconexão
retoma exatamente de onde parou. Para clientes MCP externos, o `GET /mcp` deixa
de responder 405 e passa a servir o mesmo stream SSE — push conforme o spec de
Streamable HTTP, sempre respeitando o escopo do ator autenticado.

### Grupos e tags — escopo por conjunto de projetos

Um **grupo de coordenação é uma tag** de projetos. Um cliente de automação pode
ser provisionado com um `project_filter` e então **só enxerga e só toca** os
projetos do grupo — comandos fora do grupo retornam 403 tipado, e o filtro vale
também no stream de eventos.

```json
// (sintaxe prevista) provisionamento de um cliente escopado ao grupo G:
{ "client_id": "coordenador-time-web",
  "scopes": ["conflicts:read", "sessions:control"],
  "project_filter": { "tag_ids": [7] } }
```

Assim, "coordenador do grupo X" é uma **decisão de provisionamento**, não lógica
de aplicação: o mesmo binário de orquestrador, provisionado com filtros
diferentes, governa times diferentes sem nunca cruzar a fronteira.

### Blackboard — quadro compartilhado com leases e fencing

*(sintaxe prevista)* Uma **memória compartilhada chaveada** para múltiplos
agentes coordenarem estado, com escopo `global | group | project`, TTL opcional
e **compare-and-swap por versão** (CAS):

```
openpoet_blackboard_put {
  "scope": "group", "scope_id": 7,
  "key": "release-freeze",
  "value": {"active": true, "by": "coordenador-time-web"},
  "expected_version": 3            // CAS: falha se a versão atual não for 3
}
→ {"ok": true, "version": 4}

openpoet_blackboard_get {"scope": "group", "scope_id": 7, "key": "release-freeze"}
→ {"value": {"active": true, "by": "..."}, "version": 4}
```

O CAS é a peça central de **eleição de coordenador e fencing**: eleger um
coordenador é um CAS numa chave de *lease* com TTL; e uma mutação "fenced"
carregando uma versão de lease **velha falha fechado** — um coordenador zumbi
(que perdeu o lease mas ainda está rodando) **não consegue agir**. É o que
permite ter candidatos a coordenador redundantes sem risco de dois cérebros
mandando ao mesmo tempo. Escopos previstos: `blackboard:read` /
`blackboard:write`; toda escrita registra o **ator verificado** que a fez.

---

## 8. Ambientes por projeto — environment.yaml

> 🔮 **Projeção de produto — sintaxe prevista, sujeita a mudança.**

### O que você poderá fazer

Fechar a última lacuna do paralelismo: **ambiente**. Um projeto declara em
`.openpoet/environment.yaml` como se prepara e roda; o OpenPoet provisiona
cada workspace com setup executado, porta alocada e serviço de dev com health
check — e desmonta tudo na devolução. É o que finalmente permite duas sessões
rodarem `npm run dev` no mesmo projeto sem brigar por porta.

Exemplo *(formato previsto)*:

```yaml
# .openpoet/environment.yaml
setup:
  - npm ci
ports:
  web: {}                    # porta nomeada, alocada pelo servidor
services:
  web:
    scope: sandbox           # sandbox = um por workspace | shared = declarado, não gerenciado
    driver: process          # none | process | (compose, opcional)
    command: npm run dev
    health: http://localhost:{{PORT.web}}/
env:
  PORT: "{{PORT.web}}"
  BASE_URL: "http://localhost:{{PORT.web}}"
teardown: []
reuse:
  policy: pool               # pool | ephemeral
  max_idle: 1
```

As variáveis renderizadas (`PORT`, `BASE_URL`, `DATABASE_URL`…) entram no
ambiente da sessão pelo canal confiável de env que o OpenPoet já usa; o
snapshot fica em `resources_json` no workspace, então **reabrir a sessão
devolve exatamente os mesmos valores**.

### Aprovação por SHA-256 — a propriedade de segurança central

Um manifesto transforma conteúdo do repositório em comandos que o OpenPoet
executa — e agentes editam repositórios. Por isso:

- O provisioner **recusa executar** qualquer manifesto cujo SHA-256 não esteja
  aprovado (`manifest_approval_required` traz o hash na recusa).
- A aprovação — `environments.approve_manifest` — existe **somente no plano de
  automação** (tier unsafe/explícito, com grant): **nunca** aparece como
  ferramenta MCP, e um bearer `opst1_` de sessão recebe 403. Uma sessão não
  consegue aprovar o manifesto que ela mesma escreveu.
- **Qualquer byte alterado** no manifesto (até um comentário) invalida a
  aprovação: o próximo provision falha fechado até você re-aprovar.

```bash
# (sintaxe prevista) aprovar o manifesto do projeto 5:
SHA=$(shasum -a 256 .openpoet/environment.yaml | cut -d' ' -f1)
curl -s -X POST "$AV1/commands" -H "Authorization: Bearer $OPAV1_TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{"command_id":"'"$(uuidgen)"'",
       "capability":"environments.approve_manifest",
       "target":{"project_id":5},
       "payload":{"content_sha256":"'"$SHA"'"},
       "reason":"revisei o environment.yaml do projeto web",
       "approval_token":"<grant do warden>"}'
```

### Portas são dados, não prosa

O alocador de portas é uma tabela no banco (`environment_resources`): aloca de
uma faixa configurada, sonda com `net.Listen`, e — o detalhe importante —
**portas reservadas são linhas**, não convenção. Você semeia "8080 é o app,
8081 é produção" como linhas reservadas e o alocador **nunca** as entrega;
um manifesto exigindo `port: 8081` explicitamente é recusado com erro de
porta reservada, mesmo aprovado. Dois workspaces do mesmo projeto recebem
portas distintas e seus dev servers respondem cada um na sua — o isolamento
que worktrees sozinhos não davam.

### Pooling e leases

Com `reuse: {policy: pool, max_idle: N}`, devolver um workspace o mantém
quente (node_modules instalado, serviço de pé conforme política). Um
`sessions.create {workspace: {mode: "auto"}}` com o caminho principal ocupado
faz **lease atômico** de um workspace pronto em segundos, sem re-provisionar.
Leases expiradas (sessão que morreu) são recuperadas pelo janitor. Política
`ephemeral`: o diretório é removido na devolução.

### Limites e garantias

- Projeto **sem** manifesto continua ganhando o workspace simples da seção 3 com
  **zero execução** de setup — nada roda que você não declarou.
- Setup que falha ⇒ workspace `failed`, evento `workspace.failed`, sem
  processos órfãos, checkout principal limpo.
- Processos de serviço têm PID rastreado (`runtime_ref`) e morrem no teardown;
  a reconciliação de órfãos roda no startup.
- Serviços `shared` (ex.: um Postgres de verdade) são **declarados, não
  gerenciados** (driver `none`) nesta máquina onde produção convive com dev;
  o driver compose é opcional e só entra se docker existir e for desejado.
- Workspaces SSH remotos são a última fronteira, opcional e atrás do backlog
  de hardening SSH.

---

## 9. Segurança e limites — o contrato que vale em tudo

O contrato de segurança **não relaxa** conforme as capacidades crescem — ele é o
que torna a autonomia da frota tolerável. As linhas marcadas *(já vale hoje)*
descrevem a base entregue; as demais são o compromisso que cada capacidade
projetada terá de honrar quando entregar.

1. **Toda mutação tem um ator verificado.** Uma de quatro credenciais (cookie
   de UI, `opst1_` de sessão, `opav1_` de automação, dispositivo pareado do
   túnel); mutação sem credencial = 401. Auditoria registra quem realmente
   agiu — clique de UI ≠ agente de sessão ≠ cliente de automação. *(já vale hoje)*
2. **Verbos destrutivos exigem aprovação explícita.** `sessions.stop`,
   `workspaces.remove/merge`, `environments.teardown` etc. retornam
   `approval_required` estruturado; grants são one-shot, com TTL de minutos,
   emitidos por um cliente warden (escopo `approvals:grant`) ou por você.
   Auto-grant é negado por padrão (`deny_self_grant`); `approvals:self` é uma
   decisão de provisionamento sua, nunca um acidente. *(o broker já existe hoje;
   os verbos projetados aderem a ele)*
3. **Superfícies spoofáveis só recebem verbos aditivos.** MCP com identidade
   de sessão e REST de sessão criam e listam; remover, mergear e aprovar
   manifesto vivem apenas no plano de automação autenticado.
4. **O coordenador (código ou IA) nunca segura `approvals:grant`**, e toda
   ação dele fica no ledger auditado de comandos.
5. **Enforcement é fail-open com kill switch.** Gate e veto degradam para
   observação em timeout; `conflict_enforcement_enabled=false` desliga tudo
   na hora. O sistema pode ficar cego; não pode travar seu trabalho.
6. **Conteúdo de repositório nunca vira execução sem aprovação por hash.**
   O manifesto de ambiente só executa com SHA-256 aprovado fora do alcance de
   sessões.
7. **A API de automação continua loopback-only**, rejeita Origin de navegador
   e exige bearer mesmo em localhost; requisições vindas do túnel são
   rejeitadas no plano de automação. *(já vale hoje)*
8. **Portas de produção são dados reservados** que o alocador se recusa a
   entregar.

O que continua exigindo **você**, sempre: aprovar manifesto de ambiente;
conceder (ou delegar a um warden) grants destrutivos; subir os dials
(`conflict_policy`, `coordinator_mode`) por projeto; e decidir se algum
cliente merece `approvals:self`.

---

## 10. Como este manual evolui

As seções 3–8 descrevem o **comportamento-alvo do produto**, fiéis aos
documentos de design consolidados do OpenPoet. São **projeções**: cada
capacidade só passa a valer quando é entregue e validada de ponta a ponta; até
lá, nomes de capabilities, parâmetros, eventos, endpoints e formatos de config
podem mudar. Este documento é atualizado à medida que cada capacidade sai do
papel — e o selo 🔮 some de uma seção somente quando a funcionalidade dela existe
de verdade.

Panorama das capacidades:

| Capacidade | O que entrega | Situação |
|---|---|---|
| Orquestração multi-projeto e identidade verificada | Sessões de IA sobre 5 backends e múltiplos projetos, API de automação (~150 capabilities), MCP por sessão, credenciais verificadas por ator, broker de aprovações | ✅ Base de hoje |
| Radar de conflitos (observação) | Detecção de sobreposição de arquivos entre sessões vivas, eventos duráveis, incidentes, badges na UI | 🌱 Base em introdução |
| Workspaces | Worktrees por sessão, `workspace{}` em REST/automação/MCP, merge-back assistido, GC | 🔮 Projeção |
| Coordenação entre sessões | `await_events`/`wait_for_session`, `send_to_session` com ack, veto síncrono, stop com grant, linhagem de spawn | 🔮 Projeção |
| Coordenador IA | Slot `ai_coordinator`, consulta one-shot com budget, vocabulário fechado revalidado, dial `coordinator_mode`, modo delegate | 🔮 Projeção |
| Gate de conflitos síncrono | PreToolUse bloqueante (cobre skip-permissions), escada `observe→warn→gate→enforce`, kill switch, proteção de substrato | 🔮 Projeção |
| Alcance | Adapter codex/ACP/Copilot, streaming SSE, grupos/tags com `project_filter`, blackboard com CAS/fencing | 🔮 Projeção |
| Ambientes por projeto | `environment.yaml` + aprovação SHA-256, alocador de portas com reservas, driver de processo, pooling/leases | 🔮 Projeção |

Este manual foi escrito para você poder **avaliar o destino antes da viagem**.
Se algo aqui não fizer sentido para o seu fluxo, é exatamente o momento certo de
dizer — enquanto é projeção, e não API estável.
