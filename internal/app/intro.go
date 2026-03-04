package app

import (
	"fmt"
	"log"
	"strings"
)

// introOptions carries the feature-gate flags used to tailor the intro message
// to the current deployment configuration.
type introOptions struct {
	jiraEnabled        bool
	jiraCreateEnabled  bool
	jiraProjectKeys    []string
	metabaseEnabled    bool
	csvEnabled         bool // requires PUBLIC_BASE_URL
	outlineEnabled     bool
	slackSearchEnabled bool // requires SLACK_USER_TOKEN
}

// handleIntroRequest posts a static capabilities presentation to the thread.
func (s *Service) handleIntroRequest(channel, threadTs, originTs string) error {
	busyTs, busyErr := s.Slack.PostMessageAndGetTS(channel, threadTs, "_preparando apresentação..._")
	if busyErr != nil {
		log.Printf("[JARVIS] intro: could not post busy indicator: %v", busyErr)
	}

	replyFn := func(text string) error {
		if busyTs != "" {
			if err := s.Slack.UpdateMessage(channel, busyTs, text); err != nil {
				return s.Slack.PostMessage(channel, threadTs, text)
			}
			return nil
		}
		return s.Slack.PostMessage(channel, threadTs, text)
	}

	opts := introOptions{
		jiraEnabled:        s.Cfg.JiraEnabled(),
		jiraCreateEnabled:  s.Cfg.JiraCreateEnabled,
		jiraProjectKeys:    s.Cfg.JiraProjectKeys,
		metabaseEnabled:    s.Cfg.MetabaseEnabled(),
		csvEnabled:         strings.TrimSpace(s.Cfg.PublicBaseURL) != "",
		outlineEnabled:     s.Cfg.OutlineEnabled(),
		slackSearchEnabled: strings.TrimSpace(s.Cfg.SlackUserToken) != "",
	}
	answer := buildIntroMessage(s.Cfg.BotName, opts)

	if err := replyFn(answer); err != nil {
		return err
	}
	if busyTs != "" {
		s.Slack.Tracker.Track(channel, originTs, busyTs)
	}
	return nil
}

