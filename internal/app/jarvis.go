// internal/app/jarvis.go
package app

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
	pdflib "github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

// Service encapsulates the core orchestration logic of Jarvis.  It
// coordinates between Slack, Jira and the language model to answer
// questions and handle issue creation flows.  The Service does not
// depend on net/http and is therefore easily testable.
type Service struct {
	Slack   *slack.Client
	Jira    *jira.Client
	LLM     *llm.Client
	Store   *state.Store
	Tracker *state.MessageTracker
	Cfg     config.Config
}

// NewService constructs a new Jarvis service from its dependencies.
func NewService(cfg config.Config, slackClient *slack.Client, jiraClient *jira.Client, llmClient *llm.Client, store *state.Store) *Service {
	return &Service{
		Slack:   slackClient,
		Jira:    jiraClient,
		LLM:     llmClient,
		Store:   store,
		Tracker: state.NewMessageTracker(),
		Cfg:     cfg,
	}
}

// HandleMessage processes an incoming message directed at the bot.  It
// delegates to the appropriate flows: Jira creation, issue lookup,
// context retrieval and answer generation.  On error, a fallback
// answer is posted to Slack to provide user feedback.
func (s *Service) HandleMessage(channel, threadTs, originTs, originalText, question, senderUserID string, files []slack.SlackFile) error {
	start := time.Now()
	log.Printf("[JARVIS] start question=%q originTs=%q senderUserID=%q", preview(question, 180), originTs, senderUserID)
	// 0) Early check: bot introduction / capabilities overview
	if isIntroRequest(question) {
		return s.handleIntroRequest(channel, threadTs, originTs)
	}
	// 1) Decide which thread to use as context (current vs permalink)
	contextChannel := channel
	contextThreadTs := threadTs
	hasThreadPermalink := false
	if chFromLink, tsFromLink, ok := parse.ExtractSlackThreadPermalink(originalText); ok {
		contextChannel = chFromLink
		contextThreadTs = tsFromLink
		hasThreadPermalink = true
	}
	// 2) Thread history for memory (full only when explicit permalink)
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
	// 3) High priority: Jira create flows (always refer to the Slack thread where the user spoke)
	handled, err := s.maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist)
	if handled {
		log.Printf("[JARVIS] jiraCreateFlow handled dur=%s", time.Since(start))
		return err
	}
	// 4) Post an immediate "searching…" indicator so the user knows Jarvis is working.
	//    We'll update it in-place once the answer is ready.
	busyTs, busyErr := s.Slack.PostMessageAndGetTS(channel, threadTs, "_buscando..._")
	if busyErr != nil {
		log.Printf("[WARN] could not post busy indicator: %v", busyErr)
	}

	// Helper to deliver the final answer: update the placeholder when possible,
	// fall back to a new message if the update fails or no placeholder was posted.
	replyFn := func(text string) error {
		if busyTs != "" {
			if err := s.Slack.UpdateMessage(channel, busyTs, text); err != nil {
				log.Printf("[WARN] UpdateMessage failed, falling back to PostMessage: %v", err)
				return s.Slack.PostMessage(channel, threadTs, text)
			}
			return nil
		}
		return s.Slack.PostMessage(channel, threadTs, text)
	}

	// 5) Deterministic: issue key in text
	var jiraIssueCtx string
	issueKey := parse.ExtractIssueKey(question)
	if issueKey != "" {
		it, err := s.Jira.GetIssue(issueKey)
		if err != nil {
			log.Printf("[WARN] jira get issue failed: %v", err)
		} else {
			jiraIssueCtx = buildJiraIssueContext(it)
			log.Printf("[JARVIS] jiraIssueContext key=%s chars=%d", issueKey, len(jiraIssueCtx))
		}
	}
	// 6) Decide Slack/Jira search (LLM)
	// Resolve <#CHANID> → #channel-name and <@USERID> → @username for the router and answer LLM.
	questionForLLM := s.Slack.ResolveUserMentions(s.Slack.ResolveChannelMentions(parse.StripSlackPermalinks(question)))
	decision, err := s.LLM.DecideRetrieval(questionForLLM, threadHist, s.Cfg.OpenAIModel, s.Cfg.JiraProjectKeys, senderUserID)
	if err != nil {
		log.Printf("[WARN] decideRetrieval failed: %v", err)
		if hasThreadPermalink {
			// In permalink mode, avoid over-calling Jira when the router fails.
			decision = llm.RetrievalDecision{}
		} else {
			decision = llm.RetrievalDecision{NeedSlack: false, NeedJira: true, JiraIntent: "default"}
		}
	}
	if hasThreadPermalink {
		// Explicit permalink -> thread context is authoritative.
		decision.NeedSlack = false
		decision.SlackQuery = ""
	}
	if jiraIssueCtx != "" {
		decision.NeedJira = false
		decision.JiraJQL = ""
	}
	log.Printf("[JARVIS] needSlack=%t slackQuery=%q needJira=%t jiraIntent=%q jiraJQL=%q issueKey=%q", decision.NeedSlack, preview(decision.SlackQuery, 120), decision.NeedJira, decision.JiraIntent, preview(decision.JiraJQL, 120), issueKey)
	// 7) Slack context
	var slackCtx string
	var slackMatches int
	if decision.NeedSlack && strings.TrimSpace(decision.SlackQuery) != "" && s.Cfg.SlackUserToken != "" {
		// Capture any from:USERID filters before resolution; if users:read scope is
		// missing, ResolveUserIDsInQuery strips them and we fall back to client-side
		// filtering by user ID after the search.
		unresolvedUserIDs := extractFromUserIDs(decision.SlackQuery)
		// Resolve from:USERID → from:@username so Slack search filters correctly.
		decision.SlackQuery = s.Slack.ResolveUserIDsInQuery(decision.SlackQuery)
		log.Printf("[JARVIS] slackSearch query=%q", decision.SlackQuery)
		matches, err := s.Slack.SearchMessagesAll(decision.SlackQuery)
		if err != nil {
			log.Printf("[WARN] slack search failed: %v", err)
		} else {
			// If from:USERID filters were stripped (couldn't resolve username), filter
			// client-side by user ID using the UserID field from the search results.
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
				log.Printf("[JARVIS] clientSideUserFilter from=%v reduced %d→%d matches", unresolvedUserIDs, len(matches), len(filtered))
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
	// 7b) Direct channel history fallback for unresolved <#CHANID> mentions.
	// When channels:read is missing we can't resolve IDs to names for search,
	// but conversations.history (channels:history scope) can fetch by ID directly.
	if decision.NeedSlack {
		if chanIDs := extractChannelIDsFromText(question); len(chanIDs) > 0 {
			// Monday of current week as the oldest boundary.
			now := time.Now()
			weekday := int(now.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			weekStart := now.AddDate(0, 0, -(weekday - 1)).Truncate(24 * time.Hour)
			var directMsgs []slack.SlackSearchMessage
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
	// 8) Jira context via JQL
	var jiraCtx string
	var jiraIssuesFound int
	if decision.NeedJira {
		jql := strings.TrimSpace(decision.JiraJQL)
		if jql == "" {
			jql = defaultJQLForIntent(decision.JiraIntent, question, s.Cfg.JiraProjectKeys)
		}
		jql = sanitizeJQL(jql)
		log.Printf("[JARVIS] jiraJQL (after sanitize)=%q", jql)
		issues, err := s.Jira.FetchAll(jql, 200)
		if err != nil {
			log.Printf("[WARN] jira search failed: %v", err)
		} else {
			jiraIssuesFound = len(issues)
			jiraCtx = buildJiraContext(issues, 40)
			log.Printf("[JARVIS] jiraContext issues=%d chars=%d", jiraIssuesFound, len(jiraCtx))
		}
	}
	finalJiraCtx := jiraIssueCtx
	if finalJiraCtx == "" {
		finalJiraCtx = jiraCtx
	}
	// 9) File context from attachments
	fileCtx := s.buildFileContext(files)
	if fileCtx != "" {
		log.Printf("[JARVIS] fileContext files=%d chars=%d", len(files), len(fileCtx))
	}
	images := s.buildImageAttachments(files)
	if len(images) > 0 {
		log.Printf("[JARVIS] imageAttachments count=%d", len(images))
	}
	// 10) Answer with LLM (with retry for transient errors)
	answer, err := s.LLM.AnswerWithRetry(questionForLLM, threadHist, slackCtx, finalJiraCtx, fileCtx, images, s.Cfg.OpenAIModel, s.Cfg.OpenAIFallbackModel, 2, 0)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[ERR] llmAnswer failed: %v", err)
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, (jiraIssueCtx != "" || decision.NeedJira), jiraIssuesFound, issueKey)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, (jiraIssueCtx != "" || decision.NeedJira), jiraIssuesFound, issueKey)
	}
	if err := replyFn(answer); err != nil {
		log.Printf("[ERR] postSlackMessage failed: %v", err)
		return err
	}
	// Track origin → bot reply so we can delete the reply if the user deletes their message.
	if busyTs != "" {
		s.Tracker.Track(channel, originTs, busyTs)
	}
	log.Printf("[JARVIS] done dur=%s answer_len=%d", time.Since(start), len(answer))
	return nil
}

// maybeHandleJiraCreateFlows orchestrates the state machine for Jira
// creation commands.  It returns handled=true if the message
// corresponds to a Jira creation flow, regardless of whether the
// creation succeeded.  This allows HandleMessage to short-circuit
// further processing when a creation flow is triggered.
func (s *Service) maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist string) (handled bool, err error) {
	q := strings.TrimSpace(question)
	low := strings.ToLower(q)
	// If creation is disabled, short-circuit
	if parse.LooksLikeJiraCreateIntent(low) && !s.Cfg.JiraCreateEnabled {
		_ = s.Slack.PostMessage(channel, threadTs, "Criação de issues no Jira está desabilitada (JIRA_CREATE_ENABLED != true).")
		return true, nil
	}
	// 1) Explicit creation: "jira criar | PROJ | Type | Title | Description..."
	if ok, draft := parse.ParseJiraCreateExplicit(q); ok {
		needProject := strings.TrimSpace(draft.Project) == ""
		needType := strings.TrimSpace(draft.IssueType) == ""
		if needProject || needType {
			s.Store.Save(&state.PendingIssue{
				CreatedAt:    time.Now(),
				Channel:      channel,
				ThreadTs:     threadTs,
				OriginTs:     originTs,
				OriginalText: originalText,
				Source:       "explicit",
				Draft:        draft,
				NeedProject:  needProject,
				NeedType:     needType,
			})
			_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(draft, needProject, needType))
			return true, nil
		}
		// Attach origin and create
		s.appendSlackOrigin(&draft, channel, threadTs, originTs, originalText)
		return true, s.createIssueAndReply(channel, threadTs, draft)
	}
	// 2) Natural language creation: "crie um card...", "crie essa história no JIRA", etc.
	// The heuristic is a fast pre-filter; the LLM confirms to avoid false positives.
	if strings.Contains(low, "crie um card") || strings.Contains(low, "criar um card") || parse.LooksLikeJiraCreateIntent(low) {
		if !s.LLM.ConfirmJiraCreateIntent(q, s.Cfg.OpenAIFallbackModel, s.Cfg.OpenAIModel) {
			log.Printf("[JARVIS] LLM rejected create intent — falling through to Q&A")
			return false, nil
		}
		d := jira.IssueDraft{}
		d.Project = parse.ParseProjectKeyFromText(q)
		d.IssueType = parse.ParseIssueTypeFromText(q)
		d.Summary = parse.ParseSummaryFromText(q)
		// Fetch real Jira examples to inspire the LLM
		exampleIssues := s.fetchExampleIssues(d.Project, d.IssueType)

		dd, derr := s.LLM.ExtractIssueFromThread(threadHist, q, s.Cfg.OpenAIModel, exampleIssues, s.Cfg.JiraProjectNameMap)
		if derr == nil {
			// Fields explicitly provided by the user take absolute priority
			if strings.TrimSpace(d.Project) != "" {
				dd.Project = d.Project
			}
			if strings.TrimSpace(d.IssueType) != "" {
				dd.IssueType = d.IssueType
			}
			if strings.TrimSpace(d.Summary) != "" {
				dd.Summary = d.Summary
			}
			d = dd
		} else {
			d.Description = fmt.Sprintf("Pedido do usuário:\n%s\n\nA confirmar: detalhes adicionais.", q)
		}
		if strings.TrimSpace(d.Summary) == "" {
			d.Summary = "Card criado via Jarvis"
		}
		needProject := strings.TrimSpace(d.Project) == ""
		needType := strings.TrimSpace(d.IssueType) == ""
		if needProject || needType {
			s.Store.Save(&state.PendingIssue{
				CreatedAt:    time.Now(),
				Channel:      channel,
				ThreadTs:     threadTs,
				OriginTs:     originTs,
				OriginalText: originalText,
				Source:       "explicit",
				Draft:        d,
				NeedProject:  needProject,
				NeedType:     needType,
			})
			_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(d, needProject, needType))
			return true, nil
		}
		s.appendSlackOrigin(&d, channel, threadTs, originTs, originalText)
		return true, s.createIssueAndReply(channel, threadTs, d)
	}
	// 3) Thread-based creation: "com base nessa thread crie um card..."
	if parse.IsThreadBasedCreate(q) && s.LLM.ConfirmJiraCreateIntent(q, s.Cfg.OpenAIFallbackModel, s.Cfg.OpenAIModel) {
		// 3a) Multi-card variant: "crie dois cards", "um sobre X e outro Y"
		if parse.IsMultiCardCreate(q) {
			drafts, derr := s.LLM.ExtractMultipleIssuesFromThread(threadHist, q, s.Cfg.OpenAIModel, s.Cfg.JiraProjectNameMap)
			if derr != nil {
				_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui montar os rascunhos dos cards a partir da thread: %v", derr))
				return true, nil
			}
			// Fields explicitly mentioned in the command take priority for all cards
			parsedProject := parse.ParseProjectKeyFromText(q)
			parsedType := parse.ParseIssueTypeFromText(q)
			for i := range drafts {
				if strings.TrimSpace(parsedProject) != "" {
					drafts[i].Project = parsedProject
				}
				if strings.TrimSpace(parsedType) != "" {
					drafts[i].IssueType = parsedType
				}
			}
			for i, d := range drafts {
				log.Printf("[JARVIS] multi-card draft[%d] project=%q type=%q summary=%q", i, d.Project, d.IssueType, d.Summary)
			}
			s.Store.Save(&state.PendingIssue{
				CreatedAt:    time.Now(),
				Channel:      channel,
				ThreadTs:     threadTs,
				OriginTs:     originTs,
				OriginalText: originalText,
				Source:       "thread_based",
				Drafts:       drafts,
			})
			_ = s.Slack.PostMessage(channel, threadTs, previewMultipleDraftsMsg(drafts))
			return true, nil
		}

		// 3b) Single-card variant
		parsedProject := parse.ParseProjectKeyFromText(q)
		parsedType := parse.ParseIssueTypeFromText(q)

		// Fetch real Jira examples to inspire the LLM
		exampleIssues := s.fetchExampleIssues(parsedProject, parsedType)

		draft, derr := s.LLM.ExtractIssueFromThread(threadHist, q, s.Cfg.OpenAIModel, exampleIssues, s.Cfg.JiraProjectNameMap)
		if derr != nil {
			_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui montar o rascunho do card a partir da thread: %v", derr))
			return true, nil
		}
		// Fields explicitly provided in the command take absolute priority
		if strings.TrimSpace(parsedProject) != "" {
			draft.Project = parsedProject
		}
		if strings.TrimSpace(parsedType) != "" {
			draft.IssueType = parsedType
		}
		needProject := strings.TrimSpace(draft.Project) == ""
		needType := strings.TrimSpace(draft.IssueType) == ""
		s.Store.Save(&state.PendingIssue{
			CreatedAt:    time.Now(),
			Channel:      channel,
			ThreadTs:     threadTs,
			OriginTs:     originTs,
			OriginalText: originalText,
			Source:       "thread_based",
			Draft:        draft,
			NeedProject:  needProject,
			NeedType:     needType,
		})
		if needProject || needType {
			_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(draft, needProject, needType))
			return true, nil
		}
		_ = s.Slack.PostMessage(channel, threadTs, previewDraftMsg(draft, true))
		return true, nil
	}
	// 4) Define project/type for pending draft
	if strings.HasPrefix(low, "jira definir") || strings.HasPrefix(low, "jira set") {
		p := s.Store.Load(channel, threadTs)
		if p == nil {
			_ = s.Slack.PostMessage(channel, threadTs, "Não encontrei nenhum rascunho pendente neste thread. Peça: `jarvis: com base nessa thread crie um card no jira`.")
			return true, nil
		}
		updated := parse.ApplyJiraDefine(q, &p.Draft)
		needProject := strings.TrimSpace(p.Draft.Project) == ""
		needType := strings.TrimSpace(p.Draft.IssueType) == ""
		s.Store.Save(&state.PendingIssue{
			CreatedAt:    p.CreatedAt,
			Channel:      p.Channel,
			ThreadTs:     p.ThreadTs,
			OriginTs:     p.OriginTs,
			OriginalText: p.OriginalText,
			Source:       p.Source,
			Draft:        p.Draft,
			NeedProject:  needProject,
			NeedType:     needType,
		})
		if !updated {
			_ = s.Slack.PostMessage(channel, threadTs, "Não consegui ler `projeto=` e/ou `tipo=`. Exemplo: `jarvis: jira definir | projeto=PROJ | tipo=Bug`")
			return true, nil
		}
		if needProject || needType {
			_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(p.Draft, needProject, needType))
			return true, nil
		}
		_ = s.Slack.PostMessage(channel, threadTs, previewDraftMsg(p.Draft, true))
		return true, nil
	}
	// 5) Confirm creation
	if low == "confirmar" || strings.HasPrefix(low, "confirmar ") || low == "jira confirmar" || strings.HasPrefix(low, "jira confirmar") {
		p := s.Store.Load(channel, threadTs)
		if p == nil {
			_ = s.Slack.PostMessage(channel, threadTs, "Não encontrei nenhum rascunho pendente para confirmar neste thread.")
			return true, nil
		}
		// Multi-card flow: create all queued drafts in sequence
		if len(p.Drafts) > 0 {
			for i, d := range p.Drafts {
				s.appendSlackOrigin(&d, channel, threadTs, p.OriginTs, p.OriginalText)
				log.Printf("[JARVIS] multi-card creating %d/%d project=%q type=%q summary=%q", i+1, len(p.Drafts), d.Project, d.IssueType, d.Summary)
				if err := s.createIssueAndReply(channel, threadTs, d); err != nil {
					return true, err
				}
			}
			s.Store.Delete(channel, threadTs)
			return true, nil
		}
		// Single-card flow
		needProject := strings.TrimSpace(p.Draft.Project) == ""
		needType := strings.TrimSpace(p.Draft.IssueType) == ""
		if needProject || needType {
			_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(p.Draft, needProject, needType))
			return true, nil
		}
		// Attach origin and create
		draft := p.Draft
		s.appendSlackOrigin(&draft, channel, threadTs, p.OriginTs, p.OriginalText)
		err := s.createIssueAndReply(channel, threadTs, draft)
		if err == nil {
			s.Store.Delete(channel, threadTs)
		}
		return true, nil
	}
	// 6) Cancel pending
	if strings.Contains(low, "cancelar") && (strings.Contains(low, "card") || strings.Contains(low, "jira")) {
		if s.Store.Load(channel, threadTs) != nil {
			s.Store.Delete(channel, threadTs)
			_ = s.Slack.PostMessage(channel, threadTs, "Ok — rascunho pendente descartado.")
			return true, nil
		}
	}
	return false, nil
}

