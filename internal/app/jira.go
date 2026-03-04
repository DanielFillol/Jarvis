package app

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

// maybeHandleJiraEditFlows handles requests to edit existing Jira issues:
// transition status, assign, update fields, and set parent.
func (s *Service) maybeHandleJiraEditFlows(channel, threadTs, senderUserID, question, threadHist string) (bool, error) {
	// Strip Slack markup (permalinks, mentions) so the LLM sees clean text.
	cleanQ := s.Slack.ResolveUserMentions(parse.StripSlackPermalinks(question))

	if !s.Cfg.JiraCreateEnabled {
		if s.LLM.ConfirmJiraEditIntent(cleanQ, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
			_ = s.Slack.PostMessage(channel, threadTs, "Edição de issues no Jira está desabilitada.")
			return true, nil
		}
		return false, nil
	}

	if !s.LLM.ConfirmJiraEditIntent(cleanQ, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
		return false, nil
	}

	senderName, _ := s.Slack.GetUsernameByID(senderUserID)

	req, err := s.LLM.ExtractJiraEditRequest(cleanQ, threadHist, senderName, s.Cfg.OpenAIModel)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui interpretar o pedido de edição: %v", err))
		return true, nil
	}

	if req.IssueKey == "" {
		_ = s.Slack.PostMessage(channel, threadTs, "Não consegui identificar o número do card. Informe a chave (ex: TPTDR-522).")
		return true, nil
	}

	log.Printf("[JARVIS] jiraEdit key=%s targetStatus=%q assignee=%q parent=%q priority=%q summary=%q labels=%v",
		req.IssueKey, req.TargetStatus, req.AssigneeName, req.ParentKey, req.Priority, req.Summary, req.Labels)

	base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")
	link := base + "/browse/" + req.IssueKey
	var results []string

	// Transition (multi-step: chains through intermediates if needed)
	if req.TargetStatus != "" {
		finalStatus, steps, err := s.transitionToStatus(req.IssueKey, req.TargetStatus)
		if err != nil {
			log.Printf("[JARVIS] transitionToStatus %s → %q: %v", req.IssueKey, req.TargetStatus, err)
			_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui transicionar o card %s: %v", req.IssueKey, err))
			return true, nil
		}
		if len(steps) > 1 {
			results = append(results, fmt.Sprintf("✅ Status alterado para *%s* (via: %s)", finalStatus, strings.Join(steps, " → ")))
		} else if len(steps) == 1 {
			results = append(results, fmt.Sprintf("✅ Status alterado para *%s*", finalStatus))
		} else {
			results = append(results, fmt.Sprintf("ℹ️ Card já estava com status *%s*", finalStatus))
		}
	}

	// Assign
	if req.AssigneeName != "" {
		searchName := req.AssigneeName
		if req.AssigneeName == "@me" {
			searchName = senderName
		}
		users, err := s.Jira.SearchAssignableUsers(req.IssueKey, searchName, 5)
		if err != nil {
			log.Printf("[JARVIS] SearchAssignableUsers %s query=%q: %v", req.IssueKey, searchName, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui buscar usuários para *%s*: %v", searchName, err))
		} else {
			user := pickBestUser(users, searchName)
			if user == nil {
				results = append(results, fmt.Sprintf("⚠️ Usuário *%s* não encontrado como assignável no card %s", searchName, req.IssueKey))
			} else if err := s.Jira.AssignIssue(req.IssueKey, user.AccountID); err != nil {
				log.Printf("[JARVIS] AssignIssue %s → %s: %v", req.IssueKey, user.AccountID, err)
				results = append(results, fmt.Sprintf("⚠️ Não consegui atribuir o card a *%s*: %v", user.DisplayName, err))
			} else {
				results = append(results, fmt.Sprintf("✅ Atribuído a *%s*", user.DisplayName))
			}
		}
	}

	// Set parent
	if req.ParentKey != "" {
		fields := map[string]any{"parent": map[string]any{"key": req.ParentKey}}
		if err := s.Jira.UpdateIssue(req.IssueKey, fields); err != nil {
			log.Printf("[JARVIS] SetParent %s → %s: %v", req.IssueKey, req.ParentKey, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui definir o pai como *%s*: %v", req.ParentKey, err))
		} else {
			results = append(results, fmt.Sprintf("✅ Pai definido como *%s*", req.ParentKey))
		}
	}

	// Update fields
	updateFields := map[string]any{}
	if req.Summary != "" {
		updateFields["summary"] = req.Summary
	}
	if req.Description != "" {
		updateFields["description"] = jira.TextToADF(req.Description)
	}
	if req.Priority != "" {
		updateFields["priority"] = map[string]any{"name": normalizePriority(req.Priority)}
	}
	if len(req.Labels) > 0 {
		updateFields["labels"] = req.Labels
	}
	if len(updateFields) > 0 {
		if err := s.Jira.UpdateIssue(req.IssueKey, updateFields); err != nil {
			log.Printf("[JARVIS] UpdateIssue %s fields: %v", req.IssueKey, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui atualizar campos: %v", err))
		} else {
			var changed []string
			if req.Summary != "" {
				changed = append(changed, "summary")
			}
			if req.Description != "" {
				changed = append(changed, "description")
			}
			if req.Priority != "" {
				changed = append(changed, "priority")
			}
			if len(req.Labels) > 0 {
				changed = append(changed, "labels")
			}
			results = append(results, fmt.Sprintf("✅ Campos atualizados: %s", strings.Join(changed, ", ")))
		}
	}

	if len(results) == 0 {
		_ = s.Slack.PostMessage(channel, threadTs, "Nenhuma alteração foi aplicada.")
		return true, nil
	}
	msg := fmt.Sprintf("*%s* atualizado:\n%s\n%s", req.IssueKey, strings.Join(results, "\n"), link)
	_ = s.Slack.PostMessage(channel, threadTs, msg)
	return true, nil
}

// transitionToStatus moves issueKey toward desiredStatus, chaining through
// intermediate transitions if the workflow doesn't allow direct jumps.
// The maximum number of steps is derived from the project's workflow size
// (number of statuses in jira_projects.md catalog), falling back to 10.
// Returns the final status name and the names of all transitions executed.
func (s *Service) transitionToStatus(issueKey, desiredStatus string) (finalStatus string, steps []string, err error) {
	// Extract project key from issue key (e.g. "TPTDR-522" → "TPTDR").
	maxSteps := 10
	if idx := strings.Index(issueKey, "-"); idx > 0 {
		projectKey := issueKey[:idx]
		if statuses, ok := s.Jira.WorkflowStatuses[projectKey]; ok && len(statuses) > 1 {
			maxSteps = len(statuses)
			// Normalize the desired status to the project's actual status name
			// so the equality check works regardless of language/casing.
			desiredStatus = s.LLM.MapStatusName(statuses, desiredStatus, s.Cfg.OpenAILesserModel)
		}
	}
	log.Printf("[JARVIS] transitionToStatus %s → %q maxSteps=%d", issueKey, desiredStatus, maxSteps)
	for i := 0; i < maxSteps; i++ {
		issue, err := s.Jira.GetIssue(issueKey)
		if err != nil {
			return "", steps, fmt.Errorf("GetIssue: %w", err)
		}
		currentStatus := issue.Fields.Status.Name
		if strings.EqualFold(currentStatus, desiredStatus) {
			return currentStatus, steps, nil
		}
		transitions, err := s.Jira.GetTransitions(issueKey)
		if err != nil {
			return "", steps, fmt.Errorf("GetTransitions: %w", err)
		}
		transID := s.LLM.PickBestTransition(transitions, desiredStatus, s.Cfg.OpenAIModel)
		if transID == "" {
			var tNames []string
			for _, t := range transitions {
				tNames = append(tNames, t.Name)
			}
			return currentStatus, steps, fmt.Errorf("sem caminho de *%s* até *%s*; disponíveis: %s",
				currentStatus, desiredStatus, strings.Join(tNames, ", "))
		}
		transName := transID
		for _, t := range transitions {
			if t.ID == transID {
				transName = t.Name
				break
			}
		}
		if err := s.Jira.TransitionIssue(issueKey, transID); err != nil {
			return "", steps, fmt.Errorf("TransitionIssue(%s): %w", transName, err)
		}
		steps = append(steps, transName)
		log.Printf("[JARVIS] transitionToStatus %s step=%d via=%q toward=%q", issueKey, i+1, transName, desiredStatus)
	}
	return "", steps, fmt.Errorf("não atingiu %q em %d passos", desiredStatus, maxSteps)
}

// pickBestUser selects the most relevant user from assignable search results.
// Prefers active users whose display name contains the query, then any active user.
func pickBestUser(users []jira.JiraUser, query string) *jira.JiraUser {
	q := strings.ToLower(query)
	for i, u := range users {
		if u.Active && strings.Contains(strings.ToLower(u.DisplayName), q) {
			return &users[i]
		}
	}
	for i, u := range users {
		if u.Active {
			return &users[i]
		}
	}
	if len(users) > 0 {
		return &users[0]
	}
	return nil
}

// normalizePriority maps Portuguese priority names to Jira English names.
func normalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "crítica", "critica", "crítico", "critico":
		return "Critical"
	case "alta", "alto":
		return "High"
	case "média", "media", "médio", "medio":
		return "Medium"
	case "baixa", "baixo":
		return "Low"
	case "urgente":
		return "Highest"
	default:
		return p
	}
}