// buildIntroMessage returns a Slack mrkdwn-formatted presentation of all
// capabilities, adapted to the active feature flags.
func buildIntroMessage(botName string, opts introOptions) string {
	if botName == "" {
		botName = "Jarvis"
	}

	var sections []string

	// ── Jira: queries ────────────────────────────────────────────────────────
	if opts.jiraEnabled {
		projCtx := ""
		if len(opts.jiraProjectKeys) > 0 {
			projCtx = " _(projetos: " + strings.Join(opts.jiraProjectKeys, ", ") + ")_"
		}
		sections = append(sections, fmt.Sprintf(`*Consultas no Jira* 🎯%s
• _"roadmap do projeto TPTDR"_ — veja épicos, histórias e o planejamento
• _"quais bugs estão abertos no INV?"_ — lista issues filtradas por status
• _"o que está na sprint atual do transportador?"_ — issues da sprint ativa
• _"o que o David está trabalhando?"_ — busca por assignee
• _"me mostra o TPTDR-522"_ — detalhes completos de um card específico
• _"cards bloqueados com prioridade alta"_ — filtros combinados por JQL`, projCtx))
	}

	// ── Jira: editing ────────────────────────────────────────────────────────
	if opts.jiraCreateEnabled {
		sections = append(sections, `*Edição de cards no Jira* ✏️
• _"pode concluir o TPTDR-522"_ — muda o status (passa pelos intermediários automaticamente)
• _"atribui o TPTDR-522 ao David"_ — atribui a um colega
• _"atribui o TPTDR-100 a mim"_ — atribui a você mesmo
• _"muda a prioridade do INV-88 para alta"_ — atualiza campos
• _"vincula o TPTDR-300 ao pai TPTDR-200"_ — define issue pai
• _"fecha e atribui o TPTDR-522 ao David"_ — combina várias ações em uma mensagem`)
	}

	// ── Jira: creation ───────────────────────────────────────────────────────
	if opts.jiraCreateEnabled {
		sections = append(sections, `*Criação de cards no Jira* 🆕
• _"crie um bug no TPTDR com título 'Erro no cálculo de frete'"_ — criação por linguagem natural
• _"com base nessa thread, abre uma história no INV"_ — extrai o card da conversa atual
• _"com base nessa thread crie dois cards: um bug e uma tarefa"_ — múltiplos cards de uma vez
• _"jira criar | TPTDR | Bug | Título | Descrição detalhada"_ — formato explícito
• _confirmar_ — confirma o rascunho pendente e cria o card
• _cancelar card_ — descarta o rascunho atual`)
	}

	// ── Slack search ─────────────────────────────────────────────────────────
	if opts.slackSearchEnabled {
		sections = append(sections, `*Busca no Slack* 🔍
• _"onde falamos sobre integração com o transportador?"_ — encontra threads e discussões
• _"o que foi decidido sobre a migração do banco?"_ — recupera contexto de decisões passadas
• _"o que o @fulano disse essa semana sobre deploy?"_ — filtra por usuário e período
• _"tem alguma discussão sobre autenticação no #backend?"_ — busca direcionada por canal
• Cole um link de thread do Slack e eu busco e resumo o contexto daquela conversa`)
	}

	// ── Metabase / data queries ──────────────────────────────────────────────
	if opts.metabaseEnabled {
		csvExample := ""
		if opts.csvEnabled {
			csvExample = "\n• _\"exporta isso em CSV\"_ — baixe os resultados em planilha (link com validade de 1h)"
		}
		sections = append(sections, fmt.Sprintf(`*Consultas de dados* 📊
• _"quantas coletas foram realizadas essa semana?"_ — pergunta em linguagem natural, eu gero a query SQL
• _"lista os 10 geradores com mais ocorrências em fevereiro"_ — filtros por período e ordenação
• _"qual o volume de notas emitidas por estado?"_ — agrupamentos e totalizações
• _"mostre todos os motoristas ativos"_ — resultados completos sem limitação de linhas
• _"qual foi a query que você usou?"_ — exibe o SQL executado na resposta anterior%s`, csvExample))
	}

	// ── Outline wiki ─────────────────────────────────────────────────────────
	if opts.outlineEnabled {
		sections = append(sections, `*Documentação interna* 📚
• _"como funciona o processo de onboarding de transportadores?"_ — busca na wiki interna
• _"qual é a política de SLA para reclamações?"_ — recupera docs de processo
• _"me explica o fluxo de faturamento"_ — entende e resume documentos internos
• Quando relevante, incluo links diretos para as páginas do Outline nas respostas`)
	}

	// ── File analysis ────────────────────────────────────────────────────────
	sections = append(sections, `*Análise de arquivos* 📎
• *PDF* — resumo, extração de dados, perguntas sobre o conteúdo
• *Excel / CSV* — análise de planilhas, totalizações, identificação de padrões
• *Word (DOCX)* — leitura e resumo de documentos
• *Imagens* — descrição e extração de texto via visão computacional
• Basta anexar o arquivo na mensagem junto com sua pergunta`)

	// ── Conversation context ─────────────────────────────────────────────────
	sections = append(sections, `*Contexto da conversa* 💬
• Entendo o histórico da thread — pode perguntar em sequência sem repetir contexto
• _"e no mês passado?"_ — filtro de acompanhamento sem reformular a pergunta inteira
• _"pode exportar isso?"_ — referência à resposta anterior de dados
• Cole um link de thread do Slack e busco o contexto completo daquela conversa`)

	// ── Summoning ────────────────────────────────────────────────────────────
	howToCall := fmt.Sprintf(`*Como me chamar:*
• Mencione *@%s* em qualquer canal
• Em DMs, basta enviar a mensagem diretamente — sem prefixo`, botName)

	body := strings.Join(sections, "\n\n")

	return fmt.Sprintf("Oi! Sou o *%s*, seu assistente operacional no Slack. 👋\n\nAqui está o que posso fazer por você:\n\n%s\n\n%s\n\nPode perguntar à vontade! 🚀",
		botName, body, howToCall)
}
