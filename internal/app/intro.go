package app

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// introOptions carries the feature-gate flags used to tailor the intro message.
type introOptions struct {
	jiraEnabled        bool
	jiraCreateEnabled  bool
	jiraProjectKeys    []string
	jiraKeyToName      map[string]string
	metabaseEnabled    bool
	csvEnabled         bool
	outlineEnabled     bool
	slackSearchEnabled bool
}

// isIntroRequest returns true when the message is asking the bot to introduce
// itself or list its capabilities.  Uses keyword matching — no LLM call.
func isIntroRequest(question string) bool {
	q := strings.ToLower(question)
	q = strings.NewReplacer("?", "", "!", "", ".", "", "-", " ", "_", " ").Replace(q)
	triggers := []string{
		"se apresente", "apresente se", "apresenta se", "apresenta te",
		"se apresenta", "quem é você", "quem e voce", "quem é voce",
		"o que você faz", "o que voce faz", "o que você pode fazer", "o que voce pode fazer",
		"o que sabe fazer", "o que consegue fazer",
		"quais são suas funcionalidades", "quais sao suas funcionalidades",
		"quais são suas capacidades", "quais sao suas capacidades",
		"quais são suas habilidades", "quais sao suas habilidades",
		"me apresente", "me dê uma apresentação", "me de uma apresentacao",
		"suas funções", "suas funcoes", "suas opções", "suas opcoes",
		"como pode me ajudar", "como você pode me ajudar", "como voce pode me ajudar",
		"o que é o jarvis", "o que e o jarvis",
		"me mostra o que faz", "me mostra o que você faz",
	}
	for _, t := range triggers {
		if strings.Contains(q, t) {
			return true
		}
	}
	return false
}

// handleIntroRequest generates and posts a capabilities presentation.
// The LLM writes the message using real context from the configured integrations
// (docs/jira_projects.md, docs/metabase_schema_compact.md, etc.).
// Falls back to a static message if the LLM call fails.
func (s *Service) handleIntroRequest(channel, threadTs, originTs string) error {
	// Invert JiraProjectNameMap ("project-name" → "PROJ") to ("PROJ" → "Project-Name").
	keyToName := make(map[string]string)
	for name, key := range s.Cfg.JiraProjectNameMap {
		display := strings.Title(strings.ToLower(name)) //nolint:staticcheck
		keyToName[strings.ToUpper(key)] = display
	}

	opts := introOptions{
		jiraEnabled:        s.Cfg.JiraEnabled(),
		jiraCreateEnabled:  s.Cfg.JiraCreateEnabled,
		jiraProjectKeys:    s.Cfg.JiraProjectKeys,
		jiraKeyToName:      keyToName,
		metabaseEnabled:    s.Cfg.MetabaseEnabled(),
		csvEnabled:         strings.TrimSpace(s.Cfg.PublicBaseURL) != "",
		outlineEnabled:     s.Cfg.OutlineEnabled(),
		slackSearchEnabled: strings.TrimSpace(s.Cfg.SlackUserToken) != "",
	}

	// Build feature description for the LLM prompt.
	featuresDesc := buildFeaturesDesc(opts)

	// Collect context from generated docs — the LLM uses these to write
	// realistic examples with real project and table names.
	docsContext := buildDocsContext(s.Cfg.JiraProjectsPath, s.Cfg.MetabaseSchemaPath)

	// Generate with LLM; static message is the fallback.
	fallback := buildIntroMessage(s.Cfg.BotName, opts)
	answer := s.LLM.GenerateIntroMessage(s.Cfg.BotName, featuresDesc, docsContext, s.Cfg.OpenAIModel, fallback)

	msgTs, err := s.Slack.PostMessageAndGetTS(channel, threadTs, answer)
	if err != nil {
		log.Printf("[JARVIS] intro: PostMessage failed: %v", err)
		return err
	}
	if msgTs != "" {
		s.Slack.Tracker.Track(channel, originTs, msgTs)
	}
	return nil
}

