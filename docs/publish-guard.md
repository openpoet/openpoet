# Política de publicação — o OpenPoet é open source

O repositório `openpoet/openpoet` é público. Tudo que entra num commit pode
chegar ao GitHub, e o que chega ao GitHub deve ser tratado como publicado para
sempre: forks, clones, caches e indexadores guardam cópias mesmo depois de
reescrever o histórico.

## As regras

1. **Nada comprometedor entra em commit nem chega ao remoto:** segredos,
   tokens, chaves e senhas; IPs e hosts internos; dados pessoais (caminhos com
   o usuário da máquina, nomes de clientes e de projetos de clientes, e-mails
   que não sejam do projeto); imagens e capturas de debug; documentação
   interna (relatórios de incidente, avaliações, IDs de sessões e tasks
   internas); dumps, logs, bancos `.db`, arquivos `.env`, binários.
   Fixtures de teste usam valores neutros: `example.com`, `192.0.2.x`
   (TEST-NET), `/home/dev`, `C:\Users\dev`.
2. **Antes de cada commit, revise o diff** (`git diff --staged`) com esse
   critério. O hook de pre-commit roda o guard (abaixo), mas ele não substitui
   a leitura.
3. **Antes de cada push, revise todos os commits que ainda não estão no
   remoto** (`git log -p origin/main..main`) e rode o scanner
   (`make publish-guard`). Se aparecer algo, **reescreva o histórico local**
   antes de empurrar (`git commit --amend`, `git rebase -i` ou
   `git filter-repo`, sempre com um backup antes, por exemplo
   `git bundle create <fora-do-repo>/backup.bundle --all`). Nunca empurre para
   "corrigir depois".
4. **O que está no `.gitignore` continua no `.gitignore`.** `.scripts/`,
   `CLAUDE.md`, `.claude/`, `.run/` e os bancos locais não viram arquivos
   versionados, nem com `git add -f`.
5. **Segredo que chegou ao remoto exige rotação.** Reescrever o histórico não
   basta, porque ele já pode ter sido copiado. Revogue e troque a credencial e
   só depois limpe o histórico (com `--force-with-lease`, avisando antes sobre
   o impacto em forks e clones).

## O guard automático

`ops/publish-guard/guard.sh` roda nos hooks versionados em `.githooks/`
(ativados por `make setup`, que faz `git config core.hooksPath .githooks`):

| Hook / comando | O que verifica |
|---|---|
| `pre-commit` | as mudanças staged |
| `pre-push` | cada commit que ainda não está no remoto, inclusive de branch nova |
| `make publish-guard` | os commits de `HEAD` que não estão em `origin` (com fetch) |
| `make secret-scan` | o histórico inteiro, todas as refs |

Verificações:

- **Segredos:** o [gitleaks](https://github.com/gitleaks/gitleaks) v8.30.1
  (licença MIT), instalado com `make tools`. Sem gitleaks o guard **falha
  fechado**. Falsos positivos já revisados ficam em `.gitleaksignore`, por
  fingerprint. Só acrescente um item depois de revisar o achado.
- **Conteúdo das linhas adicionadas** (e das mensagens de commit, nos modos
  de range e push): IPv4 privado (10/8, 172.16/12, 192.168/16); o home e o
  usuário da máquina atual (`$HOME`, `/home/<usuário>`, `/Users/<usuário>`,
  `\Users\<usuário>`); e o **denylist local**.
- **Arquivos:** `.env*` (exceto `.env.example`/`.sample`/`.template`), chaves e
  certificados, bancos, logs, dumps/HAR/pcap, arquivos de token, backups e
  SQL. Imagens, vídeos e PDFs só em `docs/images/` e `web/static/`. Binário
  acima de 512 KiB ou qualquer arquivo acima de 5 MiB é recusado.

### Denylist local (nunca versionado)

Nomes de clientes e domínios internos não podem estar num arquivo versionado
de um repositório público. O guard os lê de:

- `.git/info/publish-denylist` (por clone), e
- `~/.config/openpoet/publish-denylist` (por máquina).

Cada linha é uma regex estendida, comparada sem diferenciar maiúsculas de
minúsculas. Linhas vazias ou começando com `#` são ignoradas.

### Exceções

- Uma linha com o marcador `publish-guard:allow` é ignorada pelas regras de
  conteúdo. Ele fica visível no diff, então use só em fixtures deliberadas e
  justifique na revisão.
- `git commit --no-verify` / `git push --no-verify` pulam os hooks. **Agentes
  nunca usam isso sem ordem expressa do usuário** para aquele commit ou push.
  Mesmo com a ordem, rode `make publish-guard` e relate o resultado.

## Testes

`make test-publish-guard`, que também roda dentro de `go test ./...` via
`ops/publish-guard/guard_test.go`, exercita o guard e os hooks com
repositórios descartáveis e um remoto bare local. Cobre:

- conteúdo: IP privado (loopback passa), home da máquina, denylist sem
  diferenciar maiúsculas, marcador de exceção;
- arquivos: `.env` (com `.env.example` permitido), log, banco, imagem fora e
  dentro de `docs/images/`, binário grande, token detectado pelo gitleaks;
- falha fechada sem gitleaks;
- hooks: pre-commit bloqueando, mensagem de commit verificada, pre-push
  bloqueando sem que nada chegue ao remoto, push liberado depois de reescrever
  o commit local, e branch nova verificada.
