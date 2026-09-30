---
name: deploy
description: Deploy the OpenPoet application to the .run directory on port 8081. Use when the user asks to deploy, fazer deploy, atualizar o serviço, subir o serviço, restart da produção, or update the running production instance.
user-invocable: false
allowed-tools: Bash(./.scripts/deploy.sh *), Bash(make deploy*), Bash(sleep *), Bash(journalctl --user -u openpoet-prod *)
---

# Deploy Skill

Deploy the OpenPoet service to the `.run/` directory on port 8081 using the deploy script.

Production runs as the systemd user unit `openpoet-prod.service` on the production Linux host. Logs: `journalctl --user -u openpoet-prod -f`.

## REGRA CRÍTICA — Usar SEMPRE o script `.scripts/deploy.sh`

- **NUNCA** executar os passos de deploy manualmente (kill, build, cp, nohup).
- **SEMPRE** usar o script `.scripts/deploy.sh` que já encapsula toda a lógica de deploy.
- O script se daemoniza automaticamente (unit transiente `openpoet-deploy`) — sobrevive mesmo se o Claude Code ou o terminal cair.
- Use `./.scripts/deploy.sh --pull` quando o commit a publicar já está no `origin/main` e a árvore local está limpa (faz `git pull --ff-only` antes do build).
- O deploy faz health check em `/api/version` e, se o binário novo não responder, **volta sozinho** para `.run/openpoet.previous`. Rollback manual: `./.scripts/deploy.sh --rollback`.

## REGRA CRÍTICA — O deploy é SEMPRE o último passo do turno

O processo desta sessão é filho do `openpoet-prod`. Cerca de 1 s depois de o
`deploy.sh` voltar, a produção para e **esta sessão é morta junto** (volta
sozinha com `claude --resume`). Nada que você planejar para depois do
`deploy.sh` roda nesse turno.

Por isso, **antes** de chamar o `deploy.sh`:

1. Commit feito, testes rodados, relatório/task/docs já atualizados com o que
   foi entregue (deixe claro que o deploy foi disparado e que a verificação
   vem a seguir).
2. Nada de `sleep`, `--status` ou "vou verificar" depois do deploy no mesmo
   turno. Chamar o `deploy.sh` e encerrar o turno com uma linha curta.

**A verificação pós-deploy chega como prompt.** Quando o deploy terminar, o
OpenPoet digita nesta sessão: *"[OpenPoet] O deploy que você disparou (…)
terminou: …"* (ou, se o turno foi cortado, *"O servidor do OpenPoet reiniciou
durante o seu turno …"*). Aí sim: rode `./.scripts/deploy.sh --status`, faça a
verificação que a task pede e registre o resultado. O mesmo resultado sai no
outbox como `platform.deploy.completed` / `platform.deploy.failed`.

## Steps

### 1. Executar o deploy (último comando do turno)

```bash
./.scripts/deploy.sh
```

O script faz tudo automaticamente:

- (opcional `--pull`) `git pull --ff-only`, recusando árvore suja
- Roda `make build` ANTES de parar a produção (build quebrado não derruba nada)
- Para `openpoet-prod.service` (nunca toca no 8080)
- Guarda o binário atual em `.run/openpoet.previous` e instala o novo
- Inicia `openpoet-prod.service` e espera `/api/version` responder (até 60 s)
- Se não responder, restaura o binário anterior e religa (rollback automático)
- Grava o deploy em `.run/deploy.record.json` (com esta sessão como
  `requested_by_session`), que o servidor lê para retomar esta sessão e
  publicar o evento

O comando retorna imediatamente — o deploy roda em background. Encerre o turno.

### 2. Quando chegar o prompt com o resultado

```bash
./.scripts/deploy.sh --status
```

- `SUCCESS`: fazer a verificação pedida e reportar sucesso.
- `FAILED` / `ROLLED BACK`: checar `./.scripts/deploy.sh --log`, mostrar as
  linhas relevantes e reportar o erro.

### Deploy disparado de fora de uma sessão do OpenPoet

Num terminal comum (sem `OPENPOET_SESSION_ID`) ninguém é avisado: aguarde e
confira com `sleep 8 && ./.scripts/deploy.sh --status` e `./.scripts/deploy.sh --log`.