// buildFeaturesDesc returns a human-readable bullet list of active features
// for inclusion in the LLM prompt.
func buildFeaturesDesc(opts introOptions) string {
	var lines []string

	if opts.jiraEnabled {
		proj := projectList(opts.jiraProjectKeys, opts.jiraKeyToName)
		lines = append(lines, fmt.Sprintf("- Consultas no Jira: busca de cards, sprints, status, responsáveis (projetos configurados: %s)", proj))
	}
	if opts.jiraCreateEnabled {
		lines = append(lines, "- Edição de cards no Jira: mudar status (com encadeamento automático de etapas), atribuir responsável, definir card pai, mover para sprint, atualizar campos")
		lines = append(lines, "- Criação de cards no Jira: por linguagem natural, a partir da thread atual, ou em formato explícito")
	}
	if opts.slackSearchEnabled {
		lines = append(lines, "- Busca no Slack: encontrar mensagens, decisões e discussões anteriores por tema, pessoa ou canal; suporte a links de thread")
	}
	if opts.metabaseEnabled {
		line := "- Consultas de dados: perguntas em português viram consultas no banco de dados automaticamente; suporte a filtros, agrupamentos e totalizações"
		if opts.csvEnabled {
			line += "; exportação de resultados em CSV"
		}
		lines = append(lines, line)
	}
	if opts.outlineEnabled {
		lines = append(lines, "- Documentação interna: busca na wiki da empresa (Outline) por processos, políticas e guias; inclui links para as páginas relevantes nas respostas")
	}
	lines = append(lines, "- Análise de arquivos: PDF, Excel, Word, CSV e imagens — basta anexar junto com a pergunta")
	lines = append(lines, "- Contexto de conversa: lembra do histórico da thread; aceita links de conversas do Slack para buscar contexto")

	return strings.Join(lines, "\n")
}

// buildDocsContext reads available generated docs from disk and returns a
// trimmed excerpt suitable for injection into the LLM prompt.
func buildDocsContext(jiraProjectsPath, metabaseSchemaPath string) string {
	var parts []string

	if content := readDocFile(jiraProjectsPath, 4000); content != "" {
		parts = append(parts, "--- Projetos Jira ---\n"+content)
	}

	// Use the compact schema (much smaller than the full one).
	compactPath := strings.TrimSuffix(metabaseSchemaPath, ".md") + "_compact.md"
	if content := readDocFile(compactPath, 3000); content != "" {
		parts = append(parts, "--- Schema do banco de dados (resumo) ---\n"+content)
	}

	return strings.Join(parts, "\n\n")
}

// readDocFile reads a file and returns up to maxChars characters, or "" on error.
func readDocFile(path string, maxChars int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > maxChars {
		s = s[:maxChars]
	}
	return s
}

// ── Static fallback ───────────────────────────────────────────────────────────

// projectName returns the human-readable name for a project key.
func projectName(key string, keyToName map[string]string) string {
	if name, ok := keyToName[strings.ToUpper(key)]; ok && name != "" {
		return name
	}
	return key
}

// projectList returns a readable comma-separated list of project names.
func projectList(keys []string, keyToName map[string]string) string {
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, projectName(k, keyToName))
	}
	return strings.Join(names, ", ")
}

// c wraps a phrase in Slack inline-code backticks.
func c(s string) string { return "`" + s + "`" }

