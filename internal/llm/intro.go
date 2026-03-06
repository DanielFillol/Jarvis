package llm

import (
	"fmt"
	"log"
	"strings"
)

// GenerateIntroMessage asks the LLM to write a Slack mrkdwn presentation of
// the bot's capabilities.  featuresDesc describes which integrations are active
// (built by the caller from env flags).  docsContext contains excerpts from
// generated docs (jira_projects.md, metabase schema, etc.) so the LLM can
// create realistic examples using real project/table names.
// Falls back to fallback on any error.
func (c *Client) GenerateIntroMessage(botName, featuresDesc, docsContext, model, fallback string) string {
	if botName == "" {
		botName = "Jarvis"
	}

	docsSection := ""
	if strings.TrimSpace(docsContext) != "" {
		docsSection = fmt.Sprintf("\n\nCONTEXTO DOS SISTEMAS CONFIGURADOS (use para criar exemplos realistas):\n%s", clip(docsContext, 6000))
	}

	bt := "`"
	tpl := "" +
		"Oi! Sou o *" + botName + "*, seu assistente operacional no Slack. 👋\n\n" +
		"Aqui está o que posso fazer por você:\n\n\n" +
		"*Título da seção* emoji\n" +
		"• " + bt + `"frase de exemplo"` + bt + " — descrição breve do que faz\n" +
		"• " + bt + `"outra frase de exemplo"` + bt + " — outra descrição\n" +
		"• " + bt + `"mais um exemplo"` + bt + " — mais uma descrição\n\n\n" +
		"*Próxima seção* emoji\n" +
		"• " + bt + `"frase de exemplo"` + bt + " — descrição breve\n" +
		"• " + bt + `"outra frase"` + bt + " — descrição\n\n\n" +
		"*Como me chamar:*\n" +
		"• Mencione *@" + botName + "* em qualquer canal\n" +
		"• Em conversas diretas, basta enviar a mensagem diretamente\n\n" +
		"Pode perguntar à vontade! 🚀"

	rules := "" +
		"REGRAS:\n" +
		"- Cada bullet DEVE seguir o padrão: • " + bt + `"frase de exemplo"` + bt + " — descrição breve\n" +
		"- A frase de exemplo vem PRIMEIRO (entre backticks com aspas), depois \" — \" e a descrição\n" +
		"- As frases de exemplo DEVEM estar entre aspas dentro dos backticks: " + bt + `"assim"` + bt + "\n" +
		"- Duas linhas em branco entre seções (linha vazia + linha vazia)\n" +
		"- Linguagem de leigo — sem \"JQL\", sem \"assignee\", sem \"issues\", sem siglas técnicas\n" +
		"- Use os nomes reais dos projetos e entidades que aparecem no CONTEXTO acima\n" +
		"- Não invente nomes que não estejam no contexto\n" +
		"- Português brasileiro\n" +
		"- 4 a 6 bullets por seção\n" +
		"- NÃO inclua blocos de código markdown (sem ```)\n" +
		"- NÃO use emojis no meio das frases de exemplo"

	prompt := fmt.Sprintf(
		"Você é o *%s*, um assistente operacional no Slack.\n"+
			"Escreva uma mensagem de apresentação para os usuários explicando o que você pode fazer.\n\n"+
			"FUNCIONALIDADES ATIVAS:\n%s%s\n\n"+
			"FORMATO OBRIGATÓRIO — reproduza exatamente este estilo (substitua o conteúdo, mantenha o formato):\n\n"+
			"%s\n\n"+
			"%s",
		botName, featuresDesc, docsSection, tpl, rules,
	)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.3, 1500)
	if err != nil {
		log.Printf("[LLM] GenerateIntroMessage error: %v — using fallback", err)
		return fallback
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return fallback
	}
	log.Printf("[LLM] GenerateIntroMessage generated len=%d", len(out))
	return out
}
