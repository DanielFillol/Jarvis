package app

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/fileserver"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

// Service encapsulates the core orchestration logic of Jarvis.  It
// coordinates between Slack, Jira, Metabase, and the language model to answer
// questions and handle issue creation flows.  The Service does not depend on
// net/http and is therefore easily testable.
type Service struct {
	Slack    *slack.Client
	Jira     *jira.Client
	LLM      *llm.Client
	Metabase *metabase.Client
	Cfg      config.Config
	// threadLastDBID maps "channel:threadTs" → database ID for the last Metabase
	// query executed in that thread.  Used to restore routing context for follow-up messages
	threadLastDBID sync.Map

	// threadLastSQL maps "channel:threadTs" → the last successfully executed SQL
	// string in that thread.  Used as a reliable base for follow-up queries so
	// that all prior filters are preserved even when the thread history does not
	// contain the full LLM response (which may be truncated by Slack).
	threadLastSQL sync.Map

	// pendingReplies maps "channel:threadTs" → []string (message chunks) for
	// long answers awaiting user confirmation before being posted to the thread.
	pendingReplies sync.Map

	Store      state.Store
	FileServer *fileserver.FileServer
}

// NewService constructs a new Jarvis service from its dependencies.
// metabaseClient may be nil when Metabase integration is not configured.
// fs may be nil when CSV export is not needed.
func NewService(cfg config.Config, slackClient *slack.Client, jiraClient *jira.Client, llmClient *llm.Client, metabaseClient *metabase.Client, fs *fileserver.FileServer) *Service {
	return &Service{
		Slack:      slackClient,
		Jira:       jiraClient,
		LLM:        llmClient,
		Metabase:   metabaseClient,
		Cfg:        cfg,
		FileServer: fs,
		Store:      *state.NewStore(2 * time.Hour),
	}
}

