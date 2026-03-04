package app

import (
	"fmt"
	"log"
	"strings"
)

// handleIntroRequest a static message.
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
	answer := buildIntroMessage(s.Cfg.BotName, s.Cfg.JiraCreateEnabled, nil)

	if err := replyFn(answer); err != nil {
		return err
	}
	// Track so the reply is deleted if the user deletes their message.
	if busyTs != "" {
		s.Slack.Tracker.Track(channel, originTs, busyTs)
	}
	return nil
}

// buildIntroMessage returns a Slack mrkdwn-formatted presentation of
// the bot's capabilities, adapted to the current configuration.
func buildIntroMessage(botName string, jiraCreateEnabled bool, jiraProjectKeys []string) string {
	if botName == "" {
		botName = "Jarvis"
	}
	projCtx := ""
	if len(jiraProjectKeys) > 0 {
		projCtx = " (projetos configurados: " + strings.Join(jiraProjectKeys, ", ") + ")"
	}

	createSection := ""
	if jiraCreateEnabled {
		createSection = `*Criação de cards no Jira* ✏️
• _"crie um bug no backend com título X"_ — criação por linguagem natural
• _"com base nessa thread crie um card no projeto PROJ"_ — extrai da conversa
• _"com base nessa thread crie dois cards"_ — criação de múltiplos cards de uma vez
• _"jira criar | PROJ | Bug | Título | Descrição"_ — formato explícito e detalhado
• _confirmar_ — confirma o rascunho pendente e cria o card
• _cancelar card_ — descarta o rascunho atual

`
	}

	return fmt.Sprintf(`Oi! Sou o *%s*, seu assistente operacional no Slack. 👋

Aqui está o que posso fazer por você:

*Consultas no Jira* 🎯%s
• _"roadmap do projeto PROJ"_ — veja o planejamento do projeto
• _"quais bugs estão abertos?"_ — lista bugs em aberto
• _"me mostre as issues da sprint 7"_ — issues filtradas por sprint
• _"quem está trabalhando em pagamentos?"_ — busca por assignee ou tema
• _"o que é o PROJ-123?"_ — detalhes completos de uma issue específica

*Busca no Slack* 🔍
• _"onde falamos sobre integração de pagamentos?"_ — encontra threads e discussões
• _"o que foi decidido sobre autenticação?"_ — recupera contexto de decisões passadas
• _"o que o @fulano falou essa semana?"_ — filtra por usuário e período
• _"me acha discussões sobre deploy no #canal"_ — busca direcionada por canal

%s*Contexto da conversa* 💬
• Entendo o histórico da thread onde estou — pode perguntar em sequência sem repetir contexto
• Se você colar um link de thread do Slack, busco e resumo o contexto daquela conversa

*Conversas gerais* 🧠
• Posso conversar sobre qualquer assunto, responder dúvidas técnicas, ajudar a redigir textos, explicar conceitos e muito mais!

*Como me chamar:*
• Mencione _@%s_ em qualquer canal ou DM
• Use o prefixo _jarvis:_ no início da mensagem

Pode perguntar à vontade! 🚀`, botName, projCtx, createSection, botName)
}
