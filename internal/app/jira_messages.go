// internal/app/jira_messages.go
package app

import (
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// missingFieldsMsg builds the Slack mrkdwn message used when a Jira issue draft
// is missing required fields (project and/or issue type).
// It instructs the user how to provide the missing values using the
// `jarvis: jira definir | projeto=... | tipo=...` command and includes a short
// summary of the current draft so the user can confirm what will be created.
func missingFieldsMsg(d jira.IssueDraft, needProject, needType bool) string {
	var missing []string

	if needProject {
		missing = append(missing, "projeto")
	}
	if needType {
		missing = append(missing, "tipo")
	}

	msg := "Preciso de mais informações para criar o card.\n\n"

	if len(missing) > 0 {
		msg += fmt.Sprintf("Faltando: *%s*\n\n", strings.Join(missing, " e "))
	}

	msg += fmt.Sprintf(
		"*Resumo:* %s\n*Projeto:* %s\n*Tipo:* %s\n\n",
		orDash(d.Summary),
		orDash(d.Project),
		orDash(d.IssueType),
	)

	msg += "Responda com:\n"
	msg += "`jarvis: jira definir | projeto=ABC | tipo=Bug`"

	return msg
}

// previewDraftMsg builds a Slack Markdown preview of the Jira issue draft.
// It is used after the draft is assembled (especially in the thread-based flow)
// so the user can review the fields before confirming the creation.
// If includeConfirmHint is true, it appends instructions for confirming or
// canceling the pending draft.
func previewDraftMsg(d jira.IssueDraft, includeConfirmHint bool) string {
	msg := "🧾 *Prévia do card:*\n\n"

	msg += fmt.Sprintf("*Projeto:* %s\n", orDash(d.Project))
	msg += fmt.Sprintf("*Tipo:* %s\n", orDash(d.IssueType))
	msg += fmt.Sprintf("*Resumo:* %s\n\n", orDash(d.Summary))

	if includeConfirmHint {
		msg += "Se estiver ok, responda: `jarvis: confirmar`\n"
		msg += "Se quiser descartar: `jarvis: cancelar card`"
	}

	return msg
}

// previewMultipleDraftsMsg builds a Slack Markdown preview for a list of Jira
// issue drafts.  It numbers each card and appends confirm/cancel instructions.
func previewMultipleDraftsMsg(drafts []jira.IssueDraft) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🧾 *Prévia dos %d cards:*\n\n", len(drafts)))
	for i, d := range drafts {
		b.WriteString(fmt.Sprintf("*Card %d*\n", i+1))
		b.WriteString(fmt.Sprintf("*Projeto:* %s\n", orDash(d.Project)))
		b.WriteString(fmt.Sprintf("*Tipo:* %s\n", orDash(d.IssueType)))
		b.WriteString(fmt.Sprintf("*Resumo:* %s\n\n", orDash(d.Summary)))
	}
	b.WriteString("Se estiver ok, responda: `jarvis: confirmar`\n")
	b.WriteString("Se quiser descartar: `jarvis: cancelar card`")
	return b.String()
}

// orDash returns an em dash when the given string is empty or whitespace-only.
// It is a small formatting helper used by Slack messages to avoid blank fields.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
