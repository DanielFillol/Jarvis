package app

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DanielFillol/Jarvis/internal/googledrive"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

var reJiraKey = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)

// jiraEditResult holds the outcome of maybeHandleJiraEditFlows.
type jiraEditResult struct {
	// Handled is true when the edit flow consumed the message.
	Handled bool
	// Reply is the success message text when quiet=true; callers are responsible
	// for posting it (typically prepended to the combined final answer).
	Reply string
}

// maybeHandleJiraEditFlows handles requests to edit existing Jira issues:
// transition status, assign, update fields, and set parent.
// overrideIssueKey, when non-empty, is used as the target issue key instead of
// extracting it from the message (used when a card was just created in the same turn).
// intentConfirmed skips the ConfirmJiraEditIntent LLM call when the caller has
// already verified the intent via DecideActions.
// quiet suppresses the direct Slack success post; the confirmation text is instead
// returned in jiraEditResult.Reply so callers can prepend it to a combined answer.
func (s *Service) maybeHandleJiraEditFlows(channel, threadTs, senderUserID, question, threadHist, overrideIssueKey string, intentConfirmed, quiet bool) (jiraEditResult, error) {
	// Strip Slack markup (permalinks, mentions) so the LLM sees clean text.
	cleanQ := s.Slack.ResolveUserMentions(parse.StripSlackPermalinks(question))

	if !s.Cfg.JiraCreateEnabled {
		if intentConfirmed || s.LLM.ConfirmJiraEditIntent(cleanQ, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
			_ = s.Slack.PostMessage(channel, threadTs, "Edição de issues no Jira está desabilitada.")
			return jiraEditResult{Handled: true}, nil
		}
		return jiraEditResult{}, nil
	}

	if !intentConfirmed && !s.LLM.ConfirmJiraEditIntent(cleanQ, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
		return jiraEditResult{}, nil
	}

	senderName, _ := s.Slack.GetUsernameByID(senderUserID)

	req, err := s.LLM.ExtractJiraEditRequest(cleanQ, threadHist, senderName, s.Cfg.OpenAIModel)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui interpretar o pedido de edição: %v", err))
		return jiraEditResult{Handled: true}, nil
	}

	// When a card was just created in the same turn, use that key directly.
	if overrideIssueKey != "" {
		req.IssueKey = overrideIssueKey
		req.AdditionalIssueKeys = nil // override applies to the newly created card only
	}

	if req.IssueKey == "" {
		_ = s.Slack.PostMessage(channel, threadTs, "Não consegui identificar o número do card. Informe a chave (ex: PROJ-123).")
		return jiraEditResult{Handled: true}, nil
	}

	allKeys := append([]string{req.IssueKey}, req.AdditionalIssueKeys...)
	log.Printf("[JARVIS] jiraEdit keys=%v targetStatus=%q assignee=%q parent=%q priority=%q summary=%q labels=%v generateDesc=%v",
		allKeys, req.TargetStatus, req.AssigneeName, req.ParentKey, req.Priority, req.Summary, req.Labels, req.GenerateDescription)

	base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")

	// Process all cards in parallel — each may require LLM generation (~10s).
	type result struct {
		idx   int
		key   string
		lines []string
	}
	results := make([]result, len(allKeys))
	var wg sync.WaitGroup
	for i, key := range allKeys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			lines := s.applyJiraEditToIssue(key, req, senderName, cleanQ, threadHist)
			results[i] = result{idx: i, key: key, lines: lines}
		}(i, key)
	}
	wg.Wait()

	var parts []string
	for _, r := range results {
		if len(r.lines) == 0 {
			parts = append(parts, fmt.Sprintf("*%s*: nenhuma alteração aplicada.", r.key))
		} else {
			link := base + "/browse/" + r.key
			parts = append(parts, fmt.Sprintf("*%s* atualizado:\n%s\n%s", r.key, strings.Join(r.lines, "\n"), link))
		}
	}
	replyText := strings.Join(parts, "\n\n")
	if !quiet {
		_ = s.Slack.PostMessage(channel, threadTs, replyText)
		return jiraEditResult{Handled: true}, nil
	}
	return jiraEditResult{Handled: true, Reply: replyText}, nil
}