// appendSlackOrigin appends a "Thread de origem" section to the end of
// the issue description, including Slack permalinks when available.
func (s *Service) appendSlackOrigin(d *jira.IssueDraft, channel, threadTs, originTs, originalText string) {
	var originLink, threadLink string
	if l, err := s.Slack.GetPermalink(channel, originTs); err == nil {
		originLink = l
	}
	if threadTs != "" && threadTs != originTs {
		if l, err := s.Slack.GetPermalink(channel, threadTs); err == nil {
			threadLink = l
		}
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(d.Description))
	b.WriteString("\n\n---\n")
	b.WriteString("Thread de origem\n\n")
	if originLink != "" {
		b.WriteString(fmt.Sprintf("- Mensagem original: %s\n", originLink))
	} else {
		b.WriteString(fmt.Sprintf("- Mensagem original: (link indisponível) ts=%s\n", originTs))
	}
	if threadLink != "" {
		b.WriteString(fmt.Sprintf("- Thread (raiz): %s\n", threadLink))
	}
	if strings.TrimSpace(originalText) != "" {
		b.WriteString(fmt.Sprintf("\n- Comando: %q\n", clip(originalText, 400)))
	}
	d.Description = b.String()
}

// fetchExampleIssues is a helper that fetches real Jira cards from the same project/type
// to serve as inspiration for the LLM. Returns an empty slice on error or missing fields.
func (s *Service) fetchExampleIssues(project, issueType string) []string {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(issueType) == "" {
		return nil
	}
	examples, err := s.Jira.FetchExampleIssues(project, issueType, 3)
	if err != nil {
		log.Printf("[JARVIS] fetchExampleIssues project=%s type=%s err=%v", project, issueType, err)
		return nil
	}
	log.Printf("[JARVIS] fetchExampleIssues loaded=%d project=%s type=%s", len(examples), project, issueType)
	return examples
}