// maybeHandleJiraCreateFlows orchestrates the state machine for Jira creation commands.
func (s *Service) maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist string) (bool, error) {
	if !s.Cfg.JiraCreateEnabled {
		if s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
			_ = s.Slack.PostMessage(channel, threadTs, "Criação de issues no Jira está desabilitada.")
			return true, nil
		}
		return false, nil
	}

	// 1. Check for a pending draft first — a previous turn asked for missing fields.
	//    Re-run extraction on the now-complete thread (which includes the user's reply)
	//    and try to fill in what was missing.
	if pending := s.Store.Load(channel, threadTs); pending != nil {
		log.Printf("[JARVIS] pending Jira draft found for thread=%s, re-extracting", threadTs)
		draft, err := s.LLM.ExtractIssueFromThread(threadHist, pending.OriginalText, s.Cfg.OpenAIModel, nil, s.Cfg.JiraProjectNameMap)
		if err != nil {
			_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui interpretar o card: %v", err))
			s.Store.Delete(channel, threadTs)
			return true, nil
		}
		if missing := missingFields(draft); len(missing) > 0 {
			// Still missing — update store and ask again.
			s.Store.Save(&state.PendingIssue{
				CreatedAt: time.Now(), Channel: channel, ThreadTs: threadTs,
				OriginTs: pending.OriginTs, OriginalText: pending.OriginalText, Draft: draft,
			})
			_ = s.Slack.PostMessage(channel, threadTs, askForMissingFields(missing))
			return true, nil
		}
		s.Store.Delete(channel, threadTs)
		s.appendSlackOrigin(&draft, channel, threadTs, pending.OriginTs, pending.OriginalText)
		return true, s.createIssueAndReply(channel, threadTs, draft)
	}

	// 2. Detect new Jira create intent.
	if !s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
		return false, nil
	}

	// 3. Extract draft using the primary model for better accuracy.
	draft, err := s.LLM.ExtractIssueFromThread(threadHist, question, s.Cfg.OpenAIModel, nil, s.Cfg.JiraProjectNameMap)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui entender o card a partir da thread: %v", err))
		return true, nil
	}

	// 4. If essential fields are missing, ask the user and save state.
	if missing := missingFields(draft); len(missing) > 0 {
		s.Store.Save(&state.PendingIssue{
			CreatedAt: time.Now(),
			Channel:   channel, ThreadTs: threadTs,
			OriginTs: originTs, OriginalText: originalText,
			Draft: draft,
		})
		_ = s.Slack.PostMessage(channel, threadTs, askForMissingFields(missing))
		return true, nil
	}

	// 5. All fields present — create the card.
	s.appendSlackOrigin(&draft, channel, threadTs, originTs, originalText)
	return true, s.createIssueAndReply(channel, threadTs, draft)
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

