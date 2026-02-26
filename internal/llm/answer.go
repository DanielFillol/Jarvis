// internal/llm/answer.go
package llm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/text"
)

// IntroContext holds all runtime data used to generate the bot introduction.
type IntroContext struct {
	BotName           string
	PrimaryModel      string
	FallbackModel     string
	JiraBaseURL       string
	JiraProjects      []string // formatted as "KEY — Nome"
	SlackChannels     []string // formatted as "#canal"
	JiraCreateEnabled bool
}

// GenerateIntroduction calls the LLM to produce a rich, contextual
// self-introduction in Slack mrkdwn format using real configuration data.
func (c *Client) GenerateIntroduction(ctx IntroContext, model, fallbackModel string) (string, error) {
	botName := ctx.BotName
	if strings.TrimSpace(botName) == "" {
		botName = "Jarvis"
	}

	// Pick sample values to anchor examples — prefer first 2 distinct keys
	sampleKey := "PROJ"
	sampleKey2 := "PROJ"
	for i, p := range ctx.JiraProjects {
		k := strings.SplitN(p, " — ", 2)[0]
		if i == 0 {
			sampleKey = k
			sampleKey2 = k
		} else if i == 1 {
			sampleKey2 = k
			break
		}
	}

	sampleChan := "geral"
	sampleChan2 := "tech"
	if len(ctx.SlackChannels) > 0 {
		sampleChan = strings.TrimPrefix(ctx.SlackChannels[0], "#")
	}
	if len(ctx.SlackChannels) > 1 {
		sampleChan2 = strings.TrimPrefix(ctx.SlackChannels[1], "#")
	}

	// Create section varies by whether it's enabled
	createBlock := ""
	if ctx.JiraCreateEnabled {
		createBlock = fmt.Sprintf(`:pencil2: *Criar cards*
• _"cria um bug no %s: [título]"_ — direto pelo chat
• _"com base nessa thread, abre uma task no %s"_ — extrai da conversa
• _"cria 3 cards no %s: 1. X | 2. Y | 3. Z"_ — múltiplos de uma vez
• `+"`confirmar`"+` para criar  `+"`cancelar card`"+` para descartar

`, sampleKey, sampleKey2, sampleKey)
	} else {
		createBlock = `:pencil2: *Criar cards* — _desabilitado neste workspace_

`
	}

	chanExamples := ""
	if len(ctx.SlackChannels) > 0 {
		chanExamples = fmt.Sprintf(`• _"o que foi decidido sobre X no #%s?"_
• _"o que @fulano falou esta semana no #%s?"_
• _"me acha a thread sobre [tema]"_
• _"buscar menções a 'compliance' nos últimos 30 dias"_`, sampleChan, sampleChan2)
	} else {
		chanExamples = `• _"o que foi decidido sobre X no #nome-do-canal?"_
• _"o que @fulano falou esta semana?"_
• _"me acha a thread sobre [tema]"_
• _"buscar menções a 'compliance' nos últimos 30 dias"_`
	}

	prompt := fmt.Sprintf(`Você é o %s, assistente do Slack com personalidade, integrado ao Jira e IA.

Sua tarefa: gerar uma apresentação em Slack mrkdwn usando EXATAMENTE o esqueleto abaixo.
Substitua cada [PLACEHOLDER] pelo conteúdo real indicado. Não adicione nem remova seções.

═══ INÍCIO DO ESQUELETO ═══
Oi! Sou o *%s* :wave: — [1 frase direta e com personalidade descrevendo o que você faz]

:mag: *Jira*
[4 exemplos em itálico usando os projetos reais: %s, %s. Cubra roadmap, bugs abertos, issues por sprint/assignee e detalhes de um card específico como %s-42]

:slack: *Slack*
%s

%s:paperclip: *Arquivos*
• _"analise este relatório PDF"_ — lê e interpreta o conteúdo do documento
• _"o que está nessa planilha?"_ — extrai dados de XLSX/DOCX
• _"descreva a imagem anexada"_ — visão de IA para imagens (PNG, JPG, GIF, WEBP)
• Formatos: PDF, DOCX, XLSX, TXT, JSON e imagens

:speech_balloon: *Conversa livre*
[2-3 exemplos de uso livre em itálico: resumir thread, tirar dúvida técnica, analisar arquivo ou redigir texto]

> :robot_face: Modelo: *%s* — fallback: _%s_
> Me chame com _@%s_ ou _jarvis:_ em qualquer canal :rocket:
═══ FIM DO ESQUELETO ═══

REGRAS INVIOLÁVEIS de formatação Slack mrkdwn:
- Negrito *asterisco simples* — NUNCA **duplo asterisco**
- Itálico _sublinhado_
- Emojis :nome_do_emoji: (use os que já estão no esqueleto)
- Comandos inline com `+"`backticks`"+`
- Blockquote com > no início da linha
- SEM # ## ### em nenhuma hipótese
- Máximo 2500 caracteres
- Não escreva a palavra "Título" em nenhum lugar`,
		botName, botName,
		sampleKey, sampleKey2, sampleKey,
		chanExamples,
		createBlock,
		ctx.PrimaryModel, ctx.FallbackModel, botName,
	)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.75, 2000)
	if err != nil && strings.TrimSpace(fallbackModel) != "" && fallbackModel != model {
		out, err = c.Chat(messages, fallbackModel, 0.75, 2000)
	}
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("empty intro from llm")
	}
	// Strip any "Título:" prefixes the LLM may still produce despite instructions.
	out = fixIntroTitles(out)
	return out, nil
}

