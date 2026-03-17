package app

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/fileserver"
	"github.com/DanielFillol/Jarvis/internal/googledrive"
	"github.com/DanielFillol/Jarvis/internal/hubspot"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
	"github.com/DanielFillol/Jarvis/internal/outline"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
	"github.com/DanielFillol/Jarvis/internal/telemetry"
)

// pendingReply holds the message chunks for a long answer awaiting confirmation
// together with the originTs of the original question that triggered the answer.
// Storing originalOriginTs allows all posted chunks to be tracked for deletion
// cascade even when the user confirms with a separate "sim" message.
type pendingReply struct {
	chunks           []string
	originalOriginTs string
}

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

	// pendingReplies maps "channel:threadTs" → pendingReply for long answers
	// awaiting user confirmation before being posted to the thread.
	pendingReplies sync.Map

	Store       state.Store
	FileServer  *fileserver.FileServer
	Outline     *outline.Client
	GoogleDrive *googledrive.Client
	HubSpot     *hubspot.Client
	Telemetry   *telemetry.Client

	// companyCtx holds the generated domain glossary injected into every answer call.
	companyCtx atomic.Value
}

// NewService constructs a new Jarvis service from its dependencies.
// metabaseClient may be nil when Metabase integration is not configured.
// fs may be nil when CSV export is not needed.
// outlineClient may be nil when Outline integration is not configured.
// googleDriveClient may be nil when Google Drive integration is not configured.
// hubspotClient may be nil when HubSpot integration is not configured.
// telemetryClient may be nil when telemetry is not configured.
func NewService(cfg config.Config, slackClient *slack.Client, jiraClient *jira.Client, llmClient *llm.Client, metabaseClient *metabase.Client, fs *fileserver.FileServer, outlineClient *outline.Client, googleDriveClient *googledrive.Client, hubspotClient *hubspot.Client, telemetryClient *telemetry.Client) *Service {
	return &Service{
		Slack:       slackClient,
		Jira:        jiraClient,
		LLM:         llmClient,
		Metabase:    metabaseClient,
		Cfg:         cfg,
		FileServer:  fs,
		Store:       *state.NewStore(2 * time.Hour),
		Outline:     outlineClient,
		GoogleDrive: googleDriveClient,
		HubSpot:     hubspotClient,
		Telemetry:   telemetryClient,
	}
}

// SetCompanyCtx stores the generated domain glossary for injection into answer calls.
func (s *Service) SetCompanyCtx(ctx string) { s.companyCtx.Store(ctx) }