// createIssueAndReply creates a Jira issue via the Jira client, posts a
// confirmation message to Slack, and attaches any images or videos found in
// the thread to the newly created issue.
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
	s.attachThreadMediaToIssue(created.Key, channel, threadTs)
	return nil
}

// attachThreadMediaToIssue fetches all files from the Slack thread and uploads
// them as attachments to the given Jira issue.  Errors are logged but not
// propagated — attachment failures do not affect the card creation reply.
func (s *Service) attachThreadMediaToIssue(issueKey, channel, threadTs string) {
	files, err := s.Slack.GetThreadFiles(channel, threadTs)
	if err != nil {
		log.Printf("[JARVIS] GetThreadFiles for %s: %v", issueKey, err)
		return
	}
	const maxFileBytes = 10 * 1024 * 1024 // 10 MB per file
	attached := 0
	for _, f := range files {
		if f.Size > maxFileBytes {
			log.Printf("[JARVIS] skipping %s (size %d > 10MB) for jira attach", f.Name, f.Size)
			continue
		}
		data, dlErr := s.Slack.DownloadFile(f.URLPrivateDownload)
		if dlErr != nil {
			log.Printf("[JARVIS] download %s for jira attach: %v", f.Name, dlErr)
			continue
		}
		if attErr := s.Jira.AttachFileToIssue(issueKey, f.Name, data); attErr != nil {
			log.Printf("[JARVIS] attach %s to %s: %v", f.Name, issueKey, attErr)
			continue
		}
		log.Printf("[JARVIS] attached %s to %s", f.Name, issueKey)
		attached++
	}
	if attached > 0 {
		log.Printf("[JARVIS] %d file(s) attached to %s", attached, issueKey)
	}
}

// buildFileContext downloads files attached to the message and formats their
// contents for inclusion in the LLM prompt.
// Supported: text/*, JSON, YAML, XML, JS, TS (raw bytes) and XLSX (parsed as table).
// Files larger than 20 MB are skipped. Total output is capped at 8 M chars to
// stay safely under the OpenAI API limit of ~10 M chars per message.
func (s *Service) buildFileContext(files []slack.File) string {
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

		// Enforce total character cap — truncate the current file if needed.
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

// buildImageAttachments downloads image files and returns them as vision
// attachments for the LLM. Images larger than 5 MB are skipped (OpenAI
// base64 limit).
func (s *Service) buildImageAttachments(files []slack.File) []llm.ImageAttachment {
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