// fixIntroTitles removes the word "Título:" (and common variants) that some
// LLM responses prepend to section headings.
func fixIntroTitles(s string) string {
	r := strings.NewReplacer(
		"Título: ", "",
		"titulo: ", "",
		"*Título: ", "*",
		"*titulo: ", "*",
	)
	return r.Replace(s)
}

// OpenAIChatRequest and OpenAIChatResponse are exported aliases for the
// internal request and response types, allowing external packages to
// reference them without importing internal symbols directly.
type OpenAIChatRequest = openAIChatRequest
type OpenAIChatResponse = openAIChatResponse

// Answer generates an answer to the user's question using the language
// model.  It passes the thread history and any additional Slack or
// Jira context to the model as part of the prompt.  primaryModel
// designates the preferred model; fallbackModel is used if the
// primary fails or returns an empty response.  The returned answer
// uses Slack Markdown formatting.
func (c *Client) Answer(question, threadHistory, slackCtx, jiraCtx, fileCtx string, images []ImageAttachment, primaryModel, fallbackModel string) (string, error) {
	out, err := c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, fileCtx, images, primaryModel)
	if err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}
	if fallbackModel != "" && fallbackModel != primaryModel {
		out2, err2 := c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, fileCtx, images, fallbackModel)
		if err2 == nil && strings.TrimSpace(out2) != "" {
			return out2, nil
		}
		if err2 != nil {
			return "", err2
		}
		return "", errors.New("empty content from openai (fallback)")
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("empty content from openai")
}

// answerWithModel assembles the prompt and calls the Chat API with the
// specified model.  It converts Markdown into Slack Markdown before
// returning the result.
func (c *Client) answerWithModel(question, threadHistory, slackCtx, jiraCtx, fileCtx string, images []ImageAttachment, model string) (string, error) {
	botName := c.BotName
	if strings.TrimSpace(botName) == "" {
		botName = "Jarvis"
	}
	systemParts := []string{
		"Você é o " + botName + ", assistente do Slack.",
		"Responda em português brasileiro, direto, sem enrolação, usando o contexto quando existir.",
		"Se o contexto não for suficiente, diga o que falta e sugira como achar (JQL/links).",
		"Não invente fatos.",
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
		"",
		"*Princípio geral:* varie os estilos conforme o conteúdo. Respostas longas com múltiplas seções ficam melhor com títulos em negrito. Código e JQL sempre em bloco. Notas críticas em blockquote. Não use sempre o mesmo padrão — leia o que foi perguntado e escolha o formato que torna a resposta mais fácil de ler.",
	}
	if strings.TrimSpace(c.JiraBaseURL) != "" {
		baseURL := strings.TrimRight(strings.TrimSpace(c.JiraBaseURL), "/")
		systemParts = append(systemParts, "",
			"IMPORTANTE - Links do Jira:",
			"- Quando precisar gerar um link completo de uma issue Jira, use SEMPRE este base URL: "+baseURL,
			"- Formato: "+baseURL+"/browse/KEY (ex: "+baseURL+"/browse/PROJ-123)",
			"- NUNCA use outros domínios além do base URL fornecido acima.")
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
	if fileCtx != "" {
		u.WriteString("ARQUIVOS ANEXADOS:\n")
		u.WriteString(fileCtx)
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
	out, err := c.Chat(msgs, model, 0.7, 2500)
	if err != nil {
		return "", err
	}
	// Convert Markdown to Slack Markdown
	out = text.MarkdownToMarkdown(strings.TrimSpace(out))
	return out, nil
}
