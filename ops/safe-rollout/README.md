# Safe rollout do OpenPoet

Fundação operacional para preparar e aplicar releases sem compilar depois de
parar produção. O utilitário é **dry-run por padrão**. Nenhum comando mutante é
executado sem `--execute`; `apply` também exige o token exato gerado pelo
`prepare`.

Este utilitário não substitui aprovação humana. As regras do `AGENTS.md`
continuam valendo: deploy em `8081` só pode acontecer depois de autorização
direta e explícita do usuário.

## Fluxo

### 1. Inspecionar o plano

```bash
go run ./ops/safe-rollout prepare
go run ./ops/safe-rollout preflight
go run ./ops/safe-rollout apply
```

Os três comandos acima não escrevem arquivos, não executam testes/build e não
tocam em launchd.

### 2. Preparar uma release

O worktree precisa estar limpo. Testes rodam antes da criação do artefato; o
build recebe um `BuildVersion` igual ao release ID.

```bash
go run ./ops/safe-rollout prepare --execute \
  --repo "$PWD" \
  --releases-dir "$HOME/.openpoet-rollout/releases" \
  --relay-url "https://relay.example"
```

Saídas:

- diretório imutável `<releases-dir>/<release-id>/`;
- `openpoet` com SHA-256 registrado;
- `manifest.json` em modo `0600`;
- confirmation token aleatório.

O build/test acontece com a produção ainda rodando. O `prepare` recusa árvore
dirty para não gerar binário impossível de reproduzir.

### 3. Preflight somente leitura

```bash
go run ./ops/safe-rollout preflight --execute \
  --manifest "$HOME/.openpoet-rollout/releases/<release>/manifest.json" \
  --live-binary "$PWD/.run/openpoet" \
  --db "$PWD/.run/openpoet.db" \
  --plist "$PWD/.run/openpoet.production.plist"
```

Valida hash do artefato, arquivos do runtime, `PRAGMA quick_check` e
`/api/version` atual. Não altera produção.

### 4. Apply autorizado

Somente depois da autorização explícita do usuário:

```bash
go run ./ops/safe-rollout apply --execute \
  --confirm-token '<token-do-manifest>' \
  --manifest "$HOME/.openpoet-rollout/releases/<release>/manifest.json" \
  --live-binary "$PWD/.run/openpoet" \
  --db "$PWD/.run/openpoet.db" \
  --plist "$PWD/.run/openpoet.production.plist" \
  --backup-dir "$HOME/.openpoet-rollout/backups"
```

Ordem real:

1. repete o preflight;
2. cria snapshot consistente com `VACUUM INTO` e valida o backup;
3. faz `launchctl bootout`, que entrega SIGTERM ao OpenPoet;
4. aguarda `/api/version` ficar indisponível, sem `kill -9`;
5. preserva o binário anterior e troca o candidato por rename atômico;
6. migra, em uma única transação, MCPs/custom tools legacy para envelopes
   criptografados usando `OPENPOET_ENCRYPT_KEY` (ou o fallback já usado pelo
   runtime);
7. faz `launchctl bootstrap`;
8. exige três health checks consecutivos com a versão exata da release;
9. repete `PRAGMA quick_check` no banco live.

Se start ou health falhar depois da migração de secrets, o binário anterior e
o snapshot SQLite são restaurados antes de reabrir o LaunchAgent. A saída da
migração contém somente contagens.

### Preflight offline da migração de secrets

O utilitário dedicado exige caminho explícito e é dry-run por padrão. Ele não
aplica migrations de schema nem imprime nomes, comandos, env ou ciphertext.

```bash
go run ./cmd/openpoet-secret-migrate --db /caminho/openpoet.db

# Somente após backup e aprovação explícita:
OPENPOET_ENCRYPT_KEY='mesma-chave-do-runtime' \
  go run ./cmd/openpoet-secret-migrate --db /caminho/openpoet.db --execute
```

## Limites intencionais

- Dois processos OpenPoet nunca compartilham o SQLite durante o apply.
- Não há fallback para `kill -9`; graceful stop que excede o timeout aborta.
- O rollback restaura o banco automaticamente somente quando esta migração de
  secrets foi executada; outras migrations de schema continuam exigindo uma
  estratégia própria de compatibilidade N-1.
- O operador deve verificar que sessões estão idle antes do apply. Auto-restore
  não garante exactly-once para ações externas em andamento.
- O backup é consistente no instante do `VACUUM INTO`; se o candidato falhar
  depois da migração de secrets, writes entre snapshot e graceful stop são
  descartados pela restauração. Por isso o operador deve drenar atividade antes
  do apply.
- Releases e backups ficam fora de `.run` e fora do repositório por padrão.

## Testes

```bash
go test ./ops/safe-rollout
```

Os testes usam somente diretórios temporários, SQLite temporário, HTTP local e
command runners falsos. Eles não chamam launchd nem acessam a porta `8081`.
