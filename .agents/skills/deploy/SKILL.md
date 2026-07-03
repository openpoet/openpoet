---
name: deploy
description: Deploy the OpenPoet application to the .run directory on port 8081. Use when the user asks to deploy, fazer deploy, atualizar o serviço, subir o serviço, restart da produção, or update the running production instance.
user-invocable: false
allowed-tools: Bash(./.scripts/deploy.sh *), Bash(make deploy*), Bash(sleep *)
---

# Deploy Skill

Deploy the OpenPoet service to the `.run/` directory on port 8081 using the deploy script.

## REGRA CRÍTICA — Usar SEMPRE o script `.scripts/deploy.sh`

- **NUNCA** executar os passos de deploy manualmente (kill, build, cp, nohup).
- **SEMPRE** usar o script `.scripts/deploy.sh` que já encapsula toda a lógica de deploy.
- O script se daemoniza automaticamente — sobrevive mesmo se o Claude Code ou o terminal cair.

## Steps

### 1. Executar o deploy

```bash
./.scripts/deploy.sh
```

O script faz tudo automaticamente:

- Mata apenas o processo na porta 8081 (nunca o 8080)
- Verifica que o dev server continua vivo
- Roda `make build`
- Copia o binário para `.run/`
- Inicia o serviço na porta 8081
- Verifica que ambas as portas estão saudáveis

O comando retorna imediatamente — o deploy roda em background.

### 2. Aguardar e verificar o resultado

Esperar alguns segundos para o deploy completar e checar o status:

```bash
sleep 8 && ./.scripts/deploy.sh --status
```

- Se o status for `SUCCESS`, reportar sucesso ao usuário.
- Se o status for `FAILED` ou `RUNNING`, checar o log com `./.scripts/deploy.sh --log` e reportar o erro.

### 3. Em caso de falha — consultar o log

```bash
./.scripts/deploy.sh --log
```

- Mostrar as linhas relevantes do log ao usuário para diagnóstico.
