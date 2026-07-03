---
name: testing
description: Teste — Skill de Testes com Playwright
user-invocable: false
---

# Teste — Skill de Testes com Playwright

Usa o Playwright (via MCP) para testar a aplicação OpenPoet no navegador.

## !! REGRA CRÍTICA — NUNCA TESTAR EM PRODUÇÃO !!

- **NUNCA** usar a porta **8081** para testes. A porta 8081 é PRODUÇÃO e está em uso ativo.
- **NUNCA** navegar para `http://localhost:8081` durante testes.
- **NUNCA** matar ou interferir com o processo na porta 8081.
- Testes devem ser feitos **SEMPRE** na porta **8080** (desenvolvimento).
- Se o usuário passar `8081` como argumento, **RECUSAR** e explicar que é produção.

## Iniciar o servidor de desenvolvimento

Antes de testar, garantir que o OpenPoet está rodando na porta 8080. Para isso:

1. **Compilar** a aplicação:
   ```bash
   make build
   ```

2. **Iniciar** o servidor de desenvolvimento com bind em `0.0.0.0`:
   ```bash
   ./build/openpoet -port 8080 -bind 0.0.0.0
   ```

   > **IMPORTANTE:** O host padrão agora é `localhost`. Para que o Playwright consiga acessar, é necessário usar `-bind 0.0.0.0`.

3. Se o servidor já estiver rodando na porta 8080, não é necessário reiniciar — mas verificar que foi iniciado com `-bind 0.0.0.0`.

## URL padrão

- **SEMPRE usar:** `http://localhost:8080` (desenvolvimento)
- Se o usuário fornecer uma URL como argumento (`$ARGUMENTS`), usar essa URL — **exceto se for porta 8081**.

## Como testar

### 1. Navegar para a aplicação

```
browser_navigate → URL da aplicação
```

### 2. Capturar o estado da página

Sempre começar com um **snapshot** para entender a estrutura da página:

```
browser_snapshot → captura a árvore de acessibilidade
```

O snapshot é preferível ao screenshot para interações, pois retorna os `ref` dos elementos.

### 3. Interagir com elementos

Usar os `ref` retornados pelo snapshot para clicar, digitar, etc:

- `browser_click` — clicar em botões, links, tabs
- `browser_type` — digitar em campos de texto
- `browser_press_key` — pressionar teclas (Enter, Escape, Tab, etc)
- `browser_fill_form` — preencher múltiplos campos de formulário
- `browser_select_option` — selecionar opções em dropdowns

### 4. Verificar resultados

- `browser_snapshot` — verificar mudanças no DOM após interações
- `browser_take_screenshot` — capturar imagem visual da página
- `browser_console_messages` — verificar erros no console (level: "error")
- `browser_network_requests` — verificar chamadas de rede e respostas
- `browser_wait_for` — esperar texto aparecer/desaparecer

### 5. Testar responsividade mobile

Para testar a versão mobile:

```
browser_resize → width: 375, height: 812 (iPhone)
```

Depois fazer snapshot e verificar o layout mobile.

## Fluxo padrão de teste

1. **Navegar** para a URL
2. **Snapshot** para entender a página
3. **Verificar console** em busca de erros JavaScript
4. **Interagir** com elementos principais (tabs, botões, inputs)
5. **Screenshot** para documentar o estado visual
6. **Reportar** ao usuário o que foi encontrado

## Regras

- Sempre reportar ao usuário o que está sendo testado e os resultados
- Se encontrar erros no console, listar todos
- Se encontrar problemas visuais, tirar screenshot e descrever
- Ao final, fechar o browser com `browser_close` (a menos que o usuário queira continuar testando)
- Se o browser não estiver instalado e der erro, usar `browser_install` primeiro
