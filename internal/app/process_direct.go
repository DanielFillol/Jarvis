package app

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/googledrive"
	"github.com/DanielFillol/Jarvis/internal/hubspot"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
	"github.com/DanielFillol/Jarvis/internal/outline"
)

// DirectFile holds an in-memory uploaded file for the /api/chat endpoint.
type DirectFile struct {
	Name     string
	Mimetype string
	Data     []byte
}

// buildAvailableSources returns a plain-text list of active integrations so
// EnhancePrompt can reference them when rewriting the user's question.
func (s *Service) buildAvailableSources() string {
	var parts []string
	if s.HubSpot != nil {
		parts = append(parts, "- HubSpot CRM")
	}
	if s.Metabase != nil {
		parts = append(parts, "- Metabase (banco de dados SQL)")
	}
	if s.Cfg.JiraEnabled() {
		parts = append(parts, "- Jira")
	}
	if s.Outline != nil {
		parts = append(parts, "- Outline (documentação)")
	}
	if s.GoogleDrive != nil {
		parts = append(parts, "- Google Drive")
	}
	parts = append(parts, "- Slack")
	return strings.Join(parts, "\n")
}

// buildDirectFileContext reuses the same parsers as buildFileContext but skips
// the Slack download step because bytes are already in memory.
func buildDirectFileContext(files []DirectFile) string {
	const maxTotalChars = 100_000
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range files {
		isText := isTextMimetype(f.Mimetype)
		isXLSX := isXLSXMimetype(f.Mimetype)
		isDocx := isDocxMimetype(f.Mimetype)
		isPDF := isPdfMimetype(f.Mimetype)
		if !isText && !isXLSX && !isDocx && !isPDF {
			log.Printf("[DIRECT] skipping unsupported file %q mimetype=%q", f.Name, f.Mimetype)
			continue
		}

		var content string
		var err error
		switch {
		case isXLSX:
			content, err = XlsxBytesToText(f.Data)
			if err != nil {
				log.Printf("[DIRECT] failed to parse xlsx %q: %v", f.Name, err)
				continue
			}
		case isDocx:
			content, err = DocxBytesToText(f.Data)
			if err != nil {
				log.Printf("[DIRECT] failed to parse docx %q: %v", f.Name, err)
				continue
			}
		case isPDF:
			content, err = PdfBytesToText(f.Data)
			if err != nil {
				log.Printf("[DIRECT] failed to parse pdf %q: %v", f.Name, err)
				continue
			}
		default:
			content = string(f.Data)
		}

		remaining := maxTotalChars - b.Len()
		if remaining <= 0 {
			log.Printf("[DIRECT] fileContext cap reached, skipping file %q", f.Name)
			break
		}
		header := fmt.Sprintf("--- arquivo: %s (tipo: %s, tamanho: %d bytes) ---\n", f.Name, f.Mimetype, len(f.Data))
		available := remaining - len(header) - 2
		if available <= 0 {
			break
		}
		b.WriteString(header)
		if len(content) > available {
			log.Printf("[DIRECT] truncating file %q: %d → %d chars", f.Name, len(content), available)
			b.WriteString(content[:available])
			b.WriteString("\n[AVISO: conteúdo truncado por exceder o limite de contexto]\n")
		} else {
			b.WriteString(content)
		}
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// buildDirectImageAttachments filters image/* files and returns llm.ImageAttachment
// slices for the vision API, same as buildImageAttachments but from in-memory bytes.
func buildDirectImageAttachments(files []DirectFile) []llm.ImageAttachment {
	const maxImageBytes = 5 * 1024 * 1024 // 5 MB
	var out []llm.ImageAttachment
	for _, f := range files {
		if !isImageMimetype(f.Mimetype) {
			continue
		}
		if len(f.Data) > maxImageBytes {
			log.Printf("[DIRECT] skipping oversized image %q size=%d", f.Name, len(f.Data))
			continue
		}
		out = append(out, llm.ImageAttachment{
			MimeType: f.Mimetype,
			Name:     f.Name,
			Data:     f.Data,
		})
	}
	return out
}

// ProcessDirect is the transport-agnostic processing pipeline.  It accepts a
// plain question string, optional conversation history, and optional in-memory
// files, runs the full context-retrieval and LLM-answer pipeline, and returns
// the answer text directly.  No Slack calls are made.
func (s *Service) ProcessDirect(question, senderUserID, threadID, historyText string, files []DirectFile) (string, error) {
	start := time.Now()
	log.Printf("[DIRECT] start question=%q threadID=%q senderUserID=%q", preview(question, 180), threadID, senderUserID)

	// Thread context keys for threadLastSQL / threadLastDBID.
	contextChannel := ""
	contextThreadTs := threadID

	threadHist := historyText
	questionForLLM := question

	// Enhance the question to improve routing accuracy before DecideActions.
	questionForLLM = s.LLM.EnhancePrompt(
		questionForLLM,
		threadHist,
		s.buildAvailableSources(),
		s.Cfg.OpenAILesserModel,
	)
	log.Printf("[DIRECT] enhanced question=%q", preview(questionForLLM, 180))

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
		log.Printf("[DIRECT][WARN] decideActions failed: %v", actErr)
		actions = []llm.ActionDescriptor{{Kind: llm.ActionJiraSearch, JiraIntent: "default"}}
	}
	log.Printf("[DIRECT] actions=%v", actionKinds(actions))

	// Only context actions matter for the direct path (skip handler actions).
	_, contextActions := splitActions(actions)

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
	var csvDownloadLine string

	// Pre-step: directly fetch any Google Drive/Sheets URLs present in the message.
	if s.GoogleDrive != nil {
		if driveFileIDs := googledrive.ExtractFileIDsFromText(question); len(driveFileIDs) > 0 {
			detectedSheetName := ""
			if m := reSheetName.FindStringSubmatch(question); len(m) >= 2 {
				detectedSheetName = m[1]
			}
			var directResults []*googledrive.SearchResult
			for _, fileID := range driveFileIDs {
				log.Printf("[DIRECT] googleDriveDirectFetch fileID=%q sheetName=%q", fileID, detectedSheetName)
				r, fErr := s.GoogleDrive.FetchByFileID(fileID, detectedSheetName)
				if fErr != nil {
					log.Printf("[DIRECT][WARN] googleDriveDirectFetch failed: %v", fErr)
					continue
				}
				directResults = append(directResults, r)
			}
			if len(directResults) > 0 {
				googleDriveCtx = googledrive.FormatContext(directResults, 50000)
				googleDriveSources = googledrive.FormatSources(directResults)
				log.Printf("[DIRECT] googleDriveDirectFetch files=%d chars=%d", len(directResults), len(googleDriveCtx))
			}
		}
	}

	for _, action := range contextActions {
		switch action.Kind {

		case llm.ActionSlackSearch:
			if strings.TrimSpace(action.Query) == "" || s.Cfg.SlackUserToken == "" || s.Slack == nil {
				continue
			}
			executedSlackSearch = true
			unresolvedUserIDs := extractFromUserIDs(action.Query)
			resolvedQuery := s.Slack.ResolveUserIDsInQuery(action.Query)
			log.Printf("[DIRECT] slackSearch query=%q", resolvedQuery)
			matches, sErr := s.Slack.SearchMessagesAll(resolvedQuery)
			if sErr != nil {
				log.Printf("[DIRECT][WARN] slack search failed: %v", sErr)
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
				matches = filtered
			}
			if len(matches) > 0 {
				slackMatches += len(matches)
				ctx := buildSlackContext(matches, 25)
				slackCtxParts = append(slackCtxParts, ctx)
			}

		case llm.ActionJiraSearch:
			executedJiraSearch = true
			jql := strings.TrimSpace(action.JQL)
			if jql == "" {
				jql = defaultJQLForIntent(action.JiraIntent, question, s.Cfg.JiraProjectKeys)
			}
			jql = sanitizeJQL(jql)
			log.Printf("[DIRECT] jiraJQL=%q", jql)
			issues, jErr := s.Jira.FetchAll(jql, 200)
			if jErr != nil {
				if corrected := correctJQLStatus(jql, s.Jira.WorkflowStatuses); corrected != jql {
					issues, jErr = s.Jira.FetchAll(corrected, 200)
				}
			}
			if jErr != nil {
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
			jiraCtxParts = append(jiraCtxParts, buildJiraContext(issues, 40))

		case llm.ActionOutlineSearch:
			outlineQuery := strings.TrimSpace(action.Query)
			if outlineQuery == "" {
				outlineQuery = s.LLM.GenerateOutlineQuery(questionForLLM, s.Cfg.OpenAILesserModel)
			}
			if s.Outline != nil && outlineQuery != "" {
				log.Printf("[DIRECT] outlineSearch query=%q", outlineQuery)
				results, oErr := s.Outline.SearchDocuments(outlineQuery, 5)
				if oErr != nil {
					log.Printf("[DIRECT][WARN] outline search failed: %v", oErr)
				} else {
					outlineCtx = outline.FormatContext(results, 8000)
					outlineSources = outline.FormatSources(results)
					if outlineCtx == "" {
						outlineCtx = "[AVISO: A busca no Outline não retornou documentos. Informe ao usuário que não foram encontrados docs relevantes para a consulta realizada.]"
					}
				}
			}

		case llm.ActionGoogleDriveSearch:
			driveQuery := strings.TrimSpace(action.Query)
			if s.GoogleDrive != nil && driveQuery != "" {
				log.Printf("[DIRECT] googleDriveSearch query=%q sheetName=%q", driveQuery, action.GoogleDriveSheetName)
				results, dErr := s.GoogleDrive.SearchAndFetch(driveQuery, action.GoogleDriveSheetName)
				if dErr != nil {
					log.Printf("[DIRECT][WARN] google drive search failed: %v", dErr)
				} else {
					googleDriveCtx = googledrive.FormatContext(results, 30000)
					googleDriveSources = googledrive.FormatSources(results)
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
			// ID-based lookup: try FetchByID first when a record ID is provided.
			if action.HubSpotRecordID != "" {
				typesToTry := []string{objectType}
				if objectType == "" {
					typesToTry = []string{"tickets", "deals", "contacts", "companies"}
				}
				log.Printf("[DIRECT] hubspotFetchByID id=%q types=%v", action.HubSpotRecordID, typesToTry)
				for _, ot := range typesToTry {
					r, fErr := s.HubSpot.FetchByID(ot, action.HubSpotRecordID)
					if fErr != nil {
						log.Printf("[DIRECT][WARN] hubspot FetchByID type=%s id=%s: %v", ot, action.HubSpotRecordID, fErr)
						continue
					}
					if r != nil {
						hubspotCtx = hubspot.FormatContext([]*hubspot.SearchResult{r}, 8000)
						hubspotSources = hubspot.FormatSources([]*hubspot.SearchResult{r})
						log.Printf("[DIRECT] hubspotFetchByID found type=%s id=%s chars=%d", ot, action.HubSpotRecordID, len(hubspotCtx))
						break
					}
				}
				if hubspotCtx != "" {
					break // found by ID — skip text search
				}
				// Fall through to text search. Only use the bare record ID when no
				// richer query is available; keep the full question when query == question
				// so text search has more tokens to match against.
				if query == "" {
					query = action.HubSpotRecordID
				}
			}
			log.Printf("[DIRECT] hubspotSearch object_type=%q query=%q", objectType, query)
			results, hErr := s.HubSpot.Search(objectType, query, action.HubSpotAfter, action.HubSpotBefore)
			if hErr != nil {
				log.Printf("[DIRECT][WARN] hubspot search failed: %v", hErr)
				hubspotCtx = "[HUBSPOT_ERROR: busca falhou. NÃO invente dados de CRM.]"
			} else if len(results) == 0 {
				variants := s.LLM.GenerateHubSpotQueryVariants(query, questionForLLM, s.Cfg.OpenAILesserModel)
				for _, v := range variants {
					if strings.TrimSpace(v) == "" || v == query {
						continue
					}
					results, hErr = s.HubSpot.Search(objectType, v, action.HubSpotAfter, action.HubSpotBefore)
					if hErr != nil {
						break
					}
					if len(results) > 0 {
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
			}

		case llm.ActionMetabaseQuery:
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

			primaryNeedsRetry := mRes.DBCtx == "" ||
				strings.HasPrefix(mRes.DBCtx, llm.ClarificationPrefix) ||
				(mRes.QueryResult != nil && len(mRes.QueryResult.Data.Rows) == 0)
			if primaryNeedsRetry {
				for _, db := range s.Metabase.Databases {
					if db.ID == action.MetabaseDatabaseID {
						continue
					}
					fbRes := s.runMetabaseQuery(questionForLLM, threadHist, db.ID, "", action.WantsAllRows)
					if fbRes.QueryResult != nil && len(fbRes.QueryResult.Data.Rows) > 0 {
						mRes = fbRes
						action.MetabaseDatabaseID = db.ID
						break
					}
				}
			}

			thisDBCtx, thisQR, thisSql := mRes.DBCtx, mRes.QueryResult, mRes.ExecutedSQL

			// For clarification requests, return the question directly as the answer.
			if strings.HasPrefix(thisDBCtx, llm.ClarificationPrefix) {
				clarificationQ := strings.TrimPrefix(thisDBCtx, llm.ClarificationPrefix)
				log.Printf("[DIRECT] clarification requested dur=%s", time.Since(start))
				return clarificationQ, nil
			}
			if thisSql != "" {
				s.threadLastSQL.Store(contextChannel+":"+contextThreadTs, thisSql)
				s.storeThreadDBID(contextChannel, contextThreadTs, action.MetabaseDatabaseID)
			}

			const largeResultThreshold = 30
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
					csvDownloadLine = fmt.Sprintf("Download CSV: %s (expira em 1 hora)", csvURL)
					dbCtxParts = append(dbCtxParts, fmt.Sprintf(
						"Query SQL retornou %d registros com os campos: %s.\n\n"+
							"INSTRUÇÃO INTERNA: Escreva APENAS 1 frase curta de introdução descrevendo o resultado "+
							"(ex: \"Encontrei %d registros...\"). Não exiba a tabela de dados.\n\n"+
							"Amostra (3 de %d registros):\n%s",
						nRows, strings.Join(cols, ", "), nRows, nRows,
						metabase.FormatQueryResult(*thisQR, 3),
					))
				}
				dbQueryResults = append(dbQueryResults, nil)
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
			// Not meaningful without a Slack thread; skip.
			continue
		}
	}

	// Pass 3 — fallback: when all context sources returned empty/error.
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
		for _, action := range fallbackCtxActions {
			if triedKinds[action.Kind] {
				continue
			}
			switch action.Kind {
			case llm.ActionSlackSearch:
				if strings.TrimSpace(action.Query) == "" || s.Cfg.SlackUserToken == "" || s.Slack == nil {
					continue
				}
				executedSlackSearch = true
				resolvedQuery := s.Slack.ResolveUserIDsInQuery(action.Query)
				matches, sErr := s.Slack.SearchMessagesAll(resolvedQuery)
				if sErr != nil {
					break
				}
				if len(matches) > 0 {
					slackMatches += len(matches)
					slackCtxParts = append(slackCtxParts, buildSlackContext(matches, 25))
				}
			case llm.ActionJiraSearch:
				executedJiraSearch = true
				jql := strings.TrimSpace(action.JQL)
				if jql == "" {
					jql = defaultJQLForIntent(action.JiraIntent, question, s.Cfg.JiraProjectKeys)
				}
				jql = sanitizeJQL(jql)
				issues, jErr := s.Jira.FetchAll(jql, 200)
				if jErr == nil && len(issues) > 0 {
					jiraIssuesFound += len(issues)
					jiraCtxParts = append(jiraCtxParts, buildJiraContext(issues, 40))
				}
			case llm.ActionOutlineSearch:
				outlineQuery := strings.TrimSpace(action.Query)
				if s.Outline != nil && outlineQuery != "" {
					results, oErr := s.Outline.SearchDocuments(outlineQuery, 5)
					if oErr == nil {
						outlineCtx = outline.FormatContext(results, 8000)
						outlineSources = outline.FormatSources(results)
					}
				}
			case llm.ActionGoogleDriveSearch:
				driveQuery := strings.TrimSpace(action.Query)
				if s.GoogleDrive != nil && driveQuery != "" {
					results, dErr := s.GoogleDrive.SearchAndFetch(driveQuery, action.GoogleDriveSheetName)
					if dErr == nil {
						googleDriveCtx = googledrive.FormatContext(results, 30000)
						googleDriveSources = googledrive.FormatSources(results)
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
				// ID-based lookup first.
				if action.HubSpotRecordID != "" {
					typesToTry := []string{objectType}
					if objectType == "" {
						typesToTry = []string{"tickets", "deals", "contacts", "companies"}
					}
					for _, ot := range typesToTry {
						r, fErr := s.HubSpot.FetchByID(ot, action.HubSpotRecordID)
						if fErr != nil {
							log.Printf("[DIRECT][WARN] fallback hubspot FetchByID type=%s id=%s: %v", ot, action.HubSpotRecordID, fErr)
							continue
						}
						if r != nil {
							hubspotCtx = hubspot.FormatContext([]*hubspot.SearchResult{r}, 8000)
							hubspotSources = hubspot.FormatSources([]*hubspot.SearchResult{r})
							break
						}
					}
					if hubspotCtx != "" {
						break
					}
					if query == "" {
						query = action.HubSpotRecordID
					}
				}
				results, hErr := s.HubSpot.Search(objectType, query, action.HubSpotAfter, action.HubSpotBefore)
				if hErr == nil && len(results) > 0 {
					hubspotCtx = hubspot.FormatContext(results, 4000)
					hubspotSources = hubspot.FormatSources(results)
				}
			}
		}
	}

	if executedSlackSearch && slackMatches == 0 {
		slackCtxParts = append(slackCtxParts, "[AVISO: A busca no Slack não retornou mensagens. NÃO invente conteúdo de canais ou mensagens. Informe ao usuário que não foram encontrados dados para a busca realizada e sugira alternativas.]")
	}

	slackCtx := strings.Join(slackCtxParts, "\n\n")
	jiraCtx := strings.Join(jiraCtxParts, "\n\n")
	dbCtx := strings.Join(dbCtxParts, "\n\n")

	if s.HubSpot != nil && strings.TrimSpace(s.HubSpot.CatalogForLLM) != "" &&
		hubspotCtx != "" && !strings.HasPrefix(hubspotCtx, "[HUBSPOT_ERROR") && !strings.HasPrefix(hubspotCtx, "[HUBSPOT_EMPTY") {
		hubspotCtx = s.HubSpot.CatalogForLLM + "\n\n" + hubspotCtx
	}

	var appendDataTable string
	for i, qr := range dbQueryResults {
		if qr == nil {
			continue
		}
		if i < len(dbQueryActions) && dbQueryActions[i].WantsAllRows && len(qr.Data.Rows) > 30 {
			showRows := len(qr.Data.Rows)
			if showRows > 100 {
				showRows = 100
			}
			appendDataTable += metabase.FormatQueryResult(*qr, showRows)
		}
	}
	_ = dbQueryActions

	fileCtx := buildDirectFileContext(files)
	images := buildDirectImageAttachments(files)

	answer, err := s.LLM.AnswerWithRetry(
		s.getCompanyCtx(),
		questionForLLM, threadHist, slackCtx, jiraCtx, dbCtx, fileCtx, outlineCtx, googleDriveCtx, hubspotCtx, images,
		s.Cfg.OpenAIModel, s.Cfg.OpenAILesserModel, 2, 0,
	)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[DIRECT][ERR] llmAnswer failed: %v", err)
		answer = buildInformativeFallback(executedSlackSearch, slackMatches, executedJiraSearch, jiraIssuesFound, "")
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = buildInformativeFallback(executedSlackSearch, slackMatches, executedJiraSearch, jiraIssuesFound, "")
	}

	if csvDownloadLine != "" {
		answer += "\n\n" + csvDownloadLine
	}
	if outlineSources != "" {
		answer += "\n\n" + outlineSources
	}
	if googleDriveSources != "" {
		answer += "\n\n" + googleDriveSources
	}
	if hubspotSources != "" {
		answer += "\n\n" + hubspotSources
	}
	if appendDataTable != "" {
		if strings.Contains(answer, "[TABLE]") {
			answer = strings.Replace(answer, "[TABLE]", "\n```\n"+appendDataTable+"\n```", 1)
		} else {
			answer = answer + "\n\n```\n" + appendDataTable + "\n```"
		}
	}

	log.Printf("[DIRECT] done dur=%s answer_len=%d", time.Since(start), len(answer))
	return answer, nil
}
