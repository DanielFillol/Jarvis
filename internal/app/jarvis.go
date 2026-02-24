// internal/app/jarvis.go
package app

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

// Service encapsulates the core orchestration logic of Jarvis.  It
// coordinates between Slack, Jira and the language model to answer
// questions and handle issue creation flows.  The Service does not
// depend on net/http and is therefore easily testable.
type Service struct {
	Slack *slack.Client
	Jira  *jira.Client
	LLM   *llm.Client
	Store *state.Store
	Cfg   config.Config
}

// NewService constructs a new Jarvis service from its dependencies.
func NewService(cfg config.Config, slackClient *slack.Client, jiraClient *jira.Client, llmClient *llm.Client, store *state.Store) *Service {
	return &Service{
		Slack: slackClient,
		Jira:  jiraClient,
		LLM:   llmClient,
		Store: store,
		Cfg:   cfg,
	}
}

// HandleMessage processes an incoming message directed at the bot.  It
// delegates to the appropriate flows: Jira creation, issue lookup,
// context retrieval and answer generation.  On error, a fallback
// answer is posted to Slack to provide user feedback.
func (s *Service) HandleMessage(channel, threadTs, originTs, originalText, question, senderUserID string) error {
	start := time.Now()
	log.Printf("[JARVIS] start question=%q originTs=%q senderUserID=%q", preview(question, 180), originTs, senderUserID)
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
	// 4) Deterministic: issue key in text
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
	// 5) Decide Slack/Jira search (LLM)
	questionForLLM := parse.StripSlackPermalinks(question)
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
	// 5) Slack context
	var slackCtx string
	var slackMatches int
	if decision.NeedSlack && strings.TrimSpace(decision.SlackQuery) != "" && s.Cfg.SlackUserToken != "" {
		log.Printf("[JARVIS] slackSearch query=%q", decision.SlackQuery)
		matches, err := s.Slack.SearchMessagesAll(decision.SlackQuery)
		if err != nil {
			log.Printf("[WARN] slack search failed: %v", err)
		} else {
			slackMatches = len(matches)
			slackCtx = buildSlackContext(matches, 12)
			log.Printf("[JARVIS] slackContext matches=%d chars=%d", slackMatches, len(slackCtx))
		}
	}
	// 6) Jira context via JQL
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
	// 7) Answer with LLM (with retry for transient errors)
	answer, err := s.LLM.AnswerWithRetry(questionForLLM, threadHist, slackCtx, finalJiraCtx, s.Cfg.OpenAIModel, s.Cfg.OpenAIFallbackModel, 2, 0)
	if err != nil || strings.TrimSpace(answer) == "" {
		log.Printf("[ERR] llmAnswer failed: %v", err)
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, (jiraIssueCtx != "" || decision.NeedJira), jiraIssuesFound, issueKey)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = buildInformativeFallback(decision.NeedSlack, slackMatches, (jiraIssueCtx != "" || decision.NeedJira), jiraIssuesFound, issueKey)
	}
	if err := s.Slack.PostMessage(channel, threadTs, answer); err != nil {
		log.Printf("[ERR] postSlackMessage failed: %v", err)
		return err
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
	if strings.Contains(low, "crie um card") || strings.Contains(low, "criar um card") || parse.LooksLikeJiraCreateIntent(low) {
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
	if parse.IsThreadBasedCreate(q) {
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
		b.WriteString(fmt.Sprintf("%s [%s] (%s) %s — %s | assignee=%s | updated=%s\n", it.Key, it.Status, it.Type, it.Priority, it.Summary, it.Assignee, it.Updated))
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

func sanitizeJQL(jql string) string {
	j := strings.TrimSpace(jql)
	if j == "" {
		return j
	}
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
