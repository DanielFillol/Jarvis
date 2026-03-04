package app

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

// maybeHandleJiraCreateFlows orchestrates the state machine for Jira creation commands.
func (s *Service) maybeHandleJiraCreateFlows(channel, threadTs, originTs, originalText, question, threadHist string) (bool, error) {
	if !s.Cfg.JiraCreateEnabled {
		if s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
			_ = s.Slack.PostMessage(channel, threadTs, "Criação de issues no Jira está desabilitada.")
			return true, nil
		}
		return false, nil
	}

	if !s.LLM.ConfirmJiraCreateIntent(question, threadHist, s.Cfg.OpenAILesserModel, s.Cfg.OpenAIModel) {
		return false, nil
	}

	draft, err := s.LLM.ExtractIssueFromThread(threadHist, question, s.Cfg.OpenAILesserModel, nil, s.Cfg.JiraProjectNameMap)
	if err != nil {
		_ = s.Slack.PostMessage(channel, threadTs, fmt.Sprintf("Não consegui entender o card a partir da thread: %v", err))
		return true, nil
	}

	// 3. Se faltar info essencial, pergunta ao usuário e aguarda
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

	// 4. Cria o card
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
