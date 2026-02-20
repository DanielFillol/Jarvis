# Jarvis 🤖

Jarvis é um bot em Go que conecta **Slack + Jira + LLM** para transformar mensagens em ações úteis e respostas contextualizadas.

## ✨ O que ele faz

- Responde no Slack sempre em **thread**
- Entende perguntas em linguagem natural
- Busca contexto no histórico do Slack
- Consulta o Jira para:
  - Roadmaps por projeto
  - Bugs abertos
  - Issues recentes
- Cria cards no Jira via linguagem natural
- Resume e entrega respostas acionáveis

Em resumo: um copiloto operacional para times de produto e engenharia dentro do Slack.

---

## 🧠 Exemplos de perguntas

```
roadmap do TPTDR
quais bugs ainda estão abertos?
me liste os bugs do GR
me acha uma thread que fale multilixo
crie um bug no jira para o GR com título "erro no app"
```

---

## 🏗 Arquitetura

```
Slack Events API
      ↓
 Slack Handler
      ↓
   Router (intenção)
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

Crie um `.env` com:

```
PORT=8080

SLACK_SIGNING_SECRET=
SLACK_BOT_TOKEN=
SLACK_USER_TOKEN=

OPENAI_API_KEY=
OPENAI_MODEL=gpt-5.1

JIRA_BASE_URL=
JIRA_EMAIL=
JIRA_API_TOKEN=
JIRA_PROJECT_KEYS=TPTDR,INV,GR
JIRA_CREATE_ENABLED=true
```

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

- Verificação de assinatura do Slack
- Tokens sensíveis via env vars
- Bot ignora mensagens do próprio bot

---

## 📌 Roadmap futuro

- Memória de contexto persistente
- Respostas com links diretos para threads/issues
- Métricas e observabilidade
- Cache inteligente de buscas

---

## 🧙‍♂️ Filosofia

Jarvis reduz atrito operacional: menos busca manual, mais contexto, decisões mais rápidas.