// applyJiraEditToIssue applies all edits from req to a single issueKey and
// returns human-readable result lines.  When req.GenerateDescription is true
// and req.Description is empty, the description is generated via LLM using
// the individual card context.
func (s *Service) applyJiraEditToIssue(issueKey string, req jira.EditRequest, senderName, cleanQ, threadHist string) []string {
	var results []string

	// Resolve generated description per card (each card gets its own content).
	description := req.Description
	if req.GenerateDescription && description == "" {
		issue, err := s.Jira.GetIssue(issueKey)
		if err != nil {
			log.Printf("[JARVIS] GenerateDescription GetIssue %s: %v", issueKey, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui buscar o card para gerar descrição: %v", err))
			return results
		}
		generated, err := s.LLM.GenerateIssueDescription(
			issueKey,
			issue.Fields.IssueType.Name,
			issue.Fields.Summary,
			issue.RenderedFields.Description,
			cleanQ,
			threadHist,
			s.Cfg.OpenAIModel,
		)
		if err != nil {
			log.Printf("[JARVIS] GenerateIssueDescription %s: %v", issueKey, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui gerar a descrição: %v", err))
			return results
		}
		description = generated
		log.Printf("[JARVIS] GenerateIssueDescription %s generated len=%d", issueKey, len(generated))
	}

	// Transition (multi-step: chains through intermediates if needed)
	if req.TargetStatus != "" {
		tr, err := s.transitionToStatus(issueKey, req.TargetStatus)
		if err != nil {
			log.Printf("[JARVIS] transitionToStatus %s → %q: %v", issueKey, req.TargetStatus, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui transicionar: %v", err))
		} else if len(tr.Steps) > 1 {
			results = append(results, fmt.Sprintf("✅ Status alterado para *%s* (via: %s)", tr.FinalStatus, strings.Join(tr.Steps, " → ")))
		} else if len(tr.Steps) == 1 {
			results = append(results, fmt.Sprintf("✅ Status alterado para *%s*", tr.FinalStatus))
		} else {
			results = append(results, fmt.Sprintf("ℹ️ Card já estava com status *%s*", tr.FinalStatus))
		}
	}

	// Assign
	if req.AssigneeName != "" {
		searchName := req.AssigneeName
		if req.AssigneeName == "@me" {
			searchName = senderName
		}
		users, err := s.Jira.SearchAssignableUsers(issueKey, searchName, 5)
		if err != nil {
			log.Printf("[JARVIS] SearchAssignableUsers %s query=%q: %v", issueKey, searchName, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui buscar usuários para *%s*: %v", searchName, err))
		} else {
			user := pickBestUser(users, searchName)
			if user == nil {
				results = append(results, fmt.Sprintf("⚠️ Usuário *%s* não encontrado como assignável", searchName))
			} else if err := s.Jira.AssignIssue(issueKey, user.AccountID); err != nil {
				log.Printf("[JARVIS] AssignIssue %s → %s: %v", issueKey, user.AccountID, err)
				results = append(results, fmt.Sprintf("⚠️ Não consegui atribuir a *%s*: %v", user.DisplayName, err))
			} else {
				results = append(results, fmt.Sprintf("✅ Atribuído a *%s*", user.DisplayName))
			}
		}
	}

	// Set parent — resolve name/text to a valid key first when necessary.
	if req.ParentKey != "" {
		parentKey, resolveErr := s.resolveParentKey(issueKey, req.ParentKey)
		if resolveErr != nil {
			log.Printf("[JARVIS] resolveParentKey %s ref=%q: %v", issueKey, req.ParentKey, resolveErr)
			results = append(results, fmt.Sprintf("⚠️ Não encontrei o card pai %q: %v", req.ParentKey, resolveErr))
		} else {
			fields := map[string]any{"parent": map[string]any{"key": parentKey}}
			if err := s.Jira.UpdateIssue(issueKey, fields); err != nil {
				log.Printf("[JARVIS] SetParent %s → %s: %v", issueKey, parentKey, err)
				results = append(results, fmt.Sprintf("⚠️ Não consegui definir o pai como *%s*: %v", parentKey, err))
			} else {
				results = append(results, fmt.Sprintf("✅ Pai definido como *%s*", parentKey))
			}
		}
	}

	// Update fields
	updateFields := map[string]any{}
	if req.Summary != "" {
		updateFields["summary"] = req.Summary
	}
	if description != "" {
		updateFields["description"] = jira.TextToADF(description)
	}
	if req.Priority != "" {
		updateFields["priority"] = map[string]any{"name": normalizePriority(req.Priority)}
	}
	if len(req.Labels) > 0 {
		updateFields["labels"] = req.Labels
	}
	if len(updateFields) > 0 {
		log.Printf("[JARVIS] UpdateIssue %s fields=%v", issueKey, fieldKeys(updateFields))
		if err := s.Jira.UpdateIssue(issueKey, updateFields); err != nil {
			log.Printf("[JARVIS] UpdateIssue %s FAILED: %v", issueKey, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui atualizar campos: %v", err))
		} else {
			var changed []string
			if req.Summary != "" {
				changed = append(changed, "summary")
			}
			if description != "" {
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

	// Move to sprint
	if req.TargetSprint != "" {
		sprint, err := s.resolveTargetSprint(issueKey, req.TargetSprint)
		if err != nil {
			log.Printf("[JARVIS] resolveTargetSprint %s target=%q: %v", issueKey, req.TargetSprint, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui resolver a sprint: %v", err))
		} else if err := s.Jira.MoveIssueToSprint(sprint.ID, issueKey); err != nil {
			log.Printf("[JARVIS] MoveIssueToSprint %s → sprint %d: %v", issueKey, sprint.ID, err)
			results = append(results, fmt.Sprintf("⚠️ Não consegui mover para a sprint *%s*: %v", sprint.Name, err))
		} else {
			results = append(results, fmt.Sprintf("✅ Movido para a sprint *%s*", sprint.Name))
		}
	}

	return results
}

// transitionResult holds the outcome of transitionToStatus.
type transitionResult struct {
	FinalStatus string
	Steps       []string
}

// transitionToStatus moves issueKey toward desiredStatus, chaining through
// intermediate transitions if the workflow doesn't allow direct jumps.
// The maximum number of steps is derived from the project's workflow size
// (number of statuses in jira_projects.md catalog), falling back to 10.
// Returns the final status name and the names of all transitions executed.
func (s *Service) transitionToStatus(issueKey, desiredStatus string) (transitionResult, error) {
	// Extract project key from issue key (e.g. "PROJ-522" → "PROJ").
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
	var steps []string
	for i := 0; i < maxSteps; i++ {
		issue, err := s.Jira.GetIssue(issueKey)
		if err != nil {
			return transitionResult{Steps: steps}, fmt.Errorf("GetIssue: %w", err)
		}
		currentStatus := issue.Fields.Status.Name
		if strings.EqualFold(currentStatus, desiredStatus) {
			return transitionResult{FinalStatus: currentStatus, Steps: steps}, nil
		}
		transitions, err := s.Jira.GetTransitions(issueKey)
		if err != nil {
			return transitionResult{Steps: steps}, fmt.Errorf("GetTransitions: %w", err)
		}
		transID := s.LLM.PickBestTransition(transitions, desiredStatus, s.Cfg.OpenAIModel)
		if transID == "" {
			var tNames []string
			for _, t := range transitions {
				tNames = append(tNames, t.Name)
			}
			return transitionResult{FinalStatus: currentStatus, Steps: steps}, fmt.Errorf("sem caminho de *%s* até *%s*; disponíveis: %s",
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
			return transitionResult{Steps: steps}, fmt.Errorf("TransitionIssue(%s): %w", transName, err)
		}
		steps = append(steps, transName)
		log.Printf("[JARVIS] transitionToStatus %s step=%d via=%q toward=%q", issueKey, i+1, transName, desiredStatus)
	}
	return transitionResult{Steps: steps}, fmt.Errorf("não atingiu %q em %d passos", desiredStatus, maxSteps)
}

// resolveTargetSprint finds the right sprint for the issue's project board
// based on the LLM's extracted target ("current", "next", or a sprint name/number).
func (s *Service) resolveTargetSprint(issueKey, target string) (*jira.Sprint, error) {
	// Derive project key from issue key (e.g. "PROJ-522" → "PROJ").
	idx := strings.Index(issueKey, "-")
	if idx <= 0 {
		return nil, fmt.Errorf("invalid issue key: %q", issueKey)
	}
	projectKey := issueKey[:idx]

	boards, err := s.Jira.GetBoards(projectKey)
	if err != nil {
		return nil, fmt.Errorf("GetBoards(%s): %w", projectKey, err)
	}
	if len(boards) == 0 {
		return nil, fmt.Errorf("nenhum board encontrado para o projeto %s", projectKey)
	}
	boardID := boards[0].ID // use the first board for the project

	switch strings.ToLower(strings.TrimSpace(target)) {
	case "current", "atual", "corrente", "ativa":
		sprints, err := s.Jira.GetSprints(boardID, "active")
		if err != nil {
			return nil, fmt.Errorf("GetSprints(active): %w", err)
		}
		if len(sprints) == 0 {
			return nil, fmt.Errorf("nenhuma sprint ativa encontrada no board do projeto %s", projectKey)
		}
		return &sprints[0], nil

	case "next", "next sprint", "próxima", "proxima", "seguinte":
		sprints, err := s.Jira.GetSprints(boardID, "future")
		if err != nil {
			return nil, fmt.Errorf("GetSprints(future): %w", err)
		}
		if len(sprints) == 0 {
			return nil, fmt.Errorf("nenhuma sprint futura encontrada no board do projeto %s", projectKey)
		}
		return &sprints[0], nil

	default:
		// Search by name/number across active + future sprints.
		var candidates []jira.Sprint
		for _, st := range []string{"active", "future"} {
			ss, err := s.Jira.GetSprints(boardID, st)
			if err == nil {
				candidates = append(candidates, ss...)
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("nenhuma sprint ativa ou futura encontrada para o projeto %s", projectKey)
		}
		// Use LLM to pick the best match among candidate names.
		sprintID := s.LLM.PickBestSprintByName(candidates, target, s.Cfg.OpenAILesserModel)
		if sprintID == 0 {
			var names []string
			for _, sp := range candidates {
				names = append(names, sp.Name)
			}
			return nil, fmt.Errorf("sprint %q não encontrada. Disponíveis: %s", target, strings.Join(names, ", "))
		}
		for i := range candidates {
			if candidates[i].ID == sprintID {
				return &candidates[i], nil
			}
		}
		return nil, fmt.Errorf("sprint ID %d não encontrado", sprintID)
	}
}

// resolveParentKey resolves a parent issue reference to a valid Jira issue key.
// If parentRef already looks like a Jira key (e.g. "PROJ-164"), it is returned as-is.
// Otherwise it searches Jira by text within the child issue's project and returns
// the key of the first match.
func (s *Service) resolveParentKey(issueKey, parentRef string) (string, error) {
	parentRef = strings.TrimSpace(parentRef)
	if reJiraKey.MatchString(parentRef) {
		return parentRef, nil
	}
	// Derive project key from the child issue (e.g. "PROJ-531" → "PROJ").
	projKey := ""
	if idx := strings.Index(issueKey, "-"); idx > 0 {
		projKey = issueKey[:idx]
	}
	jql := fmt.Sprintf(`text ~ %q ORDER BY updated DESC`, parentRef)
	if projKey != "" {
		jql = fmt.Sprintf(`project = %s AND text ~ %q ORDER BY updated DESC`, projKey, parentRef)
	}
	log.Printf("[JARVIS] resolveParentKey %s ref=%q jql=%q", issueKey, parentRef, jql)
	issues, err := s.Jira.FetchAll(jql, 5)
	if err != nil {
		return "", fmt.Errorf("busca por %q falhou: %w", parentRef, err)
	}
	if len(issues) == 0 {
		return "", fmt.Errorf("nenhum card encontrado para %q", parentRef)
	}
	log.Printf("[JARVIS] resolveParentKey %s ref=%q → %s (%s)", issueKey, parentRef, issues[0].Key, issues[0].Summary)
	return issues[0].Key, nil
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

// fieldKeys returns the map keys for logging purposes.
func fieldKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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

// jiraCreateResult holds the outcome of maybeHandleJiraCreateFlows.
type jiraCreateResult struct {
	// Handled is true when the create flow consumed the message (even if creation
	// was skipped due to missing fields or disabled config).
	Handled bool
	// CreatedKey is the Jira issue key of the newly created card (e.g. "PROJ-526").
	// Empty when the card was not created in this turn (pending state, disabled, etc.).
	CreatedKey string
	// Reply is the success message text when quiet=true; callers are responsible
	// for posting it (typically prepended to the combined final answer).
	Reply string
}

// maybeHandleJiraCreateFlows orchestrates the state machine for Jira creation commands.
// intentConfirmed skips the ConfirmJiraCreateIntent LLM call when the caller has
// already verified the intent via DecideActions.
// quiet suppresses the direct Slack success post; the confirmation text is instead
// returned in jiraCreateResult.Reply so callers can prepend it to a combined answer.
func (s *Service) maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist string, intentConfirmed, quiet bool) (jiraCreateResult, error) {
	if !s.Cfg.JiraCreateEnabled {
		if intentConfirmed || s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
			_ = s.Slack.PostMessage(channel, threadTs, "Criação de issues no Jira está desabilitada.")
			return jiraCreateResult{Handled: true}, nil
		}
		return jiraCreateResult{}, nil
	}

	// 1. Check for a pending draft first — a previous turn asked for missing fields.
	//    Re-run extraction on the now-complete thread (which includes the user's reply)
	//    and try to fill in what was missing.
	if pending := s.Store.Load(channel, threadTs); pending != nil {
		log.Printf("[JARVIS] pending Jira draft found for thread=%s, re-extracting", threadTs)
		draft, extractErr := s.LLM.ExtractIssueFromThread(threadHist, pending.OriginalText, s.Cfg.OpenAIModel, nil, s.Cfg.JiraProjectNameMap)
		if extractErr != nil {
			_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui interpretar o card: %v", extractErr))
			s.Store.Delete(channel, threadTs)
			return jiraCreateResult{Handled: true}, nil
		}
		if missing := missingFields(draft); len(missing) > 0 {
			// Still missing — update store and ask again.
			s.Store.Save(&state.PendingIssue{
				CreatedAt: time.Now(), Channel: channel, ThreadTs: threadTs,
				OriginTs: pending.OriginTs, OriginalText: pending.OriginalText, Draft: draft,
			})
			_ = s.Slack.PostMessage(channel, threadTs, askForMissingFields(missing))
			return jiraCreateResult{Handled: true}, nil
		}
		s.Store.Delete(channel, threadTs)
		s.appendSlackOrigin(&draft, channel, threadTs, pending.OriginTs, pending.OriginalText)
		key, createErr := s.createIssueAndReply(channel, threadTs, draft, quiet)
		var replyText string
		if quiet && key != "" {
			base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")
			replyText = fmt.Sprintf("Card criado ✅ *%s*\n%s/browse/%s", key, base, key)
		}
		return jiraCreateResult{Handled: true, CreatedKey: key, Reply: replyText}, createErr
	}

	// 2. Detect new Jira create intent.
	if !intentConfirmed && !s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
		return jiraCreateResult{}, nil
	}

	// 3. Extract draft using the primary model for better accuracy.
	draft, extractErr := s.LLM.ExtractIssueFromThread(threadHist, question, s.Cfg.OpenAIModel, nil, s.Cfg.JiraProjectNameMap)
	if extractErr != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui entender o card a partir da thread: %v", extractErr))
		return jiraCreateResult{Handled: true}, nil
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
		return jiraCreateResult{Handled: true}, nil
	}

	// 5. All fields present — create the card.
	s.appendSlackOrigin(&draft, channel, threadTs, originTs, originalText)
	key, createErr := s.createIssueAndReply(channel, threadTs, draft, quiet)
	var replyText string
	if quiet && key != "" {
		base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")
		replyText = fmt.Sprintf("Card criado ✅ *%s*\n%s/browse/%s", key, base, key)
	}
	return jiraCreateResult{Handled: true, CreatedKey: key, Reply: replyText}, createErr
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

// createIssueAndReply creates a Jira issue via the Jira client and, unless
// quiet is true, posts a confirmation message to Slack.  It also attaches any
// images or videos found in the thread to the newly created issue.
// Returns (issueKey, error).  When quiet=true the caller is responsible for
// building and posting the confirmation text (typically prepended to a combined answer).
func (s *Service) createIssueAndReply(channel, threadTs string, d jira.IssueDraft, quiet bool) (string, error) {
	d.Project = strings.TrimSpace(d.Project)
	d.IssueType = strings.TrimSpace(d.IssueType)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Description = strings.TrimSpace(d.Description)
	if d.Project == "" || d.IssueType == "" {
		_ = s.Slack.PostMessage(channel, threadTs, missingFieldsMsg(d, d.Project == "", d.IssueType == "", s.Cfg.BotName))
		return "", nil
	}
	created, err := s.Jira.CreateIssue(d)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui criar o card no Jira: %v", err))
		return "", nil
	}
	base := strings.TrimRight(s.Cfg.JiraBaseURL, "/")
	link := base + "/browse/" + created.Key
	replyText := fmt.Sprintf("Card criado ✅ *%s*\n%s", created.Key, link)
	if !quiet {
		_ = s.Slack.PostMessage(channel, threadTs, replyText)
	}
	s.attachThreadMediaToIssue(created.Key, channel, threadTs)
	return created.Key, nil
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
		isGSheets := isGoogleSheetsMimetype(f.Mimetype)

		if isGSheets {
			if s.GoogleDrive == nil {
				log.Printf("[JARVIS] skipping Google Sheets file %q: Google Drive client not configured", f.Name)
				continue
			}
			fileIDs := googledrive.ExtractFileIDsFromText(f.ExternalURL)
			if len(fileIDs) == 0 {
				log.Printf("[JARVIS] skipping Google Sheets file %q: no Drive file ID in ExternalURL=%q", f.Name, f.ExternalURL)
				continue
			}
			log.Printf("[JARVIS] fetching Google Sheets file %q via Drive fileID=%s", f.Name, fileIDs[0])
			result, driveErr := s.GoogleDrive.FetchByFileID(fileIDs[0], "")
			if driveErr != nil {
				log.Printf("[JARVIS] failed to fetch Google Sheets %q: %v", f.Name, driveErr)
				continue
			}
			content := result.Content
			if content == "" {
				log.Printf("[JARVIS] Google Sheets file %q returned empty content", f.Name)
				continue
			}
			remaining := maxTotalChars - b.Len()
			if remaining <= 0 {
				log.Printf("[JARVIS] fileContext cap reached, skipping file %q", f.Name)
				break
			}
			header := fmt.Sprintf("--- arquivo: %s (tipo: %s, fonte: Google Drive) ---\n", f.Name, f.Mimetype)
			available := remaining - len(header) - 2
			if available <= 0 {
				break
			}
			b.WriteString(header)
			if len(content) > available {
				log.Printf("[JARVIS] truncating Google Sheets file %q: %d → %d chars (tail)", f.Name, len(content), available)
				b.WriteString(tailCSVContent(content, available))
				b.WriteString("\n[AVISO: conteúdo truncado — exibindo linhas mais recentes]\n")
			} else {
				b.WriteString(content)
			}
			b.WriteString("\n\n")
			continue
		}

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
			content, err = XlsxBytesToText(data)
			if err != nil {
				log.Printf("[JARVIS] failed to parse xlsx %q: %v", f.Name, err)
				continue
			}
		case isDocx:
			content, err = DocxBytesToText(data)
			if err != nil {
				log.Printf("[JARVIS] failed to parse docx %q: %v", f.Name, err)
				continue
			}
		case isPDF:
			content, err = PdfBytesToText(data)
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