// createIssueAndReply creates a Jira issue via the Jira client and posts
// a confirmation message to Slack.  It returns any error from Jira.
func (s *Service) createIssueAndReply(channel, threadTs string, d jira.IssueDraft) error {
	d.Project = strings.TrimSpace(d.Project)
	d.IssueType = strings.TrimSpace(d.IssueType)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Description = strings.TrimSpace(d.Description)
	if d.Project == "" || d.IssueType == "" {
		_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(d, d.Project == "", d.IssueType == ""))
		return nil
	}
	created, err := s.Jira.CreateIssue(d)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui criar o card no Jira: %v", err))
		return nil
	}
	base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")
	link := base + "/browse/" + created.Key
	_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Card criado ✅ *%s*\n%s", created.Key, link))
	return nil
}

// isTextMimetype reports whether a file MIME type is a supported text format
// that can be safely included in the LLM prompt as raw bytes.
func isTextMimetype(mimetype string) bool {
	mimetype = strings.ToLower(strings.TrimSpace(mimetype))
	if strings.HasPrefix(mimetype, "text/") {
		return true
	}
	switch mimetype {
	case "application/json", "application/xml",
		"application/yaml", "application/x-yaml",
		"application/javascript", "application/typescript":
		return true
	}
	return false
}

// isImageMimetype reports whether the MIME type is a supported image format
// for the OpenAI Vision API (JPEG, PNG, GIF, WebP).
func isImageMimetype(mimetype string) bool {
	switch strings.ToLower(strings.TrimSpace(mimetype)) {
	case "image/jpeg", "image/jpg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// buildImageAttachments downloads image files and returns them as vision
// attachments for the LLM. Images larger than 5 MB are skipped (OpenAI
// base64 limit).
func (s *Service) buildImageAttachments(files []slack.SlackFile) []llm.ImageAttachment {
	const maxImageBytes = 5 * 1024 * 1024 // 5 MB (OpenAI base64 limit)
	var out []llm.ImageAttachment
	for _, f := range files {
		if !isImageMimetype(f.Mimetype) {
			continue
		}
		if f.Size > maxImageBytes {
			log.Printf("[JARVIS] skipping oversized image %q size=%d", f.Name, f.Size)
			continue
		}
		if f.URLPrivateDownload == "" {
			log.Printf("[JARVIS] skipping image %q: no download URL", f.Name)
			continue
		}
		data, err := s.Slack.DownloadFile(f.URLPrivateDownload)
		if err != nil {
			log.Printf("[JARVIS] failed to download image %q: %v", f.Name, err)
			continue
		}
		log.Printf("[JARVIS] image downloaded %q bytes=%d", f.Name, len(data))
		out = append(out, llm.ImageAttachment{MimeType: f.Mimetype, Name: f.Name, Data: data})
	}
	return out
}

// isXLSXMimetype reports whether the MIME type is an Excel spreadsheet.
func isXLSXMimetype(mimetype string) bool {
	mimetype = strings.ToLower(strings.TrimSpace(mimetype))
	return mimetype == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		mimetype == "application/vnd.ms-excel"
}

// isPdfMimetype reports whether the MIME type is a PDF document.
func isPdfMimetype(mimetype string) bool {
	mimetype = strings.ToLower(strings.TrimSpace(mimetype))
	return strings.Contains(mimetype, "pdf")
}

// pdfBytesToText extracts plain text from a PDF file using the ledongthuc/pdf library.
func pdfBytesToText(data []byte) (string, error) {
	r, err := pdflib.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	plain, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("read pdf text: %w", err)
	}
	b, err := io.ReadAll(plain)
	if err != nil {
		return "", fmt.Errorf("read pdf content: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// isDocxMimetype reports whether the MIME type is a Word document.
func isDocxMimetype(mimetype string) bool {
	mimetype = strings.ToLower(strings.TrimSpace(mimetype))
	return strings.Contains(mimetype, "wordprocessingml") ||
		strings.Contains(mimetype, "msword") ||
		strings.HasSuffix(mimetype, ".docx")
}

// docxBytesToText extracts plain text from a DOCX file (which is a ZIP
// containing word/document.xml). It preserves paragraph breaks.
// Uses only the standard library — no external dependency required.
func docxBytesToText(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open docx zip: %w", err)
	}
	for _, f := range r.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open document.xml: %w", err)
		}
		defer rc.Close()
		xmlData, err := io.ReadAll(rc)
		if err != nil {
			return "", fmt.Errorf("read document.xml: %w", err)
		}
		return extractDocxText(xmlData), nil
	}
	return "", fmt.Errorf("word/document.xml not found in docx")
}

// extractDocxText walks the XML token stream of word/document.xml and
// collects text from <w:t> elements, inserting newlines at <w:p> boundaries.
func extractDocxText(data []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p": // paragraph start — add blank line between paragraphs
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
			case "t": // text run
				inText = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
		case xml.CharData:
			if inText {
				b.Write([]byte(t))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// xlsxBytesToText converts raw XLSX bytes into a plain-text table representation
// suitable for inclusion in an LLM prompt.  Each sheet is rendered as a
// tab-separated grid with its name as a header.
func xlsxBytesToText(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("parse xlsx: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		b.WriteString(fmt.Sprintf("=== aba: %s ===\n", sheet))
		for _, row := range rows {
			b.WriteString(strings.Join(row, "\t"))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}

// buildFileContext downloads files attached to the message and formats their
// contents for inclusion in the LLM prompt.
// Supported: text/*, JSON, YAML, XML, JS, TS (raw bytes) and XLSX (parsed as table).
// Files larger than 20 MB are skipped. Total output is capped at 8 M chars to
// stay safely under the OpenAI API limit of ~10 M chars per message.
func (s *Service) buildFileContext(files []slack.SlackFile) string {
	const maxFileBytes = 20 * 1024 * 1024 // 20 MB per file
	const maxTotalChars = 100_000         // ~100 k chars — safe for 128k-token models
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
			log.Printf("[JARVIS] skipping unsupported file %q mimetype=%q", f.Name, f.Mimetype)
			continue
		}
		if f.Size > maxFileBytes {
			log.Printf("[JARVIS] skipping oversized file %q size=%d", f.Name, f.Size)
			continue
		}
		if f.URLPrivateDownload == "" {
			log.Printf("[JARVIS] skipping file %q: no download URL", f.Name)
			continue
		}
		data, err := s.Slack.DownloadFile(f.URLPrivateDownload)
		if err != nil {
			log.Printf("[JARVIS] failed to download file %q: %v", f.Name, err)
			continue
		}

		var content string
		switch {
		case isXLSX:
			content, err = xlsxBytesToText(data)
			if err != nil {
				log.Printf("[JARVIS] failed to parse xlsx %q: %v", f.Name, err)
				continue
			}
		case isDocx:
			content, err = docxBytesToText(data)
			if err != nil {
				log.Printf("[JARVIS] failed to parse docx %q: %v", f.Name, err)
				continue
			}
		case isPDF:
			content, err = pdfBytesToText(data)
			if err != nil {
				log.Printf("[JARVIS] failed to parse pdf %q: %v", f.Name, err)
				continue
			}
		default:
			content = string(data)
		}

		// Enforce total character cap — truncate current file if needed.
		remaining := maxTotalChars - b.Len()
		if remaining <= 0 {
			log.Printf("[JARVIS] fileContext cap reached, skipping file %q", f.Name)
			break
		}
		header := fmt.Sprintf("--- arquivo: %s (tipo: %s, tamanho: %d bytes) ---\n", f.Name, f.Mimetype, f.Size)
		available := remaining - len(header) - 2 // 2 for trailing \n\n
		if available <= 0 {
			break
		}
		b.WriteString(header)
		if len(content) > available {
			log.Printf("[JARVIS] truncating file %q: %d → %d chars", f.Name, len(content), available)
			b.WriteString(content[:available])
			b.WriteString("\n[AVISO: conteúdo truncado por exceder o limite de contexto]\n")
		} else {
			b.WriteString(content)
		}
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// buildSlackContext builds a textual summary of Slack search results.
// It limits the number of matches included to 'limit'.
func buildSlackContext(matches []slack.SlackSearchMessage, limit int) string {
	if limit <= 0 {
		limit = 8
	}
	var b strings.Builder
	for i, m := range matches {
		if i >= limit {
			break
		}
		if m.Permalink != "" {
			b.WriteString(fmt.Sprintf("[#%s] %s: %s\nlink: %s\n\n", m.Channel, m.Username, m.Text, m.Permalink))
		} else {
			b.WriteString(fmt.Sprintf("[#%s] %s: %s\n\n", m.Channel, m.Username, m.Text))
		}
	}
	return b.String()
}

// buildJiraIssueContext builds a textual summary of a single Jira issue.
func buildJiraIssueContext(it jira.JiraIssueResp) string {
	assignee := "Unassigned"
	if it.Fields.Assignee != nil && it.Fields.Assignee.DisplayName != "" {
		assignee = it.Fields.Assignee.DisplayName
	}
	parent := ""
	if it.Fields.Parent != nil {
		parent = fmt.Sprintf("Parent: %s — %s\n", it.Fields.Parent.Key, it.Fields.Parent.Fields.Summary)
	}
	var subs []string
	for _, st := range it.Fields.Subtasks {
		subs = append(subs, fmt.Sprintf("- %s [%s] %s", st.Key, st.Fields.Status.Name, st.Fields.Summary))
	}
	subtxt := ""
	if len(subs) > 0 {
		subtxt = "Subtasks:\n" + strings.Join(subs, "\n") + "\n"
	}
	desc := strings.TrimSpace(it.RenderedFields.Description)
	descTxt := ""
	if desc != "" {
		descTxt = "Description (rendered):\n" + clip(stripHTML(desc), 1800) + "\n"
	} else if it.Fields.Description == nil {
		descTxt = "Description: (vazia)\n"
	} else {
		descTxt = "Description: (ADF presente, mas renderedFields vazio)\n"
	}
	return fmt.Sprintf(
		"Issue: %s\nStatus: %s\nType: %s\nPriority: %s\nAssignee: %s\n%s%s%s",
		it.Key,
		it.Fields.Status.Name,
		it.Fields.IssueType.Name,
		it.Fields.Priority.Name,
		assignee,
		parent,
		descTxt,
		subtxt,
	)
}

// buildJiraContext produces a formatted context summary from a slice
// of Jira issues.  If the number of issues exceeds 'limit' it will
// group by status and summarize counts.
func buildJiraContext(issues []jira.JiraSearchJQLRespIssue, limit int) string {
	if limit <= 0 {
		limit = 40
	}
	if len(issues) <= limit {
		return buildJiraContextSimple(issues)
	}
	return buildJiraContextGrouped(issues)
}

func buildJiraContextSimple(issues []jira.JiraSearchJQLRespIssue) string {
	var b strings.Builder
	for i, it := range issues {
		sprint := ""
		if it.Sprint != "" {
			sprint = " | sprint=" + it.Sprint
		}
		b.WriteString(fmt.Sprintf("%s [%s] (%s) %s — %s | assignee=%s | updated=%s%s\n", it.Key, it.Status, it.Type, it.Priority, it.Summary, it.Assignee, it.Updated, sprint))
		if i >= 39 {
			remaining := len(issues) - 40
			if remaining > 0 {
				b.WriteString(fmt.Sprintf("... e mais %d issues\n", remaining))
			}
			break
		}
	}
	return b.String()
}

func buildJiraContextGrouped(issues []jira.JiraSearchJQLRespIssue) string {
	byStatus := make(map[string][]jira.JiraSearchJQLRespIssue)
	for _, it := range issues {
		byStatus[it.Status] = append(byStatus[it.Status], it)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("TOTAL: %d issues encontradas\n\n", len(issues)))
	type statusCount struct {
		status string
		count  int
	}
	var statusList []statusCount
	for status, list := range byStatus {
		statusList = append(statusList, statusCount{status, len(list)})
	}
	sort.Slice(statusList, func(i, j int) bool { return statusList[i].count > statusList[j].count })
	maxStatusToShow := 6
	maxPerStatus := 6
	for i, sc := range statusList {
		if i >= maxStatusToShow {
			break
		}
		list := byStatus[sc.status]
		b.WriteString(fmt.Sprintf("[%s] (%d issues):\n", sc.status, len(list)))
		for j, it := range list {
			if j >= maxPerStatus {
				remaining := len(list) - maxPerStatus
				b.WriteString(fmt.Sprintf("  ... e mais %d\n", remaining))
				break
			}
			b.WriteString(fmt.Sprintf("  %s (%s/%s): %s\n", it.Key, it.Type, it.Priority, it.Summary))
		}
		b.WriteString("\n")
	}
	if len(statusList) > maxStatusToShow {
		b.WriteString(fmt.Sprintf("... e mais %d status diferentes\n", len(statusList)-maxStatusToShow))
	}
	return b.String()
}

// Helpers for JQL
func defaultJQLForIntent(intent, question string, projects []string) string {
	proj := strings.Join(projects, ", ")

	// Robustness: if there is no project, do not generate "project in ()"
	hasProj := strings.TrimSpace(proj) != ""

	switch strings.TrimSpace(intent) {
	case "listar_bugs_abertos":
		if hasProj {
			return fmt.Sprintf(`project in (%s) AND issuetype = Bug AND statusCategory != Done ORDER BY updated DESC`, proj)
		}
		return `issuetype = Bug AND statusCategory != Done ORDER BY updated DESC`

	case "busca_texto":
		q := extractJQLTextQuery(question)
		if q == "" {
			// Fallback to default listing when no meaningful term found
			if hasProj {
				return fmt.Sprintf(`project in (%s) ORDER BY updated DESC`, proj)
			}
			return "ORDER BY updated DESC"
		}
		if hasProj {
			return fmt.Sprintf(`project in (%s) AND text ~ %q ORDER BY updated DESC`, proj, q)
		}
		return fmt.Sprintf(`text ~ %q ORDER BY updated DESC`, q)

	default:
		if hasProj {
			return fmt.Sprintf(`project in (%s) ORDER BY updated DESC`, proj)
		}
		return "ORDER BY updated DESC"
	}
}

// fixJQLPrecedence fixes operator precedence when AND and OR are mixed without
// explicit parentheses. Converts:
//
//	project in (X) AND text ~ "a" OR text ~ "b" OR text ~ "c"
//
// into:
//
//	project in (X) AND (text ~ "a" OR text ~ "b" OR text ~ "c")
//
// It is a no-op when the JQL already contains proper grouping (AND (...)) or
// when there is no mixing of AND and OR.
func fixJQLPrecedence(jql string) string {
	upper := strings.ToUpper(jql)
	if !strings.Contains(upper, " AND ") || !strings.Contains(upper, " OR ") {
		return jql
	}
	// Already grouped — nothing to fix.
	if strings.Contains(upper, "AND (") || strings.Contains(upper, "AND(") {
		return jql
	}

	// Preserve ORDER BY so it stays outside the parentheses.
	orderBy := ""
	if idx := strings.Index(upper, " ORDER BY "); idx >= 0 {
		orderBy = " " + strings.TrimSpace(jql[idx:])
		jql = strings.TrimSpace(jql[:idx])
		upper = strings.ToUpper(jql)
		if !strings.Contains(upper, " AND ") || !strings.Contains(upper, " OR ") {
			return jql + orderBy
		}
	}

	orParts := reSplitOR.Split(jql, -1)
	if len(orParts) <= 1 {
		return jql + orderBy
	}

	// Only apply when the first segment has a project filter AND other conditions.
	firstUpper := strings.ToUpper(orParts[0])
	if !strings.Contains(firstUpper, "PROJECT") || !strings.Contains(firstUpper, " AND ") {
		return jql + orderBy
	}

	// Split off the last AND in the first segment to get prefix + first condition.
	m := reLastAND.FindStringSubmatch(orParts[0])
	if m == nil {
		return jql + orderBy
	}
	prefix := strings.TrimSpace(m[1])
	firstCond := strings.TrimSpace(m[2])

	allConds := make([]string, 0, len(orParts))
	allConds = append(allConds, firstCond)
	for _, p := range orParts[1:] {
		allConds = append(allConds, strings.TrimSpace(p))
	}

	return prefix + " AND (" + strings.Join(allConds, " OR ") + ")" + orderBy
}

func sanitizeJQL(jql string) string {
	j := strings.TrimSpace(jql)
	if j == "" {
		return j
	}
	j = fixJQLPrecedence(j)
	j = strings.Join(strings.Fields(j), " ")
	j = strings.ReplaceAll(j, "description ~", "text ~")
	j = strings.ReplaceAll(j, "Description ~", "text ~")
	parts := strings.Split(j, " OR ")
	seen := make(map[string]bool)
	var unique []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		norm := strings.Join(strings.Fields(part), " ")
		if !seen[norm] {
			seen[norm] = true
			unique = append(unique, part)
		}
	}
	result := strings.Join(unique, " OR ")
	return result
}

// extractJQLTextQuery extracts 1-3 meaningful keywords from a natural
// language question to use as a Jira text search term.  Common stopwords
// and intent verbs are stripped so only the topic remains.
func extractJQLTextQuery(question string) string {
	skip := map[string]bool{
		"o": true, "a": true, "os": true, "as": true, "um": true, "uma": true,
		"de": true, "do": true, "da": true, "dos": true, "das": true,
		"em": true, "no": true, "na": true, "nos": true, "nas": true,
		"para": true, "por": true, "com": true, "e": true, "é": true,
		"me": true, "que": true, "já": true, "qual": true, "quais": true,
		"quando": true, "como": true, "sobre": true, "tem": true, "foi": true,
		"está": true, "estão": true, "ser": true, "isso": true, "esse": true,
		// intent verbs
		"explica": true, "explique": true, "mostre": true, "mostra": true,
		"liste": true, "listar": true, "busca": true, "buscar": true,
		"resume": true, "resumo": true, "fala": true, "fale": true,
		"quero": true, "preciso": true, "gostaria": true,
	}
	var kept []string
	for _, w := range strings.Fields(strings.ToLower(question)) {
		w = strings.Trim(w, ".,!?;:\"'()[]{}")
		if w == "" || skip[w] {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 3 {
			break
		}
	}
	return strings.Join(kept, " ")
}

// buildInformativeFallback constructs a fallback answer when the LLM
// fails or no useful context is found.  It informs the user what
// context was attempted and suggests next steps.
func buildInformativeFallback(triedSlack bool, slackMatches int, triedJira bool, jiraIssues int, issueKey string) string {
	var parts []string
	if issueKey != "" {
		parts = append(parts, fmt.Sprintf("identifiquei a issue %s", issueKey))
	}
	if triedSlack {
		if slackMatches > 0 {
			parts = append(parts, fmt.Sprintf("encontrei %d mensagens no Slack", slackMatches))
		} else {
			parts = append(parts, "não encontrei mensagens relevantes no Slack")
		}
	}
	if triedJira {
		if jiraIssues > 0 {
			parts = append(parts, fmt.Sprintf("encontrei %d issues no Jira", jiraIssues))
		} else {
			parts = append(parts, "não encontrei issues relevantes no Jira")
		}
	}
	base := "Tentei buscar contexto"
	if len(parts) > 0 {
		base += " (" + strings.Join(parts, " e ") + ")"
	}
	base += ", mas o modelo não retornou uma resposta utilizável."
	var sug []string
	if issueKey != "" {
		sug = append(sug, "Se você colar a descrição/AC da issue aqui, eu resumo certinho.")
	}
	if triedJira && jiraIssues == 0 && issueKey == "" {
		sug = append(sug, "Tenta incluir uma issue key específica (ex: PROJ-123) ou o nome do épico.")
	}
	if triedSlack && slackMatches == 0 {
		sug = append(sug, "Tenta especificar o canal ou termos exatos (ex: 'tratamento em branco' ou '#coletas').")
	}
	if len(sug) > 0 {
		base += "\n\nSugestões:\n• " + strings.Join(sug, "\n• ")
	}
	return base
}

// Helper functions reused from other packages to avoid cross-package
// dependencies.  These are copies of clip, preview and stripHTML.

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var reHTML = regexp.MustCompile(`<[^>]+>`)
var reSpaces = regexp.MustCompile(`\s+`)
var reSplitOR = regexp.MustCompile(`(?i)\s+OR\s+`)
var reLastAND = regexp.MustCompile(`(?i)^(.*)\s+AND\s+(.+)$`)
var reChannelIDInText = regexp.MustCompile(`<#([CG][A-Z0-9]{8,})(?:\|[^>]*)?>`)
var reFromUserIDQuery = regexp.MustCompile(`\bfrom:([UW][A-Z0-9]+)\b`)

// extractFromUserIDs returns unique Slack user IDs referenced by from:USERID
// filters in the query (e.g. "from:U067UM4LRGB" → ["U067UM4LRGB"]).
func extractFromUserIDs(q string) []string {
	ms := reFromUserIDQuery.FindAllStringSubmatch(q, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		if len(m) < 2 {
			continue
		}
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// extractChannelIDsFromText returns the unique channel IDs embedded in
// Slack <#CHANID> mentions within text.
func extractChannelIDsFromText(text string) []string {
	matches := reChannelIDInText.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		id := m[1]
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = reHTML.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// handleIntroRequest fetches real Jira projects and Slack channels in parallel,
// then asks the LLM to generate a rich, contextualized self-introduction.
// Falls back to a static message if data fetching or the LLM fails.
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

	// Fetch Jira projects and Slack channels in parallel.
	var (
		jiraProjects  []jira.JiraProjectInfo
		slackChannels []slack.SlackChannelInfo
		wg            sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ps, err := s.Jira.ListProjects()
		if err != nil {
			log.Printf("[JARVIS] intro: ListProjects: %v", err)
			return
		}
		jiraProjects = ps
	}()
	go func() {
		defer wg.Done()
		chs, err := s.Slack.ListChannels()
		if err != nil {
			log.Printf("[JARVIS] intro: ListChannels: %v", err)
			return
		}
		slackChannels = chs
	}()
	wg.Wait()

	log.Printf("[JARVIS] intro: jiraProjects=%d slackChannels=%d", len(jiraProjects), len(slackChannels))

	// Build project list prioritizing configured keys, capped at 15 entries.
	// This keeps the LLM context focused and examples concrete.
	projByKey := make(map[string]string, len(jiraProjects))
	for _, p := range jiraProjects {
		projByKey[p.Key] = p.Name
	}
	seen := make(map[string]bool)
	var projList []string
	// 1) Configured keys first (always included, with names when available)
	for _, k := range s.Cfg.JiraProjectKeys {
		name := projByKey[k]
		entry := k
		if name != "" && name != k {
			entry = k + " — " + name
		}
		projList = append(projList, entry)
		seen[k] = true
	}
	// 2) Fill up to 15 with remaining projects from API
	for _, p := range jiraProjects {
		if len(projList) >= 15 {
			break
		}
		if seen[p.Key] {
			continue
		}
		entry := p.Key
		if p.Name != "" && p.Name != p.Key {
			entry = p.Key + " — " + p.Name
		}
		projList = append(projList, entry)
		seen[p.Key] = true
	}
	// Fall back to configured keys if API returned nothing
	if len(projList) == 0 {
		projList = s.Cfg.JiraProjectKeys
	}

	// Build formatted channel list: "#name", capped at 20
	chanList := make([]string, 0, len(slackChannels))
	for i, ch := range slackChannels {
		if i >= 20 {
			break
		}
		chanList = append(chanList, "#"+ch.Name)
	}

	ctx := llm.IntroContext{
		BotName:           s.Cfg.BotName,
		PrimaryModel:      s.Cfg.OpenAIModel,
		FallbackModel:     s.Cfg.OpenAIFallbackModel,
		JiraBaseURL:       s.Cfg.JiraBaseURL,
		JiraProjects:      projList,
		SlackChannels:     chanList,
		JiraCreateEnabled: s.Cfg.JiraCreateEnabled,
	}

	answer, err := s.LLM.GenerateIntroduction(ctx, s.Cfg.OpenAIModel, s.Cfg.OpenAIFallbackModel)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[JARVIS] intro: LLM failed (%v), using static fallback", err)
		answer = buildIntroMessage(s.Cfg.BotName, s.Cfg.JiraCreateEnabled, projList)
	}

	if err := replyFn(answer); err != nil {
		return err
	}
	// Track so the reply is deleted if the user deletes their message.
	if busyTs != "" {
		s.Tracker.Track(channel, originTs, busyTs)
	}
	return nil
}

// isIntroRequest returns true when the question looks like the user
// is asking the bot to introduce itself or describe its capabilities.
func isIntroRequest(q string) bool {
	low := strings.ToLower(strings.TrimSpace(q))
	if low == "" {
		return false
	}
	triggers := []string{
		"o que você faz", "o que voce faz",
		"se apresente", "se apresenta",
		"quais suas funcionalidades", "quais são suas funcionalidades", "quais sao suas funcionalidades",
		"o que é o jarvis", "o que e o jarvis",
		"me fala sobre você", "me conta sobre você", "me fala sobre voce", "me conta sobre voce",
		"o que sabe fazer", "o que você sabe fazer", "o que voce sabe fazer",
		"como você pode me ajudar", "como voce pode me ajudar",
		"como posso usar você", "como posso usar voce",
		"suas capacidades", "suas funções", "suas funcoes",
		"me apresente", "me apresenta",
		"quem é você", "quem e voce", "quem é vc", "quem e vc",
		"o que pode fazer", "o que você pode fazer", "o que voce pode fazer",
		"quais são seus recursos", "quais sao seus recursos",
		"como funciona",
	}
	for _, t := range triggers {
		if strings.Contains(low, t) {
			return true
		}
	}
	return false
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
