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