// buildIntroMessage is the static fallback used when the LLM call fails.
func buildIntroMessage(botName string, opts introOptions) string {
	if botName == "" {
		botName = "Jarvis"
	}

	var sections []string

	if opts.jiraEnabled {
		projCtx := ""
		if len(opts.jiraProjectKeys) > 0 {
			projCtx = " _(projetos: " + projectList(opts.jiraProjectKeys, opts.jiraKeyToName) + ")_"
		}
		sections = append(sections, fmt.Sprintf(
			"*Consultas no Jira* 🎯%s\n"+
				"• %s — planejamento, épicos e histórias do projeto\n"+
				"• %s — cards em aberto por tipo ou status\n"+
				"• %s — o que está sendo feito na sprint atual\n"+
				"• %s — tudo que uma pessoa está trabalhando\n"+
				"• %s — detalhes completos de um card específico",
			projCtx,
			c(`"roadmap do projeto"`),
			c(`"quais bugs estão abertos?"`),
			c(`"o que está na sprint atual?"`),
			c(`"o que o David está trabalhando?"`),
			c(`"me mostra o PROJ-100"`),
		))
	}

	if opts.jiraCreateEnabled {
		sections = append(sections,
			"*Edição de cards no Jira* ✏️\n"+
				"• "+c(`"pode concluir o PROJ-100"`)+" — muda o status passando pelas etapas automaticamente\n"+
				"• "+c(`"atribui o PROJ-100 ao David"`)+" — define o responsável\n"+
				"• "+c(`"manda o PROJ-100 pra sprint atual"`)+" — move para a sprint em andamento\n"+
				"• "+c(`"muda a prioridade do PROJ-100 para alta"`)+" — atualiza campos do card")

		sections = append(sections,
			"*Criação de cards no Jira* 📝\n"+
				"• "+c(`"crie um bug com título '...'"`)+" — criação por linguagem natural\n"+
				"• "+c(`"com base nessa conversa, abre uma tarefa"`)+" — extrai da thread atual\n"+
				"• "+c("confirmar")+" — confirma o rascunho e cria o card")
	}

	if opts.slackSearchEnabled {
		sections = append(sections,
			"*Busca no Slack* 🔍\n"+
				"• "+c(`"onde falamos sobre X?"`)+" — encontra discussões anteriores\n"+
				"• "+c(`"o que foi decidido sobre Y?"`)+" — recupera contexto de decisões\n"+
				"• Cole um link de thread e eu busco e resumo aquela conversa")
	}

	if opts.metabaseEnabled {
		csvLine := ""
		if opts.csvEnabled {
			csvLine = "\n• " + c(`"exporta isso em planilha"`) + " — resultados em CSV"
		}
		sections = append(sections,
			"*Consultas de dados* 📊\n"+
				"• "+c(`"quantos registros temos essa semana?"`)+" — pergunta em português, eu monto a consulta\n"+
				"• "+c(`"lista os 10 maiores por volume em fevereiro"`)+" — filtros e ordenações\n"+
				"• "+c(`"qual foi a consulta que você usou?"`)+" — exibe a busca anterior"+csvLine)
	}

	if opts.outlineEnabled {
		sections = append(sections,
			"*Documentação interna* 📚\n"+
				"• "+c(`"como funciona o processo de X?"`)+" — busca na wiki da empresa\n"+
				"• Incluo links diretos para as páginas relevantes nas respostas")
	}

	sections = append(sections,
		"*Análise de arquivos* 📎\n"+
			"• PDF, Excel, Word, imagens — basta anexar junto com sua pergunta")

	sections = append(sections,
		"*Contexto da conversa* 💬\n"+
			"• Lembro do histórico da thread — pergunte em sequência sem repetir contexto\n"+
			"• Cole um link de conversa do Slack e busco o contexto completo")

	howToCall := fmt.Sprintf(
		"*Como me chamar:*\n"+
			"• Mencione *@%s* em qualquer canal\n"+
			"• Em conversas diretas, basta enviar a mensagem diretamente",
		botName,
	)

	body := strings.Join(sections, "\n\n\n")
	return fmt.Sprintf(
		"Oi! Sou o *%s*, seu assistente operacional no Slack. 👋\n\nAqui está o que posso fazer por você:\n\n%s\n\n\n%s\n\nPode perguntar à vontade! 🚀",
		botName, body, howToCall,
	)
}
