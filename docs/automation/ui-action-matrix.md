# Matriz de paridade UI → Automation

Fonte canônica: [`ui-action-manifest.v1.json`](./ui-action-manifest.v1.json).
Esta matriz cobre mutations HTTP declaradas no router, chamadas mutáveis do
frontend e ações inline puramente locais. Cada rota de negócio possui binding
verificado para o mesmo Application Service usado pela UI e pela Automation API.

## Cobertura

| Item | Quantidade |
| --- | ---: |
| Rotas mutáveis `/api` | 118 |
| Ações inline de negócio | 57 |
| Ações `presentation_only` | 49 |
| Listeners estáticos de negócio | 19 |
| Listeners estáticos de apresentação | 27 |
| Exceções explícitas de telemetria/teste | 7 |

| Estado | Quantidade | Significado |
| --- | ---: | --- |
| `implemented` | 116 | UI e Automation compartilham Application Service/invariantes |
| `application_service_ready` | 0 | Nenhuma rota permanece em caminho intermediário |
| `gap` | 0 | Nenhum gap de Application Service |
| `internal_only` | 2 | Ingestão de hooks, não ação autônoma de usuário |

## Matriz por domínio

| Domínio | Total | Implementado | Service pronto | Gap | Interno | Riscos |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| agents | 3 | 3 | 0 | 0 | 0 | R2, R3 |
| ai | 16 | 16 | 0 | 0 | 0 | R1, R2, R3 |
| ai_configs | 4 | 4 | 0 | 0 | 0 | R4 |
| config | 1 | 1 | 0 | 0 | 0 | R4 |
| documents | 2 | 2 | 0 | 0 | 0 | R2 |
| files | 4 | 4 | 0 | 0 | 0 | R2, R3 |
| git | 3 | 3 | 0 | 0 | 0 | R2, R3 |
| hooks | 4 | 2 | 0 | 0 | 2 | R1, R2, R3 |
| mcp | 8 | 8 | 0 | 0 | 0 | R4 |
| notifications | 6 | 6 | 0 | 0 | 0 | R1, R2, R3 |
| projects | 8 | 8 | 0 | 0 | 0 | R1, R2, R3, R4 |
| proposals | 9 | 9 | 0 | 0 | 0 | R1, R2, R3, R4 |
| sessions | 9 | 9 | 0 | 0 | 0 | R1, R2, R3 |
| settings | 1 | 1 | 0 | 0 | 0 | R4 |
| skills | 10 | 10 | 0 | 0 | 0 | R2, R3 |
| tags | 4 | 4 | 0 | 0 | 0 | R2, R3 |
| task_links | 2 | 2 | 0 | 0 | 0 | R2 |
| tasks | 10 | 10 | 0 | 0 | 0 | R2, R3 |
| token_usage | 1 | 1 | 0 | 0 | 0 | R3 |
| tools | 6 | 6 | 0 | 0 | 0 | R4 |
| tunnel | 5 | 5 | 0 | 0 | 0 | R4 |
| update | 1 | 1 | 0 | 0 | 0 | R4 |
| voice | 1 | 1 | 0 | 0 | 0 | R1 |

## Segurança

- Todo `DELETE` é R3 ou R4.
- Credenciais, MCP com env/commands, AI configs, custom tools, tunnel e update
  são R4.
- Escrita de arquivo e commit são R3; execução arbitrária de tool é R4.
- `presentation_only` não recebe capability. A lista existe apenas para provar
  que botões/toggles locais foram inventariados.
- Telemetria, OTLP e endpoint test-only ficam numa allowlist machine-readable
  com justificativa; não viram capabilities.

## Application Services compartilhados

- `ProjectService`: list/get/create/update/delete/duplicate, boundary de
  credencial criptografada, validação e effects desacoplados.
- `TagService`: CRUD e atribuição de tags a projeto com deduplicação.
- `NotificationService`: listagem bounded, ativos, unread count e mark read/all
  por backend já existente.
