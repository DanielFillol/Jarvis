package llm

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/text"
)

// ImageAttachment holds a raw image downloaded from Slack for vision API calls.
type ImageAttachment struct {
	MimeType string // e.g. "image/jpeg"
	Name     string
	Data     []byte
}

// DataURL returns the base64-encoded data URL for the image.
func (a ImageAttachment) DataURL() string {
	return "data:" + a.MimeType + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
}

// answerWithModel assembles the prompt and calls the Chat API with the
// specified model.  It converts Markdown into Slack Markdown before
// returning the result.
func (c *Client) answerWithModel(companyCtx, question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx string, images []ImageAttachment, model string) (string, error) {
	botName := c.BotName
	if strings.TrimSpace(botName) == "" {
		botName = "Jarvis"
	}
	sqlOnlyCtx := dbCtx != "" && jiraCtx == ""

	contextHint := "Se o contexto não for suficiente, diga o que falta e sugira como achar (JQL/links)."
	if sqlOnlyCtx {
		contextHint = "Se os dados não forem suficientes para responder, informe o usuário e sugira como refinar a pergunta. Nunca mencione JQL — este contexto é exclusivamente de banco de dados SQL."
	}

	systemParts := []string{}
	if strings.TrimSpace(companyCtx) != "" {
		systemParts = append(systemParts,
			"## Contexto da empresa (domínio e vocabulário):\n"+strings.TrimSpace(companyCtx),
			"",
		)
	}
	systemParts = append(systemParts,
		"Você é o "+botName+", assistente do Slack.",
		"Responda em português brasileiro, direto, sem enrolação, usando o contexto quando existir.",
		contextHint,
		"Não invente fatos.",
		"Quando a pergunta for ambígua ou faltar informação essencial para uma boa resposta, prefira fazer uma pergunta de esclarecimento direta ao usuário em vez de adivinhar ou dar uma resposta genérica.",
		"MENÇÕES DE USUÁRIOS SLACK: Ao mencionar um usuário pelo ID (ex: U067UM4LRGB), SEMPRE use o formato de mention <@USERID> (ex: <@U067UM4LRGB>). O Slack renderiza automaticamente como o nome de exibição. NUNCA escreva @U067UM4LRGB ou o ID puro — use sempre <@ID>.",
		"CAPACIDADES: Você consegue ler e resumir threads do Slack quando o usuário fornece um link direto (permalink). Ao receber um link como https://empresa.slack.com/archives/CHANID/pTIMESTAMP, você recupera e resume o conteúdo da thread automaticamente. Nunca diga que não consegue acessar links de thread do Slack.",
		"",
		"FORMATAÇÃO — use Slack mrkdwn com variedade visual. Recursos disponíveis e quando usar cada um:",
		"",
		"*Texto:*",
		"- *negrito* (asterisco simples) → títulos de seção, termos-chave, nomes de projetos. NUNCA use **duplo**.",
		"- _itálico_ → datas, nomes, valores secundários, ênfase suave.",
		"- ~tachado~ → itens cancelados, versões antigas, coisas descartadas.",
		"",
		"*Código e comandos:*",
		"- `código inline` → issue keys (PROJ-123), nomes de campos, comandos curtos, valores exatos, JQL de uma linha.",
		"- ```bloco de código``` → JQL longo, JSON, código-fonte, saídas de terminal, queries multi-linha. Use ``` em linha separada.",
		"",
		"*Estrutura:*",
		"- > blockquote → notas importantes, avisos, dicas, callouts de atenção. Uma ou mais linhas com > no início.",
		"- • ou - → listas sem ordem definida. Sub-itens com dois espaços de indentação.",
		"- 1. 2. 3. → passos sequenciais, rankings, procedimentos passo a passo.",
		"- NÃO use # ## ### → use *Título* ou *Título:* em linha própria.",
		"- NÃO use tabelas Markdown (| col | col |) → Slack não as renderiza. Para dados tabulares use lista ou bloco de código.",
		"",
		"*Princípio geral:* varie os estilos conforme o conteúdo. Respostas longas com múltiplas seções ficam melhor com títulos em negrito. Código e JQL sempre em bloco. Notas críticas em blockquote. Não use sempre o mesmo padrão — leia o que foi perguntado e escolha o formato que torna a resposta mais fácil de ler.",
		"",
		"LIMITAÇÃO IMPORTANTE: Você não consegue enviar arquivos, anexos ou downloads no Slack. Quando o usuário pedir dados em CSV, Excel ou qualquer outro formato de arquivo para download, informe claramente que essa funcionalidade não está disponível no momento e ofereça apresentar os dados diretamente na mensagem (tabela em bloco de código, lista, etc.).",
	)
	if strings.TrimSpace(c.JiraBaseURL) != "" {
		baseURL := strings.TrimRight(strings.TrimSpace(c.JiraBaseURL), "/")
		systemParts = append(systemParts, "",
			"IMPORTANTE - Links do Jira:",
			"- Quando precisar gerar um link completo de uma issue Jira, use SEMPRE este base URL: "+baseURL,
			"- Formato: "+baseURL+"/browse/KEY (ex: "+baseURL+"/browse/PROJ-123)",
			"- NUNCA use outros domínios além do base URL fornecido acima.")
	}
	if sqlOnlyCtx {
		systemParts = append(systemParts, "",
			"CONTEXTO: Esta pergunta é respondida com dados de banco de dados SQL.",
			"- NÃO mencione JQL em nenhuma hipótese — JQL é exclusivo para perguntas sobre o Jira.",
			"- NÃO oriente o usuário a usar o Jira ou a fazer buscas no Jira.",
			"- Se os dados forem insuficientes, pergunte ao usuário como refinar a consulta.")
	}
	// When the Jira context contains a sentinel (error or empty result), reinforce
	// the anti-hallucination instruction so the LLM never invents issue data.
	if strings.Contains(jiraCtx, "[JIRA_ERROR:") || strings.Contains(jiraCtx, "[JIRA_EMPTY:") {
		systemParts = append(systemParts, "",
			"DADOS JIRA AUSENTES: A busca no Jira falhou ou não retornou resultados.",
			"- NÃO invente issues, títulos, assignees, chaves (PROJ-NNN) ou links.",
			"- Use apenas o que está no CONTEXTO DO JIRA acima.",
			"- Se não houver dados, informe o usuário claramente e sugira refinar a busca.")
	}
	// Inform the LLM which optional integrations are active so it responds
	// honestly when users ask about capabilities that are not configured.
	if !c.JiraEnabled {
		systemParts = append(systemParts, "",
			"JIRA NÃO CONFIGURADO: A integração com Jira não está habilitada nesta instalação. Se o usuário pedir algo relacionado a Jira (criar card, buscar issue, roadmap etc.), informe gentilmente que essa integração não está disponível e sugira que o administrador configure as variáveis JIRA_BASE_URL, JIRA_EMAIL e JIRA_API_TOKEN.")
	}
	if !c.MetabaseEnabled {
		systemParts = append(systemParts, "",
			"METABASE NÃO CONFIGURADO: A integração com Metabase (banco de dados) não está habilitada nesta instalação. Se o usuário pedir consultas de dados, métricas ou relatórios que requeiram SQL, informe gentilmente que essa integração não está disponível e sugira que o administrador configure as variáveis METABASE_BASE_URL e METABASE_API_KEY.")
	}
	if c.MetabaseEnabled && strings.TrimSpace(dbCtx) == "" {
		systemParts = append(systemParts, "",
			"DADOS DE BANCO AUSENTES: Nenhum resultado de consulta SQL está disponível para esta resposta.",
			"- Se a pergunta requer dados do banco de dados, informe o usuário que não foi possível buscar os dados.",
			"- NÃO invente, estime ou fabrique registros, nomes, status, valores ou qualquer dado operacional.",
			"- Responda apenas com o que está no contexto da thread ou nos outros contextos fornecidos.")
	}
	if !c.OutlineEnabled {
		systemParts = append(systemParts, "",
			"OUTLINE NÃO CONFIGURADO: A integração com o Outline Wiki não está habilitada nesta instalação. Se o usuário pedir documentação interna, processos ou guias que provavelmente estão na wiki, informe gentilmente que essa integração não está disponível e sugira que o administrador configure as variáveis OUTLINE_BASE_URL e OUTLINE_API_KEY.")
	}
	if !c.GoogleDriveEnabled {
		systemParts = append(systemParts, "",
			"GOOGLE DRIVE NÃO CONFIGURADO: A integração com o Google Drive não está habilitada nesta instalação. Se o usuário pedir arquivos, documentos ou planilhas do Drive, informe gentilmente que essa integração não está disponível e sugira que o administrador configure as variáveis GOOGLE_DRIVE_CREDENTIALS_JSON ou GOOGLE_DRIVE_CREDENTIALS_PATH.")
	}
	if !c.HubSpotEnabled {
		systemParts = append(systemParts, "",
			"HUBSPOT NÃO CONFIGURADO: A integração com o HubSpot CRM não está habilitada nesta instalação. Se o usuário pedir dados de CRM (contatos, empresas, deals, tickets, clientes), informe gentilmente que essa integração não está disponível e sugira que o administrador configure a variável HUBSPOT_API_KEY.")
	}
	system := strings.Join(systemParts, "\n")
	var u strings.Builder
	if threadHistory != "" {
		u.WriteString("CONTEXTO DO THREAD:\n")
		u.WriteString(threadHistory)
		u.WriteString("\n\n")
	}
	if slackCtx != "" {
		u.WriteString("CONTEXTO DO SLACK (busca):\n")
		u.WriteString(slackCtx)
		u.WriteString("\n\n")
	}
	if jiraCtx != "" {
		u.WriteString("CONTEXTO DO JIRA:\n")
		u.WriteString(jiraCtx)
		u.WriteString("\n\n")
	}
	if dbCtx != "" {
		u.WriteString("DADOS DO BANCO DE DADOS (resultado de query SQL):\n")
		u.WriteString(dbCtx)
		u.WriteString("\n\n")
	}
	if fileCtx != "" {
		u.WriteString("ARQUIVOS ANEXADOS:\n")
		u.WriteString(fileCtx)
		u.WriteString("\n\n")
	}
	if outlineCtx != "" {
		u.WriteString("DOCUMENTAÇÃO INTERNA (Outline Wiki):\n")
		u.WriteString(outlineCtx)
		u.WriteString("\n\n")
	}
	if googleDriveCtx != "" {
		u.WriteString("DOCUMENTOS DO GOOGLE DRIVE:\n")
		u.WriteString(googleDriveCtx)
		u.WriteString("\n\n")
	}
	if hubspotCtx != "" {
		u.WriteString("CONTEXTO DO HUBSPOT CRM:\n")
		u.WriteString(hubspotCtx)
		u.WriteString("\n\n")
	}
	u.WriteString("PERGUNTA:\n")
	u.WriteString(question)
	u.WriteString("\n\n")
	u.WriteString("Use formatação variada e contextual. Issue keys sempre em `código inline` (ex: `PROJ-123`). JQL em bloco de código. Passos numerados quando relevante. Blockquotes para avisos ou notas importantes.")
	var userMsg OpenAIMessage
	if len(images) > 0 {
		// Vision message: text + images as content parts array.
		parts := []ContentPart{{Type: "text", Text: u.String()}}
		for _, img := range images {
			parts = append(parts, ContentPart{
				Type:     "image_url",
				ImageURL: &ImageURLPart{URL: img.DataURL(), Detail: "auto"},
			})
		}
		userMsg = OpenAIMessage{Role: "user", ContentParts: parts}
	} else {
		userMsg = OpenAIMessage{Role: "user", Content: u.String()}
	}
	msgs := []OpenAIMessage{
		{Role: "system", Content: system},
		userMsg,
	}
	out, err := c.Chat(msgs, model, 0.7, 20000)
	if err != nil {
		return "", err
	}
	// Convert Markdown to Slack Markdown
	out = text.MarkdownToMarkdown(strings.TrimSpace(out))
	return out, nil
}