// HandleMessage processes an incoming message directed at the bot.  It
// delegates to the appropriate flows: Jira creation, context retrieval, and
// answer generation.  All routing decisions go through the LLM — there are no
// hardcoded keyword overrides.  On error, a fallback answer is posted to Slack.
func (s *Service) HandleMessage(channel, threadTs, originTs, originalText, question, senderUserID string, files []slack.File) error {
	start := time.Now()
	log.Printf("[JARVIS] start question=%q originTs=%q senderUserID=%q", preview(question, 180), originTs, senderUserID)

	// 1) Resolve thread context: current thread vs. explicit Slack permalink.
	contextChannel := channel
	contextThreadTs := threadTs
	hasThreadPermalink := false
	if chFromLink, tsFromLink, ok := parse.ExtractSlackThreadPermalink(originalText); ok {
		contextChannel = chFromLink
		contextThreadTs = tsFromLink
		hasThreadPermalink = true
	}

	// 2) Check if the user is confirming or cancelling a pending long reply.
	{
		pendingKeyTs := contextThreadTs
		if pendingKeyTs == "" {
			pendingKeyTs = originTs
		}
		pendingKey := contextChannel + ":" + pendingKeyTs
		if raw, ok := s.pendingReplies.Load(pendingKey); ok {
			chunks := raw.([]string)
			if isLongReplyCancellation(question) {
				s.pendingReplies.Delete(pendingKey)
				_ = s.Slack.PostMessage(channel, threadTs, "Ok, resposta cancelada.")
				log.Printf("[JARVIS] long reply cancelled dur=%s", time.Since(start))
				return nil
			}
			if isLongReplyConfirmation(question) {
				s.pendingReplies.Delete(pendingKey)
				for i, chunk := range chunks {
					if postErr := s.Slack.PostMessage(channel, threadTs, chunk); postErr != nil {
						log.Printf("[ERR] long reply chunk %d/%d: %v", i+1, len(chunks), postErr)
					}
				}
				log.Printf("[JARVIS] long reply confirmed, posted %d chunks dur=%s", len(chunks), time.Since(start))
				return nil
			}
			// Not a clear yes/no — discard pending and process as a new question.
			s.pendingReplies.Delete(pendingKey)
		}
	}

	// 3) Thread history (full fetch when an explicit permalink was provided).
	var threadHist string
	var err error
	if hasThreadPermalink {
		threadHist, err = s.Slack.GetThreadHistoryFull(contextChannel, contextThreadTs, 400, 40000)
	} else {
		threadHist, err = s.Slack.GetThreadHistory(contextChannel, contextThreadTs, 60)
	}
	if err != nil {
		log.Printf("[WARN] thread history failed: %v", err)
	}
	log.Printf("[JARVIS] thread history chars=%d", len(threadHist))

	// 4) High-priority: Jira creation flows (always use the original thread).
	handled, err := s.maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist)
	if handled {
		log.Printf("[JARVIS] jiraCreateFlow handled dur=%s", time.Since(start))
		return err
	}

	// 5) Post a "searching…" placeholder so the user knows Jarvis is working.
	busyTs, busyErr := s.Slack.PostMessageAndGetTS(channel, threadTs, "_buscando..._")
	if busyErr != nil {
		log.Printf("[WARN] could not post busy indicator: %v", busyErr)
	}

	// replyFn updates the busy placeholder in-place; falls back to a new post.
	replyFn := func(text string) error {
		if busyTs != "" {
			if err := s.Slack.UpdateMessage(channel, busyTs, text); err != nil {
				log.Printf("[WARN] UpdateMessage failed, falling back: %v", err)
				return s.Slack.PostMessage(channel, threadTs, text)
			}
			return nil
		}
		return s.Slack.PostMessage(channel, threadTs, text)
	}

	// 6) Routing decision via LLM (lesser model).
	// Resolve Slack mentions first so the router sees readable names.
	questionForLLM := s.Slack.ResolveUserMentions(s.Slack.ResolveChannelMentions(parse.StripSlackPermalinks(question)))
	storedDBID, _ := s.loadThreadDBID(contextChannel, contextThreadTs)
	decision, err := s.LLM.DecideRetrieval(
		questionForLLM, threadHist, s.Cfg.OpenAILesserModel,
		s.Cfg.JiraEnabled(), s.Jira.CatalogCompact, senderUserID,
		s.formattedMetabaseDatabases(), storedDBID,
	)
	if err != nil {
		log.Printf("[WARN] decideRetrieval failed: %v", err)
		if hasThreadPermalink {
			decision = llm.RetrievalDecision{}
		} else {
			decision = llm.RetrievalDecision{NeedSlack: false, NeedJira: true, JiraIntent: "default"}
		}
	}
	// Explicit permalink → thread content is already the authoritative context;
	// no need for an additional Slack search.
	if hasThreadPermalink {
		decision.NeedSlack = false
		decision.SlackQuery = ""
	}
	log.Printf("[JARVIS] needSlack=%t slackQuery=%q needJira=%t jiraJQL=%q needMetabase=%t dbID=%d showSQL=%t wantsAllRows=%t wantsCSV=%t",
		decision.NeedSlack, preview(decision.SlackQuery, 120),
		decision.NeedJira, preview(decision.JiraJQL, 120),
		decision.NeedMetabase, decision.MetabaseDatabaseID,
		decision.ShowSQL, decision.WantsAllRows, decision.WantsCSVExport)

	// 7) Slack context retrieval.
	var slackCtx string
	var slackMatches int
	if decision.NeedSlack && strings.TrimSpace(decision.SlackQuery) != "" && s.Cfg.SlackUserToken != "" {
		unresolvedUserIDs := extractFromUserIDs(decision.SlackQuery)
		decision.SlackQuery = s.Slack.ResolveUserIDsInQuery(decision.SlackQuery)
		log.Printf("[JARVIS] slackSearch query=%q", decision.SlackQuery)
		matches, err := s.Slack.SearchMessagesAll(decision.SlackQuery)
		if err != nil {
			log.Printf("[WARN] slack search failed: %v", err)
		} else {
			if len(unresolvedUserIDs) > 0 {
				filtered := matches[:0]
				for _, m := range matches {
					for _, uid := range unresolvedUserIDs {
						if m.UserID == uid {
							filtered = append(filtered, m)
							break
						}
					}
				}
				log.Printf("[JARVIS] clientSideUserFilter from=%v reduced %d→%d", unresolvedUserIDs, len(matches), len(filtered))
				matches = filtered
			}
			slackMatches = len(matches)
			slackCtx = buildSlackContext(matches, 25)
			if slackMatches == 0 {
				slackCtx = "[AVISO: A busca no Slack não retornou mensagens. NÃO invente conteúdo de canais ou mensagens. Informe ao usuário que não foram encontrados dados para a busca realizada e sugira alternativas.]"
			}
			log.Printf("[JARVIS] slackContext matches=%d chars=%d", slackMatches, len(slackCtx))
		}
	}

	// Direct channel history fallback for raw <#CHANID> mentions that cannot
	// be resolved to names for Slack search.
	if decision.NeedSlack {
		if chanIDs := extractChannelIDsFromText(question); len(chanIDs) > 0 {
			now := time.Now()
			weekday := int(now.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			weekStart := now.AddDate(0, 0, -(weekday - 1)).Truncate(24 * time.Hour)
			var directMsgs []slack.SearchMessage
			for _, cid := range chanIDs {
				msgs, err := s.Slack.GetChannelHistoryForPeriod(cid, weekStart, now, 80)
				if err != nil {
					log.Printf("[JARVIS] channelHistory %s failed: %v", cid, err)
					continue
				}
				log.Printf("[JARVIS] channelHistory %s messages=%d", cid, len(msgs))
				directMsgs = append(directMsgs, msgs...)
			}
			if len(directMsgs) > 0 {
				slackCtx = buildSlackContext(directMsgs, 40)
				slackMatches = len(directMsgs)
				log.Printf("[JARVIS] channelHistory total=%d chars=%d", slackMatches, len(slackCtx))
			}
		}
	}

	// 8) Jira context via LLM-generated JQL.
	var jiraCtx string
	var jiraIssuesFound int
	if decision.NeedJira {
		jql := strings.TrimSpace(decision.JiraJQL)
		if jql == "" {
			jql = defaultJQLForIntent(decision.JiraIntent, question, s.Cfg.JiraProjectKeys)
		}
		jql = sanitizeJQL(jql)
		log.Printf("[JARVIS] jiraJQL=%q", jql)
		issues, err := s.Jira.FetchAll(jql, 200)
		if err != nil {
			log.Printf("[WARN] jira search failed: %v", err)
		} else {
			jiraIssuesFound = len(issues)
			jiraCtx = buildJiraContext(issues, 40)
			log.Printf("[JARVIS] jiraContext issues=%d chars=%d", jiraIssuesFound, len(jiraCtx))
		}
	}

	// 9) Metabase database query.
	var dbCtx string
	var dbQueryResult *metabase.QueryResult
	var executedSQL string
	if decision.NeedMetabase && s.Metabase != nil {
		dbCtx, dbQueryResult, executedSQL = s.runMetabaseQuery(
			questionForLLM, threadHist, decision.MetabaseDatabaseID, "",
		)
		// LLM requested clarification — ask the user and stop processing here.
		if strings.HasPrefix(dbCtx, llm.ClarificationPrefix) {
			clarificationQ := strings.TrimPrefix(dbCtx, llm.ClarificationPrefix)
			if replyErr := replyFn(clarificationQ); replyErr != nil {
				log.Printf("[ERR] clarification reply failed: %v", replyErr)
				return replyErr
			}
			if busyTs != "" {
				s.Slack.Tracker.Track(channel, originTs, busyTs)
			}
			log.Printf("[JARVIS] clarification requested dur=%s", time.Since(start))
			return nil
		}
		// Persist DB ID so the LLM router can identify future follow-ups.
		if executedSQL != "" {
			s.storeThreadDBID(contextChannel, contextThreadTs, decision.MetabaseDatabaseID)
		}
		if dbCtx == "" {
			dbCtx = "[ERRO: A consulta ao banco de dados falhou ou não retornou dados. NÃO invente métricas, nomes ou valores. Informe ao usuário que não foi possível obter os dados neste momento e sugira tentar novamente.]"
		}
	}

	// 9b) ShowSQL: user asked for the query used in a prior answer.
	// runMetabaseQuery above already reconstructed and validated it.
	if decision.ShowSQL {
		var showSQLReply string
		if executedSQL != "" {
			showSQLReply = fmt.Sprintf(
				"Reconstruí e validei a query com base no contexto desta conversa:\n```sql\n%s\n```\n\n> _Nota: esta é uma reconstrução — pode diferir levemente da query original, mas foi executada com sucesso no banco de dados._",
				executedSQL,
			)
		} else {
			showSQLReply = "Não consegui reconstruir uma query válida para esta conversa após várias tentativas. Tente reformular a pergunta original para que eu possa buscar os dados novamente."
		}
		if replyErr := replyFn(showSQLReply); replyErr != nil {
			log.Printf("[ERR] show SQL reply failed: %v", replyErr)
			return replyErr
		}
		if busyTs != "" {
			s.Slack.Tracker.Track(channel, originTs, busyTs)
		}
		log.Printf("[JARVIS] show SQL handled executedSQL_len=%d dur=%s", len(executedSQL), time.Since(start))
		return nil
	}

	// 10) File context from attachments (current message + thread history files).
	// When the user references a file shared in an earlier reply, we collect all
	// files from the thread so follow-up questions don't miss prior attachments.
	allFiles := append([]slack.File{}, files...)
	if threadTs != "" {
		fetchTs := contextThreadTs
		if fetchTs == "" {
			fetchTs = threadTs
		}
		if tf, err := s.Slack.GetThreadFiles(contextChannel, fetchTs); err != nil {
			log.Printf("[WARN] GetThreadFiles failed: %v", err)
		} else {
			seen := make(map[string]bool)
			for _, f := range allFiles {
				seen[f.ID] = true
			}
			added := 0
			const maxThreadFiles = 5
			for _, f := range tf {
				if added >= maxThreadFiles {
					break
				}
				if !seen[f.ID] {
					seen[f.ID] = true
					allFiles = append(allFiles, f)
					added++
				}
			}
			if added > 0 {
				log.Printf("[JARVIS] threadFiles added=%d total=%d", added, len(allFiles))
			}
		}
	}
	fileCtx := s.buildFileContext(allFiles)
	if fileCtx != "" {
		log.Printf("[JARVIS] fileContext files=%d chars=%d", len(allFiles), len(fileCtx))
	}
	images := s.buildImageAttachments(allFiles)
	if len(images) > 0 {
		log.Printf("[JARVIS] imageAttachments count=%d", len(images))
	}

	// 10b) CSV export or large result set.
	const largeResultThreshold = 30
	var appendDataTable string
	var csvDownloadLine string

	if dbQueryResult != nil && decision.WantsCSVExport && s.FileServer != nil && strings.TrimSpace(s.Cfg.PublicBaseURL) != "" {
		// Generate CSV for download; give LLM only a sample for the intro sentence.
		nRows := len(dbQueryResult.Data.Rows)
		cols := make([]string, len(dbQueryResult.Data.Cols))
		for i, c := range dbQueryResult.Data.Cols {
			if c.DisplayName != "" {
				cols[i] = c.DisplayName
			} else {
				cols[i] = c.Name
			}
		}
		csvBytes := []byte(metabase.FormatQueryResultAsCSV(*dbQueryResult))
		if len(csvBytes) > 0 {
			fileID := s.FileServer.Store("resultado.csv", csvBytes, time.Hour)
			csvURL := s.Cfg.PublicBaseURL + "/files/" + fileID
			csvDownloadLine = fmt.Sprintf("\n\n:page_facing_up: *Download CSV:* <%s|resultado.csv> _(expira em 1 hora)_", csvURL)
			log.Printf("[JARVIS] CSV generated rows=%d id=%s", nRows, fileID)
		}
		sample := metabase.FormatQueryResult(*dbQueryResult, 3)
		dbCtx = fmt.Sprintf(
			"Query SQL retornou %d registros com os campos: %s.\n\n"+
				"INSTRUÇÃO INTERNA: Escreva APENAS 1 frase curta de introdução descrevendo o resultado "+
				"(ex: \"Encontrei %d registros...\"). Não exiba a tabela de dados. Finalize a resposta aí.\n\n"+
				"Amostra (3 de %d registros):\n%s",
			nRows, strings.Join(cols, ", "), nRows, nRows, sample,
		)
		log.Printf("[JARVIS] CSV export: rows=%d", nRows)
	} else if dbQueryResult != nil && len(dbQueryResult.Data.Rows) > largeResultThreshold && decision.WantsAllRows {
		// Large result inline display (no CSV requested).
		nRows := len(dbQueryResult.Data.Rows)
		cols := make([]string, len(dbQueryResult.Data.Cols))
		for i, c := range dbQueryResult.Data.Cols {
			if c.DisplayName != "" {
				cols[i] = c.DisplayName
			} else {
				cols[i] = c.Name
			}
		}
		appendDataTable = metabase.FormatQueryResult(*dbQueryResult, nRows)
		sample := metabase.FormatQueryResult(*dbQueryResult, 3)
		dbCtx = fmt.Sprintf(
			"Query SQL retornou %d registros com os campos: %s.\n\n"+
				"INSTRUÇÃO INTERNA: Escreva APENAS 1 frase curta de introdução descrevendo o resultado "+
				"(ex: \"Encontrei %d registros...\"). Finalize com o marcador exato [TABLE] e nada mais.\n\n"+
				"Amostra (3 de %d registros):\n%s",
			nRows, strings.Join(cols, ", "), nRows, nRows, sample,
		)
		log.Printf("[JARVIS] large result bypass: rows=%d", nRows)
	}

	// 11) Generate the answer with the primary LLM (with retry and fallback).
	answer, err := s.LLM.AnswerWithRetry(
		questionForLLM, threadHist, slackCtx, jiraCtx, dbCtx, fileCtx, images,
		s.Cfg.OpenAIModel, s.Cfg.OpenAILesserModel, 2, 0,
	)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[ERR] llmAnswer failed: %v", err)
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, decision.NeedJira, jiraIssuesFound, "")
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, decision.NeedJira, jiraIssuesFound, "")
	}

	// Append CSV download link when a file was generated.
	if csvDownloadLine != "" {
		answer += csvDownloadLine
	}

	// Append full data table when bypassing LLM for large result formatting.
	if appendDataTable != "" {
		if strings.Contains(answer, "[TABLE]") {
			answer = strings.Replace(answer, "[TABLE]", "\n```\n"+appendDataTable+"\n```", 1)
		} else {
			answer = answer + "\n\n```\n" + appendDataTable + "\n```"
		}
	}

	// If the answer is too long for an in-place update, ask for confirmation
	// before posting multiple messages to the thread.
	const longReplyThreshold = 3900
	if len(answer) > longReplyThreshold {
		chunks := splitIntoChunks(answer, longReplyThreshold)
		pendingKeyTs := contextThreadTs
		if pendingKeyTs == "" {
			pendingKeyTs = originTs
		}
		s.pendingReplies.Store(contextChannel+":"+pendingKeyTs, chunks)
		var confirmMsg string
		if len(chunks) == 1 {
			confirmMsg = "Essa resposta é longa. Posso postar na thread? _(responda *sim* ou *não*)_"
		} else {
			confirmMsg = fmt.Sprintf("Essa resposta precisará de *%d mensagens* para ser enviada por completo. Posso postar tudo? _(responda *sim* ou *não*)_", len(chunks))
		}
		if err := replyFn(confirmMsg); err != nil {
			log.Printf("[ERR] long reply confirmation prompt failed: %v", err)
			return err
		}
		if busyTs != "" {
			s.Slack.Tracker.Track(channel, originTs, busyTs)
		}
		log.Printf("[JARVIS] long reply pending chunks=%d total_chars=%d dur=%s", len(chunks), len(answer), time.Since(start))
		return nil
	}

	if err := replyFn(answer); err != nil {
		log.Printf("[ERR] postSlackMessage failed: %v", err)
		return err
	}
	if busyTs != "" {
		s.Slack.Tracker.Track(channel, originTs, busyTs)
	}
	log.Printf("[JARVIS] done dur=%s answer_len=%d", time.Since(start), len(answer))
	return nil
}