- `SkillService`: CRUD global e por projeto, duplicate/import/restore,
  versionamento transacional e configuração de skills por projeto.
- `AIAgentService`: CRUD com validação de políticas/filtros e proteção do agente
  default.
- `AIConfigService`: CRUD/assignments, segredo criptografado, update parcial sem
  apagar API key e reinit somente pós-commit.
- `MCPService`: MCP global/projeto com ownership, comandos/args/env em envelopes
  criptografados e DTOs estritamente redigidos.
- `CustomToolService`: CRUD por projeto com comando criptografado e retorno sem
  plaintext.
- `ConfigurationService`: settings, MCP API key, sync global/projeto, shares e
  políticas globais/de projeto com lotes atômicos.
- `SessionService`: create/stop/reopen/input/evaluate e hint de imagem, com
  transições de estado, env/unsafe explícitos e efeitos pós-sucesso.
- `SessionEventWatcherService`: start/stop de watcher via adapter direto, sem
  requisição HTTP sintética.
- `HookResponseService`: respostas de permissão e task notification; ingestão
  de hooks permanece `internal_only`.
- `FileMutationService`: escrita/upload em projeto e sessão, paste de imagem,
  limites de payload e confinamento estrito de caminho relativo.
- `GitMutationService`: stage/unstage/commit por argumentos tipados e aprovação
  explícita para commit.
- `VoiceTranscriptionService`: transcrição com áudio, filename e language tag
  bounded antes do provider atual.
- `TunnelMutationService`: enable/disable, confirmação one-time de pairing,
  revoke e delete permanente; resultados e eventos não carregam credenciais,
  tokens ou chaves de dispositivo.
- `UpdateMutationService`: apply com bloqueio de instalação gerenciada/sessões
  ativas e autorização separada para `force`.
- `DocumentService`: memory docs e documentos temporários bounded, com secrets
  redigidos antes da persistência e respostas sem conteúdo bruto na criação.
- `ProposalService`: memory/task/skill/tool proposals via backend durável;
  accept/reject atômicos e risco R3/R4 com aprovação explícita.
- `AIAssistantService`: chat, conversations, initiate, suggestions,
  generate/validate/test e execução de tool via ports, sem transcript/prompt
  bruto e com outputs bounded/redigidos.
- `NotificationDeliveryService`: subscribe/unsubscribe, preferência e teste de
  push via port; chaves de subscription nunca aparecem na resposta.
- `TokenUsageService`: clear total com aprovação R3 e effect pós-commit.

Operações que manipulam segredo, comando, policy de tools, assignment de
provider ou sincronização externa exigem `R4Boundary` explícito com aprovação,
aprovador e justificativa. Credenciais, env e comandos não fazem parte dos DTOs
de saída; updates parciais preservam segredo omitido. Effects e reinit são
executados somente após persistência/sincronização bem-sucedida.

Conteúdo, prompts, argumentos JSON e outputs de AI/proposals possuem limites
explícitos. Secrets são removidos antes de provider/tool/persistência; respostas
de chat não incluem prompt, transcript, proactive context ou mensagens brutas.
Deletes, clear de usage e execução/aprovação de tool usam
`ActionAuthorization` com aprovação explícita.

Os services estão registrados em um registry tipado de 153 capabilities
(104 mutations e 49 reads) e compostos sobre as dependências reais do processo.
Os handlers humanos falham fechados se o bundle não estiver pronto; não existe
fallback mutável para o caminho legado. SSE, multipart e WebSocket permanecem
somente como bordas de transporte.

## Gates operacionais restantes

1. Ensaio integrado em banco e porta descartáveis.
2. Shadow mode com identities/scopes reais.
3. Backup, migração idempotente de secrets e rollback ensaiados.
4. Autorização explícita do presidente antes do cutover.

O teste `TestUIActionManifestCoversBackendAndFrontendMutations` falha quando uma
nova mutation de router/frontend ou um novo `onclick="app.*"` não recebe
classificação explícita.