// AnswerWithRetry generates an answer using primaryModel, retrying on transient
// failures, then falls back to lesserModel when configured and different.
// This makes answer generation resilient to flaky networking, 429s, and 5xxs.
func (c *Client) AnswerWithRetry(
	companyCtx,
	question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx string,
	images []ImageAttachment,
	primaryModel, lesserModel string,
	maxAttempts int,
	baseDelay time.Duration,
) (string, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if baseDelay <= 0 {
		baseDelay = 400 * time.Millisecond
	}

	// Try primary first.
	out, err := c.answerWithRetrySingleModel(companyCtx, question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx, images, primaryModel, maxAttempts, baseDelay)
	if err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}

	// Fall back to the lesser model if configured and different from the primary.
	if lesserModel != "" && lesserModel != primaryModel {
		out2, err2 := c.answerWithRetrySingleModel(companyCtx, question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx, images, lesserModel, maxAttempts, baseDelay)
		if err2 == nil && strings.TrimSpace(out2) != "" {
			return out2, nil
		}
		if err2 != nil {
			return "", err2
		}
		return "", errors.New("empty content from openai (lesser model fallback)")
	}

	if err != nil {
		return "", err
	}
	return "", errors.New("empty content from openai")
}

func (c *Client) answerWithRetrySingleModel(
	companyCtx,
	question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx string,
	images []ImageAttachment,
	model string,
	maxAttempts int,
	baseDelay time.Duration,
) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.answerWithModel(companyCtx, question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx, images, model)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("empty content from openai")
		}

		// backoff with jitter
		if attempt < maxAttempts {
			sleep := backoffWithJitter(baseDelay, attempt)
			time.Sleep(sleep)
		}
	}
	return "", lastErr
}
