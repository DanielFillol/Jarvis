# Jarvis 🤖

Jarvis é um bot em Go que conecta **Slack + Jira + LLM** para transformar mensagens em ações úteis e respostas contextualizadas.

## ✨ O que ele faz

- Responde no Slack sempre em **thread**
- Entende perguntas em linguagem natural
- Busca contexto no histórico do Slack
- Consulta o Jira para:
  - Roadmaps por projeto
  - Bugs abertos
  - Issues recentes (por status, tipo, assignee, etc.)
- Cria cards no Jira via linguagem natural
- Resume e entrega respostas acionáveis

Em resumo: um copiloto operacional para times de produto e engenharia dentro do Slack.

---

## 🧠 Exemplos de perguntas

```
roadmap do projeto BACKEND
quais bugs ainda estão abertos?
me liste os bugs do projeto OPS
me acha uma thread que fale sobre integração de pagamentos
crie um bug no jira com título "erro ao salvar formulário"
com base nessa thread crie um card no jira
```

---

## 🏗 Arquitetura

```
Slack Events API
      ↓
 Slack Handler
      ↓
   Router (intenção via LLM)
      ↓
 ┌───────────────┬───────────────┐
 │ Slack Search  │ Jira Client   │
 └───────────────┴───────────────┘
      ↓
       LLM
      ↓
  Resposta em thread
```

---

## ⚙️ Variáveis de ambiente

Crie um `.env` baseado no `Example.env`:

| Variável | Descrição | Padrão |
|---|---|---|
| `PORT` | Porta HTTP do servidor | `8080` |
| `SLACK_SIGNING_SECRET` | Signing secret do app Slack | — |
| `SLACK_BOT_TOKEN` | Token do bot (`xoxb-`) | — |
| `SLACK_USER_TOKEN` | Token de usuário (`xoxp-`) para busca | — |
| `SLACK_SEARCH_MAX_PAGES` | Máximo de páginas na busca Slack | `10` |
| `OPENAI_API_KEY` | Chave da API OpenAI | — |
| `OPENAI_MODEL` | Modelo primário | `gpt-4o-mini` |
| `OPENAI_FALLBACK_MODEL` | Modelo de fallback (opcional) | — |
| `JIRA_BASE_URL` | URL base do Jira (ex: `https://empresa.atlassian.net`) | — |
| `JIRA_EMAIL` | E-mail da conta Jira | — |
| `JIRA_API_TOKEN` | API token do Jira | — |
| `JIRA_PROJECT_KEYS` | Chaves dos projetos Jira (CSV) para buscas padrão | — |
| `JIRA_PROJECT_NAME_MAP` | Mapeamento nome→chave para linguagem natural (ex: `backend:BE,ops:OPS`) | — |
| `JIRA_CREATE_ENABLED` | Habilita criação de issues via bot | `false` |
| `BOT_NAME` | Nome do bot exibido nas mensagens | `Jarvis` |

### JIRA_PROJECT_NAME_MAP

Este campo permite que o bot entenda referências em linguagem natural aos seus projetos.

**Formato:** `nome1:CHAVE1,nome2:CHAVE2`

**Exemplo:**
```
JIRA_PROJECT_NAME_MAP=backend:BE,frontend:FE,infraestrutura:INFRA,mobile:MOB
```

Com isso, o usuário pode dizer `"crie um bug no backend"` e o bot resolverá automaticamente para o projeto `BE`.

---

## 🔧 Configuração do App Slack

Acesse [api.slack.com/apps](https://api.slack.com/apps), selecione seu app e vá em **OAuth & Permissions**.

### Bot Token Scopes

Escopos necessários para o token do bot (`xoxb-`):

| OAuth Scope | Descrição |
|---|---|
| `channels:history` | Ver mensagens em canais públicos em que o Jarvis foi adicionado |
| `channels:read` | Ver informações básicas sobre canais públicos |
| `chat:write` | Enviar mensagens como @Jarvis |
| `groups:history` | Ver mensagens em canais privados em que o Jarvis foi adicionado |
| `im:history` | Ver mensagens em DMs em que o Jarvis foi adicionado |
| `links.embed:write` | Embedar URLs de vídeo em mensagens e app surfaces |
| `links:read` | Ver URLs em mensagens |
| `links:write` | Exibir previews de URLs em mensagens |
| `mpim:history` | Ver mensagens em group DMs em que o Jarvis foi adicionado |

### User Token Scopes

Escopos necessários para o token de usuário (`xoxp-`), usado para buscas com contexto mais amplo:

| OAuth Scope | Descrição |
|---|---|
| `channels:history` | Ver mensagens em canais públicos do usuário |
| `channels:read` | Ver informações básicas sobre canais públicos |
| `chat:write` | Enviar mensagens em nome do usuário |
| `groups:history` | Ver mensagens em canais privados do usuário |
| `im:history` | Ver mensagens em DMs do usuário |
| `links.embed:write` | Embedar URLs de vídeo em mensagens e app surfaces |
| `links:read` | Ver URLs em mensagens |
| `links:write` | Exibir previews de URLs em mensagens |
| `mpim:history` | Ver mensagens em group DMs do usuário |
| `search:read` | Buscar conteúdo no workspace |
| `search:read.files` | Buscar arquivos no workspace |
| `search:read.private` | Buscar conteúdo privado no workspace |
| `search:read.public` | Buscar conteúdo público no workspace |
| `users:read` | Ver pessoas no workspace (necessário para resolver `<@USERID>` → username em buscas `from:`) |

> **Nota:** O scope `users:read` no User Token é necessário para que o bot consiga filtrar mensagens por autor quando o usuário menciona alguém com `<@USERID>`. Sem ele, a busca `from:` não consegue resolver o ID para o username e opera de forma mais ampla.

Após adicionar os escopos, clique em **Reinstall App** para aplicar as permissões.

---

## ▶️ Executar

```bash
go run cmd/jarvis/main.go
```

---

## 🧪 Testes

```bash
go test ./...
```

---

## 🔒 Segurança

- Verificação de assinatura HMAC-SHA256 do Slack
- Tokens sensíveis via env vars
- Bot ignora mensagens do próprio bot

---

## 💬 Comandos suportados

| Comando | Descrição |
|---|---|
| `jira criar \| PROJ \| Tipo \| Título \| Descrição` | Cria card com campos explícitos |
| `crie um card no jira...` | Cria card por linguagem natural |
| `com base nessa thread crie um card` | Extrai card do contexto da thread |
| `jira definir \| projeto=PROJ \| tipo=Bug` | Define campos de rascunho pendente |
| `confirmar` | Confirma criação de card pendente |
| `cancelar card` | Descarta rascunho pendente |

---

## 📌 Roadmap futuro

- Memória de contexto persistente
- Respostas com links diretos para threads/issues
- Métricas e observabilidade
- Cache inteligente de buscas

---

## 🧙‍♂️ Filosofia

Jarvis reduz atrito operacional: menos busca manual, mais contexto, decisões mais rápidas.