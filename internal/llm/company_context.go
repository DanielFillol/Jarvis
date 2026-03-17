package llm

import (
	"fmt"
	"log"
	"strings"
)

// GenerateCompanyContext asks the LLM to synthesize a compact domain glossary
// from the provided Jira, Metabase, and Outline documentation.
// The output is a short (~1200 chars) Markdown reference in Portuguese that
// will be injected into every answer call.  Returns "" on error.
func (c *Client) GenerateCompanyContext(jiraDoc, metabaseDoc, outlineDocs, hubspotDoc, model string) string {
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}

	var parts []string
	if strings.TrimSpace(jiraDoc) != "" {
		parts = append(parts, "### Projetos Jira\n"+clip(jiraDoc, 6000))
	}
	if strings.TrimSpace(metabaseDoc) != "" {
		parts = append(parts, "### Schema Metabase\n"+clip(metabaseDoc, 6000))
	}
	if strings.TrimSpace(outlineDocs) != "" {
		parts = append(parts, "### Documentação Outline\n"+clip(outlineDocs, 6000))
	}
	if strings.TrimSpace(hubspotDoc) != "" {
		parts = append(parts, "### Pipelines HubSpot\n"+clip(hubspotDoc, 4000))
	}
	if len(parts) == 0 {
		return ""
	}
	sources := strings.Join(parts, "\n\n")

	prompt := fmt.Sprintf(`Você é um assistente que analisa documentação interna de uma empresa e produz um glossário compacto de domínio.

Com base nas fontes abaixo, produza um contexto de domínio em português brasileiro com as seguintes seções (inclua somente as que tiver dados suficientes):

## Entidades & Vocabulário
- <termo>: <o que significa no contexto da empresa, em 1 linha>

## Projetos
- <CHAVE>: <propósito do projeto em 1 linha>

## Sistemas de Dados
- <banco ou sistema>: <o que armazena, principais entidades>

REGRAS:
- Não invente nada — use apenas o que está nas fontes.
- Omita termos genéricos ou óbvios (ex: "usuário", "data", "id").
- Foque em termos específicos do negócio da empresa (entidades de domínio, processos, siglas internas).
- Seja extremamente conciso: máximo 1500 caracteres no total.
- Não inclua seções vazias.
- Saída em português brasileiro.

FONTES:
%s`, sources)

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0.2, 600)
	if err != nil {
		log.Printf("[LLM] GenerateCompanyContext error: %v", err)
		return ""
	}
	out = strings.TrimSpace(out)
	log.Printf("[LLM] GenerateCompanyContext generated len=%d", len(out))
	return out
}