func (s *Service) getCompanyCtx() string {
	if v := s.companyCtx.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// HandleMessage processes an incoming message directed at the bot.  It
// delegates to the appropriate flows: Jira creation, context retrieval, and
// answer generation.  All routing decisions go through the LLM — there are no
// hardcoded keyword overrides.  On error, a fallback answer is posted to Slack.
func (s *Service) HandleMessage(channel, threadTs, originTs, originalText, question, senderUserID string, files []slack.File) error {
	start := time.Now()
	log.Printf("[JARVIS] start question=%q originTs=%q senderUserID=%q", preview(question, 180), originTs, senderUserID)

	// Telemetry: populate base fields now; remaining fields are filled in-line
	// and the event is recorded (fire-and-forget) when HandleMessage returns.
	telEvent := telemetry.Event{
		Channel:      channel,
		ChannelType:  channelType(channel),
		ThreadTs:     threadTs,
		OriginTs:     originTs,
		SenderUserID: senderUserID,
		QuestionLen:  len(question),
		FileCount:    len(files),
		LLMModel:     s.Cfg.OpenAIModel,
		Success:      true,
	}
	defer func() {
		telEvent.DurationMs = int(time.Since(start).Milliseconds())
		s.Telemetry.Record(telEvent)
	}()

	// 1) Resolve thread context: current thread vs. explicit Slack permalink.
	contextChannel := channel
	contextThreadTs := threadTs
	hasThreadPermalink := false
	if link, ok := parse.ExtractSlackThreadPermalink(originalText); ok {
		contextChannel = link.ChannelID
		contextThreadTs = link.MessageTs
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
			pr := raw.(pendingReply)
			if isLongReplyCancellation(question) {
				s.pendingReplies.Delete(pendingKey)
				_ = s.Slack.PostMessage(channel, threadTs, "Ok, resposta cancelada.")
				log.Printf("[JARVIS] long reply cancelled dur=%s", time.Since(start))
				return nil
			}
			if isLongReplyConfirmation(question) {
				s.pendingReplies.Delete(pendingKey)
				for i, chunk := range pr.chunks {
					chunkTs, postErr := s.Slack.PostMessageAndGetTS(channel, threadTs, chunk)
					if postErr != nil {
						log.Printf("[ERR] long reply chunk %d/%d: %v", i+1, len(pr.chunks), postErr)
					} else if chunkTs != "" {
						// Track each chunk against the original question so deleting it
						// removes all reply chunks from the thread.
						s.Slack.Tracker.Track(channel, pr.originalOriginTs, chunkTs)
					}
				}
				log.Printf("[JARVIS] long reply confirmed, posted %d chunks dur=%s", len(pr.chunks), time.Since(start))
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

	// 3b) Intro request: user asked the bot to introduce itself.
	if isIntroRequest(question, s.Cfg.BotName) {
		log.Printf("[JARVIS] introFlow handled dur=%s", time.Since(start))
		return s.handleIntroRequest(channel, threadTs, originTs)
	}

	// 4) Unified action dispatcher: one LLM call determines every skill needed.
	// Resolve Slack mentions before passing to the router.
	questionForLLM := s.Slack.ResolveUserMentions(s.Slack.ResolveChannelMentions(parse.StripSlackPermalinks(question)))
	hasPending := s.Cfg.JiraEnabled() && s.Store.Load(channel, threadTs) != nil
	storedDBID, _ := s.loadThreadDBID(contextChannel, contextThreadTs)

	hubspotCatalog := ""
	if s.HubSpot != nil {
		hubspotCatalog = s.HubSpot.CatalogCompact
	}
	actions, actErr := s.LLM.DecideActions(
		questionForLLM, threadHist, s.Cfg.OpenAIModel,
		s.Cfg.JiraEnabled(), s.Jira.CatalogCompact, senderUserID,
		s.formattedMetabaseDatabases(), storedDBID,
		s.Cfg.OutlineEnabled(),
		s.Cfg.GoogleDriveEnabled(),
		s.Cfg.HubSpotEnabled(),
		hubspotCatalog,
	)
	if actErr != nil {
		log.Printf("[WARN] decideActions failed: %v", actErr)
		actions = fallbackActions(hasThreadPermalink)
	}
	telEvent.Actions = actionKinds(actions)
	// Explicit permalink → thread is already the authoritative context; drop Slack searches.
	if hasThreadPermalink {
		filtered := actions[:0]
		for _, a := range actions {
			if a.Kind != llm.ActionSlackSearch {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	log.Printf("[JARVIS] actions=%v hasPending=%t", actionKinds(actions), hasPending)

	handlerActions, contextActions := splitActions(actions)

	// Pass 1 — handler actions run before the busy placeholder and may consume the message.
	// When context actions follow, quiet=true suppresses direct posts so handler confirmation
	// text is collected and prepended to the single combined answer instead.
	quiet := len(contextActions) > 0
	var createdKey string
	var anyHandled bool
	var handlerReplyParts []string

	if containsKind(handlerActions, llm.ActionJiraCreate) || hasPending {
		res, createErr := s.maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist,
			containsKind(handlerActions, llm.ActionJiraCreate), quiet)
		if res.Handled {
			anyHandled = true
			createdKey = res.CreatedKey
			if res.Reply != "" {
				handlerReplyParts = append(handlerReplyParts, res.Reply)
			}
			if createErr != nil {
				log.Printf("[JARVIS] jiraCreateFlow handled dur=%s", time.Since(start))
				return createErr
			}
		}
	}

	// Do not run jira_edit when create was handled without a card being produced
	// (e.g. bot is asking for missing fields). Once a card is actually created
	// (createdKey != ""), edit may run to apply extra fields from the same request.
	pendingCreateHandled := anyHandled && createdKey == ""
	if containsKind(handlerActions, llm.ActionJiraEdit) && !pendingCreateHandled {
		editRes, editErr := s.maybeHandleJiraEditFlows(channel, threadTs, senderUserID, question, threadHist, createdKey, true, quiet)
		if editRes.Handled {
			anyHandled = true
			if editRes.Reply != "" {
				handlerReplyParts = append(handlerReplyParts, editRes.Reply)
			}
			if editErr != nil {
				log.Printf("[JARVIS] jiraEditFlow handled dur=%s", time.Since(start))
				return editErr
			}
		}
	}

	if anyHandled && len(contextActions) == 0 {
		log.Printf("[JARVIS] handlerFlows handled dur=%s", time.Since(start))
		return nil
	}

	// 4b) Smoke-test command: run the prompt library test cycle.
	if isTestCommand(question, s.Cfg.BotName) {
		log.Printf("[JARVIS] testFlow triggered dur=%s", time.Since(start))
		return s.handleTestFlow(channel, threadTs)
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

	// Pass 2 — context actions: fetch external data, then call the answer LLM.
	var slackCtxParts []string
	var jiraCtxParts []string
	var dbCtxParts []string
	var dbQueryResults []*metabase.QueryResult
	var dbQueryActions []llm.ActionDescriptor
	var outlineCtx, outlineSources string
	var googleDriveCtx, googleDriveSources string
	var hubspotCtx, hubspotSources string
	var executedSlackSearch, executedJiraSearch bool
	var slackMatches, jiraIssuesFound int
	var csvDownloadLine string // set when a CSV file is generated; appended to answer unconditionally

	for _, action := range contextActions {
		switch action.Kind {

		case llm.ActionSlackSearch:
			if strings.TrimSpace(action.Query) == "" || s.Cfg.SlackUserToken == "" {
				continue
			}
			executedSlackSearch = true
			telEvent.SlackSearched = true
			unresolvedUserIDs := extractFromUserIDs(action.Query)
			resolvedQuery := s.Slack.ResolveUserIDsInQuery(action.Query)
			log.Printf("[JARVIS] slackSearch query=%q", resolvedQuery)
			matches, sErr := s.Slack.SearchMessagesAll(resolvedQuery)
			if sErr != nil {
				log.Printf("[WARN] slack search failed: %v", sErr)
				break
			}
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
			if len(matches) > 0 {
				slackMatches += len(matches)
				telEvent.SlackMatches += len(matches)
				ctx := buildSlackContext(matches, 25)
				log.Printf("[JARVIS] slackContext matches=%d chars=%d", len(matches), len(ctx))
				slackCtxParts = append(slackCtxParts, ctx)
			} else {
				log.Printf("[JARVIS] slackSearch query=%q returned 0 results", resolvedQuery)
			}

		case llm.ActionJiraSearch:
			executedJiraSearch = true
			telEvent.JiraSearched = true
			jql := strings.TrimSpace(action.JQL)
			if jql == "" {
				jql = defaultJQLForIntent(action.JiraIntent, question, s.Cfg.JiraProjectKeys)
			}
			jql = sanitizeJQL(jql)
			log.Printf("[JARVIS] jiraJQL=%q", jql)
			issues, jErr := s.Jira.FetchAll(jql, 200)
			if jErr != nil {
				log.Printf("[WARN] jira search failed: %v", jErr)
				// Attempt JQL correction using real workflow statuses from catalog.
				if corrected := correctJQLStatus(jql, s.Jira.WorkflowStatuses); corrected != jql {
					log.Printf("[JARVIS] jiraJQL corrected=%q", corrected)
					issues, jErr = s.Jira.FetchAll(corrected, 200)
				}
			}
			if jErr != nil {
				telEvent.JiraError = true
				jiraCtxParts = append(jiraCtxParts,
					"[JIRA_ERROR: A busca falhou. NÃO invente issues, títulos, assignees ou chaves. "+
						"Informe o usuário que houve um erro ao consultar o Jira e peça para refinar a busca.]")
				break
			}
			if len(issues) == 0 {
				jiraCtxParts = append(jiraCtxParts,
					fmt.Sprintf("[JIRA_EMPTY: JQL '%s' retornou 0 issues. "+
						"Informe o usuário que não foram encontradas issues com esses critérios. "+
						"NÃO invente issues.]", jql))
				break
			}
			jiraIssuesFound += len(issues)
			telEvent.JiraIssues += len(issues)
			ctx := buildJiraContext(issues, 40)
			log.Printf("[JARVIS] jiraContext issues=%d chars=%d", len(issues), len(ctx))
			jiraCtxParts = append(jiraCtxParts, ctx)

		case llm.ActionOutlineSearch:
			telEvent.OutlineSearched = true
			outlineQuery := strings.TrimSpace(action.Query)
			if outlineQuery == "" {
				outlineQuery = s.LLM.GenerateOutlineQuery(questionForLLM, s.Cfg.OpenAILesserModel)
				if outlineQuery != "" {
					log.Printf("[JARVIS] outlineQuery generated=%q", outlineQuery)
				}
			}
			if s.Outline != nil && outlineQuery != "" {
				log.Printf("[JARVIS] outlineSearch query=%q", outlineQuery)
				results, oErr := s.Outline.SearchDocuments(outlineQuery, 5)
				if oErr != nil {
					log.Printf("[WARN] outline search failed: %v", oErr)
				} else {
					outlineCtx = outline.FormatContext(results, 8000)
					outlineSources = outline.FormatSources(results)
					log.Printf("[JARVIS] outlineContext docs=%d chars=%d", len(results), len(outlineCtx))
					if outlineCtx == "" {
						outlineCtx = "[AVISO: A busca no Outline não retornou documentos. Informe ao usuário que não foram encontrados docs relevantes para a consulta realizada.]"
					}
				}
			}

		case llm.ActionGoogleDriveSearch:
			driveQuery := strings.TrimSpace(action.Query)
			if s.GoogleDrive != nil && driveQuery != "" {
				log.Printf("[JARVIS] googleDriveSearch query=%q", driveQuery)
				results, dErr := s.GoogleDrive.SearchAndFetch(driveQuery)
				if dErr != nil {
					log.Printf("[WARN] google drive search failed: %v", dErr)
				} else {
					googleDriveCtx = googledrive.FormatContext(results, 8000)
					googleDriveSources = googledrive.FormatSources(results)
					log.Printf("[JARVIS] googleDriveContext files=%d chars=%d", len(results), len(googleDriveCtx))
					if googleDriveCtx == "" {
						googleDriveCtx = "[AVISO: A busca no Google Drive não retornou arquivos. Informe ao usuário que não foram encontrados documentos relevantes para a consulta realizada.]"
					}
				}
			}

		case llm.ActionHubSpotSearch:
			if s.HubSpot == nil {
				break
			}
			objectType := strings.TrimSpace(action.HubSpotObjectType)
			query := strings.TrimSpace(action.HubSpotQuery)
			if query == "" {
				query = question
			}
			log.Printf("[JARVIS] hubspotSearch object_type=%q query=%q after=%q before=%q", objectType, query, action.HubSpotAfter, action.HubSpotBefore)
			results, hErr := s.HubSpot.Search(objectType, query, action.HubSpotAfter, action.HubSpotBefore)
			if hErr != nil {
				log.Printf("[WARN] hubspot search failed: %v", hErr)
				hubspotCtx = "[HUBSPOT_ERROR: busca falhou. NÃO invente dados de CRM.]"
			} else if len(results) == 0 {
				// Ask LLM to generate alternative query variants and retry.
				variants := s.LLM.GenerateHubSpotQueryVariants(query, questionForLLM, s.Cfg.OpenAILesserModel)
				log.Printf("[JARVIS] hubspotSearch empty, LLM variants=%v", variants)
				for _, v := range variants {
					if strings.TrimSpace(v) == "" || v == query {
						continue
					}
					log.Printf("[JARVIS] hubspotSearch retry variant=%q", v)
					results, hErr = s.HubSpot.Search(objectType, v, action.HubSpotAfter, action.HubSpotBefore)
					if hErr != nil {
						log.Printf("[WARN] hubspot search failed variant=%q: %v", v, hErr)
						break
					}
					if len(results) > 0 {
						log.Printf("[JARVIS] hubspotContext records=%d chars=%d (variant=%q)", len(results), len(hubspot.FormatContext(results, 4000)), v)
						break
					}
				}
				if len(results) == 0 {
					hubspotCtx = "[HUBSPOT_EMPTY: nenhum resultado encontrado no HubSpot.]"
				} else {
					hubspotCtx = hubspot.FormatContext(results, 4000)
					hubspotSources = hubspot.FormatSources(results)
				}
			} else {
				hubspotCtx = hubspot.FormatContext(results, 4000)
				hubspotSources = hubspot.FormatSources(results)
				log.Printf("[JARVIS] hubspotContext records=%d chars=%d", len(results), len(hubspotCtx))
			}

		case llm.ActionMetabaseQuery:
			telEvent.MetabaseQueried = true
			if s.Metabase == nil {
				break
			}
			var baseSQL string
			if v, ok := s.threadLastSQL.Load(contextChannel + ":" + contextThreadTs); ok {
				if sql, ok2 := v.(string); ok2 {
					baseSQL = sql
				}
			}
			mRes := s.runMetabaseQuery(questionForLLM, threadHist, action.MetabaseDatabaseID, baseSQL, action.WantsAllRows)

			// Cross-database fallback: when primary DB returned no data, failed entirely,
			// OR returned a clarification about a missing table/schema — try remaining
			// databases before asking the user.  A clarification about which DB to use
			// is a signal that the schema doesn't match; another DB may answer correctly.
			primaryNeedsRetry := mRes.DBCtx == "" ||
				strings.HasPrefix(mRes.DBCtx, llm.ClarificationPrefix) ||
				(mRes.QueryResult != nil && len(mRes.QueryResult.Data.Rows) == 0)
			if primaryNeedsRetry {
				for _, db := range s.Metabase.Databases {
					if db.ID == action.MetabaseDatabaseID {
						continue
					}
					log.Printf("[METABASE] primary db=%d needs retry (clarification/empty), trying fallback db=%d (%s)",
						action.MetabaseDatabaseID, db.ID, db.Name)
					fbRes := s.runMetabaseQuery(questionForLLM, threadHist, db.ID, "", action.WantsAllRows)
					if fbRes.QueryResult != nil && len(fbRes.QueryResult.Data.Rows) > 0 {
						mRes = fbRes
						action.MetabaseDatabaseID = db.ID
						log.Printf("[METABASE] fallback db=%d succeeded rows=%d", db.ID, len(fbRes.QueryResult.Data.Rows))
						break
					}
				}
			}

			thisDBCtx, thisQR, thisSql := mRes.DBCtx, mRes.QueryResult, mRes.ExecutedSQL
			if thisQR != nil {
				telEvent.MetabaseRows += len(thisQR.Data.Rows)
			}
			if strings.HasPrefix(thisDBCtx, llm.ClarificationPrefix) {
				clarificationQ := strings.TrimPrefix(thisDBCtx, llm.ClarificationPrefix)
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
			if thisSql != "" {
				s.threadLastSQL.Store(contextChannel+":"+contextThreadTs, thisSql)
				s.storeThreadDBID(contextChannel, contextThreadTs, action.MetabaseDatabaseID)
			}

			// CSV export or large result handling.
			const largeResultThreshold = 30
			// Auto-trigger CSV when the result is large enough that an inline table
			// would be unreadably long (> 100 rows), even if the user didn't explicitly
			// request a CSV export.
			const csvAutoThreshold = 100
			wantsCSV := action.WantsCSVExport || (thisQR != nil && len(thisQR.Data.Rows) > csvAutoThreshold && action.WantsAllRows)
			if thisQR != nil && wantsCSV && s.FileServer != nil && strings.TrimSpace(s.Cfg.PublicBaseURL) != "" {
				nRows := len(thisQR.Data.Rows)
				cols := make([]string, len(thisQR.Data.Cols))
				for i, c := range thisQR.Data.Cols {
					if c.DisplayName != "" {
						cols[i] = c.DisplayName
					} else {
						cols[i] = c.Name
					}
				}
				csvBytes := []byte(metabase.FormatQueryResultAsCSV(*thisQR))
				if len(csvBytes) > 0 {
					fileID := s.FileServer.Store("resultado.csv", csvBytes, time.Hour)
					csvURL := s.Cfg.PublicBaseURL + "/files/" + fileID
					// Store the download line separately so it is always appended to the
					// final answer — never rely on the LLM to include it.
					csvDownloadLine = fmt.Sprintf(":page_facing_up: *Download CSV:* <%s|resultado.csv> _(expira em 1 hora)_", csvURL)
					telEvent.CSVGenerated = true
					dbCtxParts = append(dbCtxParts, fmt.Sprintf(
						"Query SQL retornou %d registros com os campos: %s.\n\n"+
							"INSTRUÇÃO INTERNA: Escreva APENAS 1 frase curta de introdução descrevendo o resultado "+
							"(ex: \"Encontrei %d registros...\"). Não exiba a tabela de dados.\n\n"+
							"Amostra (3 de %d registros):\n%s",
						nRows, strings.Join(cols, ", "), nRows, nRows,
						metabase.FormatQueryResult(*thisQR, 3),
					))
					log.Printf("[JARVIS] CSV generated rows=%d id=%s", nRows, fileID)
				}
				dbQueryResults = append(dbQueryResults, nil) // placeholder; CSV handled in context
				dbQueryActions = append(dbQueryActions, action)
				break
			} else if thisQR != nil && len(thisQR.Data.Rows) > largeResultThreshold && action.WantsAllRows {
				nRows := len(thisQR.Data.Rows)
				cols := make([]string, len(thisQR.Data.Cols))
				for i, c := range thisQR.Data.Cols {
					if c.DisplayName != "" {
						cols[i] = c.DisplayName
					} else {
						cols[i] = c.Name
					}
				}
				dbCtxParts = append(dbCtxParts, fmt.Sprintf(
					"Query SQL retornou %d registros com os campos: %s.\n\n"+
						"INSTRUÇÃO INTERNA: Escreva APENAS 1 frase curta de introdução descrevendo o resultado "+
						"(ex: \"Encontrei %d registros...\"). Finalize com o marcador exato [TABLE] e nada mais.\n\n"+
						"Amostra (3 de %d registros):\n%s",
					nRows, strings.Join(cols, ", "), nRows, nRows,
					metabase.FormatQueryResult(*thisQR, 3),
				))
				log.Printf("[JARVIS] large result bypass: rows=%d", nRows)
				dbQueryResults = append(dbQueryResults, thisQR)
				dbQueryActions = append(dbQueryActions, action)
				break
			}

			if thisDBCtx == "" {
				thisDBCtx = "[ERRO: A consulta ao banco de dados falhou ou não retornou dados. NÃO invente métricas, nomes ou valores. Informe ao usuário que não foi possível obter os dados neste momento e sugira tentar novamente.]"
			}
			dbCtxParts = append(dbCtxParts, thisDBCtx)
			dbQueryResults = append(dbQueryResults, thisQR)
			dbQueryActions = append(dbQueryActions, action)

		case llm.ActionShowSQL:
			var baseSQL string
			if v, ok := s.threadLastSQL.Load(contextChannel + ":" + contextThreadTs); ok {
				if sql, ok2 := v.(string); ok2 {
					baseSQL = sql
				}
			}
			dbID := action.MetabaseDatabaseID
			executedSQL := s.runMetabaseQuery(questionForLLM, threadHist, dbID, baseSQL, false).ExecutedSQL
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
	}

	// Pass 3 — fallback: when all context sources returned empty/error, ask the router
	// again with a note about what was tried, then execute any newly suggested sources.
	if len(contextActions) > 0 && allContextsNegative(hubspotCtx, jiraCtxParts, slackCtxParts, dbCtxParts, outlineCtx, googleDriveCtx) {
		var triedSources []string
		for _, a := range contextActions {
			triedSources = append(triedSources, a.Kind)
		}
		fallbackNote := fmt.Sprintf(
			"[SISTEMA: fontes consultadas retornaram vazio ou erro: %s. Considere fontes alternativas disponíveis.] ",
			strings.Join(triedSources, ", "))
		fallbackActions2, _ := s.LLM.DecideActions(
			fallbackNote+questionForLLM, threadHist, s.Cfg.OpenAIModel,
			s.Cfg.JiraEnabled(), s.Jira.CatalogCompact, senderUserID,
			s.formattedMetabaseDatabases(), storedDBID,
			s.Cfg.OutlineEnabled(), s.Cfg.GoogleDriveEnabled(), s.Cfg.HubSpotEnabled(),
			hubspotCatalog,
		)
		triedKinds := make(map[string]bool)
		for _, a := range contextActions {
			triedKinds[a.Kind] = true
		}
		_, fallbackCtxActions := splitActions(fallbackActions2)
		var newActions []llm.ActionDescriptor
		for _, a := range fallbackCtxActions {
			if !triedKinds[a.Kind] {
				newActions = append(newActions, a)
			}
		}
		log.Printf("[JARVIS] fallbackPass tried=%v new=%v", triedSources, actionKinds(newActions))
		for _, action := range newActions {
			switch action.Kind {
			case llm.ActionSlackSearch:
				if strings.TrimSpace(action.Query) == "" || s.Cfg.SlackUserToken == "" {
					continue
				}
				executedSlackSearch = true
				resolvedQuery := s.Slack.ResolveUserIDsInQuery(action.Query)
				log.Printf("[JARVIS] fallback slackSearch query=%q", resolvedQuery)
				matches, sErr := s.Slack.SearchMessagesAll(resolvedQuery)
				if sErr != nil {
					log.Printf("[WARN] fallback slack search failed: %v", sErr)
					break
				}
				if len(matches) > 0 {
					slackMatches += len(matches)
					slackCtxParts = append(slackCtxParts, buildSlackContext(matches, 25))
					log.Printf("[JARVIS] fallback slackContext matches=%d", len(matches))
				}
			case llm.ActionJiraSearch:
				executedJiraSearch = true
				jql := strings.TrimSpace(action.JQL)
				if jql == "" {
					jql = defaultJQLForIntent(action.JiraIntent, question, s.Cfg.JiraProjectKeys)
				}
				jql = sanitizeJQL(jql)
				log.Printf("[JARVIS] fallback jiraJQL=%q", jql)
				issues, jErr := s.Jira.FetchAll(jql, 200)
				if jErr != nil {
					log.Printf("[WARN] fallback jira search failed: %v", jErr)
					break
				}
				if len(issues) > 0 {
					jiraIssuesFound += len(issues)
					jiraCtxParts = append(jiraCtxParts, buildJiraContext(issues, 40))
					log.Printf("[JARVIS] fallback jiraContext issues=%d", len(issues))
				}
			case llm.ActionOutlineSearch:
				outlineQuery := strings.TrimSpace(action.Query)
				if s.Outline != nil && outlineQuery != "" {
					log.Printf("[JARVIS] fallback outlineSearch query=%q", outlineQuery)
					results, oErr := s.Outline.SearchDocuments(outlineQuery, 5)
					if oErr != nil {
						log.Printf("[WARN] fallback outline search failed: %v", oErr)
					} else {
						outlineCtx = outline.FormatContext(results, 8000)
						outlineSources = outline.FormatSources(results)
						log.Printf("[JARVIS] fallback outlineContext docs=%d", len(results))
					}
				}
			case llm.ActionGoogleDriveSearch:
				driveQuery := strings.TrimSpace(action.Query)
				if s.GoogleDrive != nil && driveQuery != "" {
					log.Printf("[JARVIS] fallback googleDriveSearch query=%q", driveQuery)
					results, dErr := s.GoogleDrive.SearchAndFetch(driveQuery)
					if dErr != nil {
						log.Printf("[WARN] fallback google drive search failed: %v", dErr)
					} else {
						googleDriveCtx = googledrive.FormatContext(results, 8000)
						googleDriveSources = googledrive.FormatSources(results)
						log.Printf("[JARVIS] fallback googleDriveContext files=%d", len(results))
					}
				}
			case llm.ActionHubSpotSearch:
				if s.HubSpot == nil {
					break
				}
				objectType := strings.TrimSpace(action.HubSpotObjectType)
				query := strings.TrimSpace(action.HubSpotQuery)
				if query == "" {
					query = question
				}
				log.Printf("[JARVIS] fallback hubspotSearch object_type=%q query=%q", objectType, query)
				results, hErr := s.HubSpot.Search(objectType, query, action.HubSpotAfter, action.HubSpotBefore)
				if hErr != nil {
					log.Printf("[WARN] fallback hubspot search failed: %v", hErr)
				} else if len(results) == 0 {
					variants := s.LLM.GenerateHubSpotQueryVariants(query, questionForLLM, s.Cfg.OpenAILesserModel)
					log.Printf("[JARVIS] fallback hubspotSearch empty, LLM variants=%v", variants)
					for _, v := range variants {
						if strings.TrimSpace(v) == "" || v == query {
							continue
						}
						results, hErr = s.HubSpot.Search(objectType, v, action.HubSpotAfter, action.HubSpotBefore)
						if hErr != nil {
							log.Printf("[WARN] fallback hubspot search failed variant=%q: %v", v, hErr)
							break
						}
						if len(results) > 0 {
							log.Printf("[JARVIS] fallback hubspotContext records=%d (variant=%q)", len(results), v)
							break
						}
					}
				}
				if len(results) > 0 {
					hubspotCtx = hubspot.FormatContext(results, 4000)
					hubspotSources = hubspot.FormatSources(results)
				}
			}
		}
	}

	// Channel history fallback: runs once after all slack_search actions are done,
	// for raw <#CHANID> mentions that cannot be resolved to names for the search API.
	if executedSlackSearch {
		if chanIDs := extractChannelIDsFromText(question); len(chanIDs) > 0 {
			now := time.Now()
			weekday := int(now.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			weekStart := now.AddDate(0, 0, -(weekday - 1)).Truncate(24 * time.Hour)
			var directMsgs []slack.SearchMessage
			for _, cid := range chanIDs {
				msgs, chErr := s.Slack.GetChannelHistoryForPeriod(cid, weekStart, now, 80)
				if chErr != nil {
					log.Printf("[JARVIS] channelHistory %s failed: %v", cid, chErr)
					continue
				}
				log.Printf("[JARVIS] channelHistory %s messages=%d", cid, len(msgs))
				directMsgs = append(directMsgs, msgs...)
			}
			if len(directMsgs) > 0 {
				histCtx := buildSlackContext(directMsgs, 40)
				slackCtxParts = append(slackCtxParts, histCtx)
				slackMatches += len(directMsgs)
				log.Printf("[JARVIS] channelHistory total=%d chars=%d", len(directMsgs), len(histCtx))
			}
		}
		// If all searches returned nothing, inject a single warning.
		if slackMatches == 0 {
			slackCtxParts = append(slackCtxParts, "[AVISO: A busca no Slack não retornou mensagens. NÃO invente conteúdo de canais ou mensagens. Informe ao usuário que não foram encontrados dados para a busca realizada e sugira alternativas.]")
		}
	}

	// Merge multi-source contexts.
	slackCtx := strings.Join(slackCtxParts, "\n\n")
	jiraCtx := strings.Join(jiraCtxParts, "\n\n")
	dbCtx := strings.Join(dbCtxParts, "\n\n")

	// Prepend HubSpot pipeline/stage ID→label catalog so the LLM can decode
	// numeric dealstage/pipeline IDs in the search results.
	if s.HubSpot != nil && strings.TrimSpace(s.HubSpot.CatalogForLLM) != "" &&
		hubspotCtx != "" && !strings.HasPrefix(hubspotCtx, "[HUBSPOT_ERROR") && !strings.HasPrefix(hubspotCtx, "[HUBSPOT_EMPTY") {
		hubspotCtx = s.HubSpot.CatalogForLLM + "\n\n" + hubspotCtx
	}

	// Build appendDataTable for large inline results.
	var appendDataTable string
	for i, qr := range dbQueryResults {
		if qr == nil {
			continue
		}
		if i < len(dbQueryActions) && dbQueryActions[i].WantsAllRows && len(qr.Data.Rows) > 30 {
			// Cap inline table to 100 rows; larger results should have gone through CSV.
			showRows := len(qr.Data.Rows)
			if showRows > 100 {
				showRows = 100
			}
			appendDataTable += metabase.FormatQueryResult(*qr, showRows)
		}
	}
	_ = dbQueryActions // suppress unused warning if no large results

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

	// 11) Generate the answer with the primary LLM (with retry and fallback).
	answer, err := s.LLM.AnswerWithRetry(
		s.getCompanyCtx(),
		questionForLLM, threadHist, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx, images,
		s.Cfg.OpenAIModel, s.Cfg.OpenAILesserModel, 2, 0,
	)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[ERR] llmAnswer failed: %v", err)
		answer = buildInformativeFallback(executedSlackSearch, slackMatches, executedJiraSearch, jiraIssuesFound, "")
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = buildInformativeFallback(executedSlackSearch, slackMatches, executedJiraSearch, jiraIssuesFound, "")
	}

	// Prepend handler action confirmations (e.g. "Card criado ✅") when they were
	// suppressed in quiet mode so both the handler result and the answer appear in
	// a single Slack message.
	if len(handlerReplyParts) > 0 {
		answer = strings.Join(handlerReplyParts, "\n\n") + "\n\n" + answer
	}

	// Append CSV download link unconditionally when a file was generated.
	// We never rely on the LLM to copy the link from the context.
	if csvDownloadLine != "" {
		answer += "\n\n" + csvDownloadLine
	}

	// Append Outline source links when documentation was used.
	if outlineSources != "" {
		answer += "\n\n" + outlineSources
	}

	// Append Google Drive source links when files were used.
	if googleDriveSources != "" {
		answer += "\n\n" + googleDriveSources
	}

	// Append HubSpot source links when CRM data was used.
	if hubspotSources != "" {
		answer += "\n\n" + hubspotSources
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
		s.pendingReplies.Store(contextChannel+":"+pendingKeyTs, pendingReply{
			chunks:           chunks,
			originalOriginTs: originTs,
		})
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

	telEvent.AnswerLen = len(answer)
	telEvent.Question = question
	telEvent.Answer = answer
	if err := replyFn(answer); err != nil {
		log.Printf("[ERR] postSlackMessage failed: %v", err)
		telEvent.Success = false
		telEvent.ErrorStage = "post_message"
		return err
	}
	if busyTs != "" {
		s.Slack.Tracker.Track(channel, originTs, busyTs)
	}
	log.Printf("[JARVIS] done dur=%s answer_len=%d", time.Since(start), len(answer))
	return nil
}

// splitActions partitions actions into handler actions (reply directly, no busy
// placeholder) and context actions (feed into the answer LLM).
func splitActions(actions []llm.ActionDescriptor) (handlers, context []llm.ActionDescriptor) {
	for _, a := range actions {
		switch a.Kind {
		case llm.ActionJiraCreate, llm.ActionJiraEdit:
			handlers = append(handlers, a)
		default:
			context = append(context, a)
		}
	}
	return
}

// containsKind reports whether any action in the slice has the given kind.
func containsKind(actions []llm.ActionDescriptor, kind string) bool {
	for _, a := range actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// actionKinds extracts the Kind strings from a slice of ActionDescriptors for logging.
func actionKinds(actions []llm.ActionDescriptor) []string {
	kinds := make([]string, len(actions))
	for i, a := range actions {
		kinds[i] = a.Kind
	}
	return kinds
}

// fallbackActions returns the default action set when DecideActions fails.
func fallbackActions(hasThreadPermalink bool) []llm.ActionDescriptor {
	if hasThreadPermalink {
		return nil // thread content is already the context
	}
	return []llm.ActionDescriptor{{Kind: llm.ActionJiraSearch, JiraIntent: "default"}}
}

// channelType returns "dm" for direct-message channels (IDs starting with "D")
// and "channel" for everything else.
func channelType(channel string) string {
	if strings.HasPrefix(channel, "D") {
		return "dm"
	}
	return "channel"
}

// allContextsNegative returns true when every accumulated context is empty or
// contains only an error/empty marker — indicating all sources came up blank.
func allContextsNegative(hubspotCtx string, jiraCtxParts, slackCtxParts, dbCtxParts []string, outlineCtx, googleDriveCtx string) bool {
	isNeg := func(s string) bool {
		return s == "" ||
			strings.HasPrefix(s, "[HUBSPOT_EMPTY") ||
			strings.HasPrefix(s, "[HUBSPOT_ERROR") ||
			strings.HasPrefix(s, "[JIRA_EMPTY") ||
			strings.HasPrefix(s, "[JIRA_ERROR") ||
			strings.HasPrefix(s, "[ERRO:") ||
			strings.HasPrefix(s, "[AVISO:")
	}
	allNeg := func(parts []string) bool {
		if len(parts) == 0 {
			return true
		}
		for _, p := range parts {
			if !isNeg(p) {
				return false
			}
		}
		return true
	}
	return isNeg(hubspotCtx) && allNeg(jiraCtxParts) && len(slackCtxParts) == 0 && allNeg(dbCtxParts) && isNeg(outlineCtx) && isNeg(googleDriveCtx)
}
