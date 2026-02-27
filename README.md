# Jarvis 🤖

Jarvis é um bot em Go que conecta **Slack + Jira + Metabase + LLM** para transformar mensagens em ações úteis e respostas contextualizadas.

---

## O que é o Jarvis

Jarvis é um copiloto operacional para times de produto e engenharia dentro do Slack. Ele responde perguntas em linguagem natural consultando o Jira, o histórico do Slack e os bancos de dados via Metabase em tempo real, cria cards no Jira direto pelo chat, lê e analisa arquivos anexados (PDFs, planilhas, documentos e imagens via API de visão) e mantém todas as respostas em thread para não poluir os canais.

---

## ✨ Funcionalidades

- Responde perguntas sempre em **thread**, usando contexto do Slack + Jira + Metabase + LLM
- **Consultas analíticas ao banco de dados** via Metabase: gera SQL automaticamente e retorna os dados formatados
- Busca de mensagens no Slack com filtros avançados (`from:`, `in:`, `after:`, `before:`)
- Leitura e análise de arquivos anexados: **PDF, DOCX, XLSX, TXT, JSON, imagens** (vision API)
- Consulta o Jira para roadmaps, bugs abertos, issues por sprint/assignee/status
- Criação de cards Jira via linguagem natural (simples, múltiplos, baseado em thread)
- **Apresentação dinâmica**: ao perguntar "o que você faz?", o bot gera uma introdução personalizada com os projetos, canais e capacidades reais do ambiente
- Suporte a **modelo primário + fallback** com retry automático para erros transientes
- **Cascata de exclusão**: exclui a resposta do bot quando o usuário apaga a mensagem original
- Funciona via **menção direta** (`@Jarvis`) ou **DMs** sem necessidade de prefixo
- Resolução automática de mentions Slack (`<@USERID>`) para busca correta por autor

---

## 🏗 Arquitetura

```
Slack Events API
      ↓
HTTP Handler (verifica assinatura HMAC-SHA256)
      ↓
Jarvis Service
      ↓ (roteamento via LLM)
 ┌──────────────┬──────────────┬──────────────┬──────────────┐
 │ Slack Search │ Jira Client  │ Metabase     │ File Parser  │
 │ (mensagens)  │ (JQL/issues) │ (SQL + dados)│ PDF/DOCX/    │
 │              │              │              │ XLSX/imagens │
 └──────────────┴──────────────┴──────────────┴──────────────┘
      ↓
     LLM (primary + fallback)
      ↓
 Resposta em thread no Slack
```

---

## ⚙️ Variáveis de ambiente

Crie um `.env` baseado no `Example.env`:

| Variável | Descrição | Padrão |
|---|---|---|
| `PORT` | Porta HTTP do servidor | `8080` |
| `SLACK_SIGNING_SECRET` | Signing secret do app Slack | — |
| `SLACK_BOT_TOKEN` | Token do bot (`xoxb-`) | — |
| `SLACK_USER_TOKEN` | Token de usuário (`xoxp-`) para busca e download de arquivos | — |
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
| `METABASE_BASE_URL` | URL base do Metabase (ex: `https://metabase.empresa.com`) | — |
| `METABASE_API_KEY` | API key do Metabase (Admin → Settings → Authentication → API Keys) | — |
| `METABASE_SCHEMA_PATH` | Caminho do arquivo de schema gerado | `./docs/metabase_schema.md` |
| `METABASE_ENV` | Label de ambiente escrito no cabeçalho do schema | `production` |

### JIRA_PROJECT_NAME_MAP

Este campo permite que o bot entenda referências em linguagem natural aos seus projetos.

**Formato:** `nome1:CHAVE1,nome2:CHAVE2`

**Exemplo:**
```
JIRA_PROJECT_NAME_MAP=backend:BE,frontend:FE,infraestrutura:INFRA,mobile:MOB
```

Com isso, o usuário pode dizer `"crie um bug no backend"` e o bot resolverá automaticamente para o projeto `BE`.

---

## 🗄 Integração com Metabase

O Jarvis se conecta ao Metabase para responder perguntas analíticas que exigem dados do banco diretamente.

### Como funciona

No **startup**, o bot:
1. Lista todos os bancos de dados cadastrados no Metabase
2. Busca as tabelas e campos de cada banco via `GET /api/database/:id/metadata`
3. Gera um arquivo Markdown (`./docs/metabase_schema.md`) documentando todo o schema

Quando uma pergunta analítica chega:
1. O roteador LLM identifica que a resposta requer dados do banco (`need_metabase=true`)
2. O LLM lê o schema gerado e escreve o SQL adequado (apenas `SELECT`)
3. A query é executada via `POST /api/dataset` no Metabase
4. O resultado é formatado como tabela e incluído no contexto da resposta final

### Exemplos de uso

```
quantos pedidos foram feitos hoje?
qual a receita total do mês de janeiro?
me mostra os 10 clientes com maior valor de compra
quantos usuários se cadastraram essa semana?
qual o ticket médio por categoria de produto?
```

### Configuração da API key

A autenticação usa exclusivamente **API Key** (sem usuário/senha):

1. No Metabase, acesse **Admin → Settings → Authentication → API Keys**
2. Clique em **Create API Key** e dê um nome (ex: `jarvis-bot`)
3. Copie a chave gerada e configure em `METABASE_API_KEY`

> Requer Metabase versão **0.47 ou superior**.

### Schema gerado

O arquivo `./docs/metabase_schema.md` é regenerado a cada restart e tem o formato:

```markdown
# Documentação de Schema — Metabase

> **Gerado em:** 2026-02-26 10:00:00 UTC
> **Ambiente:** production

## Banco: `production_db` (postgres) · ID 1

### Tabela: `public`.`orders` — _Orders_

| Campo | Tipo | Semântico | Chave | Visibilidade | Descrição |
|-------|------|-----------|-------|--------------|----------|
| `id` | Integer | PK | **PK** | — | — |
| `user_id` | Integer | FK | FK | — | — |
| `total` | Decimal | — | — | — | Valor total do pedido |
```

---

## 🗣 Apresentação do bot

Ao perguntar sobre as capacidades do Jarvis, ele gera uma **apresentação dinâmica e personalizada** com os dados reais do ambiente: projetos Jira disponíveis, canais Slack, modelos de IA em uso e funcionalidades habilitadas.

### Como acionar

Qualquer uma destas frases aciona a apresentação:

```
o que você faz?
se apresente
quais suas funcionalidades?
como você pode me ajudar?
o que você sabe fazer?
me conta sobre você
quem é você?
como funciona?
```

### O que é mostrado

A resposta inclui:
- Exemplos de consultas ao **Jira** usando os projetos reais configurados
- Exemplos de **busca no Slack** com os canais reais do workspace
- Funcionalidades de **criação de cards** (quando habilitada)
- Capacidade de **análise de arquivos** (PDF, DOCX, XLSX, imagens)
- Modelo de IA primário e fallback em uso
- Como chamar o bot (`@Nome` ou prefixo `jarvis:`)

> A apresentação é gerada pelo LLM com dados reais do ambiente. Se a chamada à API falhar, uma mensagem estática de fallback é exibida.

---

## 🔧 Escopos Slack necessários

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
| `files:read` | Baixar arquivos anexados a mensagens para análise pelo LLM |

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
| `files:read` | Baixar arquivos anexados a mensagens para análise pelo LLM |

> **Notas:**
> - `users:read` é necessário para filtrar mensagens por autor quando o usuário menciona alguém com `<@USERID>`. Sem ele, a busca `from:` não consegue resolver o ID para o username.
> - `files:read` é necessário em ambos os tokens (bot e user) para que o Jarvis consiga baixar arquivos privados anexados às mensagens.

Após adicionar os escopos, clique em **Reinstall App** para aplicar as permissões.

---

## 📎 Formatos de arquivo suportados

| Formato | Extensões | Como é processado |
|---|---|---|
| PDF | `.pdf` | Extração de texto via biblioteca nativa |
| Word | `.docx` | Extração de texto dos parágrafos do documento |
| Excel | `.xlsx` | Leitura de células de todas as abas da planilha |
| Texto | `.txt`, `.csv`, `.json`, `.xml`, `.log`, `.md` | Lido diretamente como UTF-8 |
| Imagens | `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp` | Descrição via vision API (multimodal) |

> Arquivos acima de 20 MB são ignorados. Apenas o bot token e o user token com escopo `files:read` podem baixar arquivos privados.

---

## 💬 Como usar

### Perguntas sobre o Jira

```
roadmap do projeto BACKEND
quais bugs ainda estão abertos?
me liste os bugs do projeto OPS
qual o status da PROJ-42?
o que está no sprint atual do time de frontend?
```

### Busca no Slack

```
me acha uma thread que fale sobre integração de pagamentos
o que o @fulano falou essa semana no #prod-geral?
buscar menções a 'compliance' nos últimos 30 dias
qual foi a decisão sobre a migração de banco?
```

### Consultas ao banco de dados (Metabase)

```
quantos pedidos foram feitos hoje?
qual a receita total do mês passado?
me mostra os 10 clientes com maior valor de compra
quantos usuários novos se cadastraram essa semana?
qual o ticket médio por categoria de produto?
```

### Análise de arquivos

```
[anexar PDF] analise este relatório e me dê um resumo
[anexar planilha] o que está nessa aba de métricas?
[anexar imagem] descreva o que aparece nessa screenshot
[anexar DOCX] quais são os pontos principais desse documento?
```

### Criação de cards no Jira

```
crie um bug no jira com título "erro ao salvar formulário"
com base nessa thread crie um card no jira
cria 3 cards no BACKEND: 1. Migrar auth | 2. Atualizar docs | 3. Revisar testes
```

### Apresentação do bot

```
o que você faz?
se apresente
quais suas funcionalidades?
```

### Comandos explícitos

| Comando | Descrição |
|---|---|
| `jira criar \| PROJ \| Tipo \| Título \| Descrição` | Cria card com campos explícitos |
| `jira definir \| projeto=PROJ \| tipo=Bug` | Define campos de rascunho pendente |
| `confirmar` | Confirma criação de card pendente |
| `cancelar card` | Descarta rascunho pendente |

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

- Verificação de assinatura HMAC-SHA256 do Slack em todas as requisições
- Tokens sensíveis via variáveis de ambiente (nunca em código)
- Bot ignora mensagens do próprio bot para evitar loops
- Queries ao Metabase são exclusivamente `SELECT` — mutações são bloqueadas no nível do prompt e validadas no código

---

## 📌 Roadmap

- Memória de contexto persistente entre threads
- Respostas com links diretos para threads/issues Jira
- Métricas e observabilidade (traces, latência por etapa)
- Cache inteligente de buscas Slack/Jira
- Suporte a mais formatos de arquivo (PPTX, ODT)

---

## 🧙‍♂️ Filosofia

Jarvis reduz atrito operacional: menos busca manual, mais contexto, decisões mais rápidas.
