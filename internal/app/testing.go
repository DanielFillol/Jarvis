package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/outline"
	apptest "github.com/DanielFillol/Jarvis/internal/testing"
)

// isTestCommand reports whether the question is a smoke-test trigger.
func isTestCommand(q, botName string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	lower := strings.ToLower(botName)
	return q == lower+" teste" || q == lower+" test" || q == "/teste"
}

// handleTestFlow runs the full prompt library smoke test and posts results in the thread.
func (s *Service) handleTestFlow(channel, threadTs string) error {
	libraryPath := "docs/prompt_library.md"
	tests, err := apptest.ParsePromptLibrary(libraryPath)
	if err != nil {
		log.Printf("[TEST] could not parse prompt library: %v", err)
		return s.Slack.PostMessage(channel, threadTs,
			fmt.Sprintf("Não consegui ler a biblioteca de prompts (%s): %v", libraryPath, err))
	}
	if len(tests) == 0 {
		return s.Slack.PostMessage(channel, threadTs,
			"Biblioteca de prompts sem entradas testáveis. Verifique o formato do arquivo.")
	}

	startMsg := fmt.Sprintf("_Iniciando ciclo de testes da biblioteca de prompts (%d prompts)..._", len(tests))
	if err := s.Slack.PostMessage(channel, threadTs, startMsg); err != nil {
		log.Printf("[TEST] could not post start message: %v", err)
	}

	ctx := context.Background()
	results := apptest.RunAll(ctx, s, tests, channel, threadTs)
	summary := apptest.FormatSummary(results)
	return s.Slack.PostMessage(channel, threadTs, summary)
}

// HandleMessageDirect executes the context-building and LLM answer flow and
// returns the response as a string instead of posting it to Slack.
// Used by smoke tests in the prompt library runner.
func (s *Service) HandleMessageDirect(ctx context.Context, channel, threadTs, originTs, question, senderUserID string) (string, error) {
	hubspotCatalog := ""
	if s.HubSpot != nil {
		hubspotCatalog = s.HubSpot.CatalogCompact
	}
	actions, err := s.LLM.DecideActions(
		question, "", s.Cfg.OpenAILesserModel,
		s.Cfg.JiraEnabled(), s.Jira.CatalogCompact, senderUserID,
		s.formattedMetabaseDatabases(), 0, s.Cfg.OutlineEnabled(),
		s.Cfg.GoogleDriveEnabled(),
		s.Cfg.HubSpotEnabled(),
		hubspotCatalog,
	)
	if err != nil {
		log.Printf("[TEST] decideActions failed: %v", err)
		actions = []llm.ActionDescriptor{{Kind: llm.ActionJiraSearch, JiraIntent: "default"}}
	}

	_, contextActions := splitActions(actions)

	var jiraCtxParts, slackCtxParts, dbCtxParts []string
	var outlineCtx string

	for _, action := range contextActions {
		switch action.Kind {
		case llm.ActionJiraSearch:
			jql := strings.TrimSpace(action.JQL)
			if jql == "" {
				jql = defaultJQLForIntent(action.JiraIntent, question, s.Cfg.JiraProjectKeys)
			}
			jql = sanitizeJQL(jql)
			issues, jErr := s.Jira.FetchAll(jql, 200)
			if jErr != nil {
				if corrected := correctJQLStatus(jql, s.Jira.WorkflowStatuses); corrected != jql {
					issues, jErr = s.Jira.FetchAll(corrected, 200)
				}
			}
			if jErr != nil {
				jiraCtxParts = append(jiraCtxParts,
					"[JIRA_ERROR: A busca falhou. NÃO invente issues, títulos, assignees ou chaves.]")
			} else if len(issues) == 0 {
				jiraCtxParts = append(jiraCtxParts,
					fmt.Sprintf("[JIRA_EMPTY: JQL '%s' retornou 0 issues. NÃO invente issues.]", jql))
			} else {
				jiraCtxParts = append(jiraCtxParts, buildJiraContext(issues, 40))
			}

		case llm.ActionSlackSearch:
			// Skip Slack search in test mode — avoid polluting real channels.
			slackCtxParts = append(slackCtxParts, "[AVISO: busca Slack desabilitada em modo de teste.]")

		case llm.ActionMetabaseQuery:
			if s.Metabase == nil {
				break
			}
			mRes := s.runMetabaseQuery(question, "", action.MetabaseDatabaseID, "", action.WantsAllRows)
			if mRes.DBCtx != "" {
				dbCtxParts = append(dbCtxParts, mRes.DBCtx)
			}

		case llm.ActionOutlineSearch:
			if s.Outline == nil {
				break
			}
			outlineQuery := strings.TrimSpace(action.Query)
			if outlineQuery == "" {
				outlineQuery = s.LLM.GenerateOutlineQuery(question, s.Cfg.OpenAILesserModel)
			}
			if outlineQuery != "" {
				results, oErr := s.Outline.SearchDocuments(outlineQuery, 5)
				if oErr == nil {
					outlineCtx = outline.FormatContext(results, 8000)
				}
			}
		}
	}

	jiraCtx := strings.Join(jiraCtxParts, "\n\n")
	slackCtx := strings.Join(slackCtxParts, "\n\n")
	dbCtx := strings.Join(dbCtxParts, "\n\n")

	answer, err := s.LLM.AnswerWithRetry(
		s.getCompanyCtx(),
		question, "", slackCtx, jiraCtx, dbCtx, "", outlineCtx, "", "", nil,
		s.Cfg.OpenAIModel, s.Cfg.OpenAILesserModel, 2, 0,
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(answer), nil
}
