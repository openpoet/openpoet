# Deploy gate — nada vai para produção sem estar commitado

**Regra:** nunca mais haver deploy do OpenPoet sem tudo
commitado. Todo caminho que publica, faz release ou reinicia o OpenPoet em
produção roda o gate obrigatório `ops/deploy-gate/gate.sh` **antes de agir**.

## O que o gate exige

O gate só passa quando as três condições valem ao mesmo tempo:

1. **Working tree limpo:** `git status --porcelain` vazio. Arquivo modificado,
   staged, apagado ou **untracked** reprova. Arquivo coberto pelo `.gitignore`
   (`.run/`, `build/`, `openpoet.db`, …) não conta.
2. **Commit na `main`:** o commit implantado (HEAD, ou `--commit`) está contido
   na branch local `main`.
3. **Commit no remoto:** o mesmo commit está contido em `origin/main`, com um
   `git fetch` feito na hora. Se o fetch falhar (sem rede, sem credencial), o
   gate **reprova**. Ele nunca presume que o commit foi empurrado.

Quando reprova, o comando sai com código 1, não faz nada e lista o motivo e
cada arquivo pendente:

```
==================================================================
DEPLOY GATE FAILED (deploy) — nothing was deployed or restarted.
Rule: no deploy without everything committed on main and pushed.
Repository: /home/dev/openpoet
Commit:     59b7d567333c
  ✗ working tree has uncommitted changes (git status --porcelain is not empty)
  ✗ commit 59b7d567333c is not on 'origin/main' (git push origin main first)
Uncommitted files (git status --porcelain):
    ?? ops/deploy-gate/
Fix: commit (and push) everything, or discard what is not meant to ship,
then run the deploy again. See docs/deploy-gate.md.
==================================================================
```

Para conferir sem fazer deploy: `make deploy-gate`.

## Onde o gate roda

| Caminho | Quando o gate roda | Bypass |
|---|---|---|
| `.scripts/deploy.sh` (deploy e `--pull`) | (a) no chamador, antes de daemonizar; (b) no deploy daemonizado, depois do `git pull` e antes do build; (c) depois do build, logo antes de parar a produção. Também recusa se o HEAD mudar durante o build. | `--emergency-bypass "<motivo>"` |
| `.scripts/deploy.sh --rollback` | antes de parar a produção | `--emergency-bypass "<motivo>"` |
| `ops/safe-rollout prepare` | antes dos testes e do build | nenhum |
| `ops/safe-rollout apply` | antes do preflight e do `launchctl bootout`, contra o `git_sha` do manifest | nenhum |
| GitHub Actions `release.yml` (tag `v*`) | antes do GoReleaser: a tag tem que apontar para um commit da `main` | nenhum |

O `deploy.sh` **falha fechado**: se `ops/deploy-gate/gate.sh` não existir, ele
se recusa a fazer deploy ou rollback. Iniciar o deploy daemonizado direto
(`_DEPLOY_DAEMONIZED=1`) não pula o gate, porque o próprio daemon também o
roda. O resultado fica em `.run/deploy.record.json`, no campo `gate`
(`passed` ou `bypassed: <motivo>`).

### Fora do escopo, e por quê

- **`dev-server.sh` (porta 8080):** o servidor de desenvolvimento existe para
  testar código ainda não commitado. Ele não toca na produção.
- **`systemctl --user restart openpoet-prod` direto, e os restarts automáticos
  do systemd (`Restart=always`, reboot):** reiniciam o **mesmo** binário que já
  está em `.run/openpoet`. Nenhum código novo chega à produção por esse
  caminho. Colocar o gate no `ExecStartPre` da unit derrubaria a produção
  depois de um crash só porque havia um arquivo sujo no repositório. A única
  forma sancionada de trocar o binário é o `deploy.sh` (a skill de deploy
  proíbe `cp`/`kill`/`nohup` manuais).
- **`update.apply` (auto-update pela UI):** instala um binário de release do
  GitHub, que só existe se passou pelo gate do `release.yml`. Em produção, o
  binário vem do `make build` (versão `<sha>-<timestamp>`), que o updater trata
  como dev build e por isso não atualiza.

## Bypass de emergência

Existe um só bypass, e só no `deploy.sh`. Ele é explícito e registrado. Não
existe variável de ambiente que desligue o gate.

```bash
./.scripts/deploy.sh --emergency-bypass "produção fora, hotfix do INC-123"
./.scripts/deploy.sh --emergency-bypass "binário novo corrompendo dados" --rollback
```

- O motivo é obrigatório, com pelo menos 10 caracteres.
- Todas as verificações rodam e são impressas mesmo assim, com a lista de
  arquivos pendentes.
- Cada uso grava uma linha JSON em `.run/deploy-gate-bypass.log`, com `time`,
  `context`, `user`, `session` (`OPENPOET_SESSION_ID`), `commit`, `reason`,
  `failures` e `dirty`. Se não der para gravar o log, o bypass é recusado.
- O deploy registra `gate: "bypassed: <motivo>"` no `.run/deploy.record.json`.
- **Agentes (Claude Code, Codex, …) nunca usam o bypass por conta própria.**
  Só com ordem expressa do usuário para aquele deploy específico, e informando
  o motivo dado por ele.
- Depois da emergência: commitar e empurrar o que foi implantado e registrar o
  incidente.

## Testes

`make test-deploy-gate` roda:

- `ops/deploy-gate/gate_test.sh`: o gate contra repositórios git descartáveis,
  com um `origin` bare local. Cobre árvore limpa, arquivo ignorado,
  modificado, untracked, staged, commit não empurrado, commit só em feature
  branch, commit antigo já na main, `--commit` inexistente, origin inacessível,
  bypass sem motivo, motivo curto, bypass logado e variável de ambiente que
  não desliga o gate.
- `ops/deploy-gate/deploy_sh_test.sh`: roda uma **cópia** do `.scripts/deploy.sh`
  num repositório descartável, com `systemctl`, `systemd-run`, `make`, `lsof`,
  `curl` e `journalctl` substituídos por stubs. Verifica que árvore suja
  barra deploy, `--pull` e `--rollback`; que o daemon iniciado direto também
  barra; que o bypass passa e fica logado; que árvore limpa passa; que commit
  não empurrado barra; e que o deploy falha fechado sem o script do gate. É
  pulado quando `.scripts/` não existe (por exemplo num clone novo ou no CI).

As duas suítes também rodam em `go test ./...`, via
`ops/deploy-gate/gate_test.go`. Os casos do gate no safe-rollout ficam em
`ops/safe-rollout/plan_test.go`.

## Observação: `.scripts/` não é versionado

`.scripts/` (onde ficam `deploy.sh`, `dev-server.sh` e `backup-db.sh`) está no
`.gitignore` desde a limpeza para open source. Por isso a lógica do gate fica
em `ops/deploy-gate/`, que é versionado e testado, e o `deploy.sh` só a chama.
O teste `deploy_sh_test.sh` é o que garante que o `deploy.sh` local continua
chamando o gate.
