package app

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
	pdflib "github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

var reSplitOR = regexp.MustCompile(`(?i)\s+OR\s+`)
var reLastAND = regexp.MustCompile(`(?i)^(.*)\s+AND\s+(.+)$`)

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

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func missingFields(d jira.IssueDraft) []string {
	var missing []string
	if strings.TrimSpace(d.Project) == "" {
		missing = append(missing, "projeto")
	}
	if strings.TrimSpace(d.IssueType) == "" {
		missing = append(missing, "tipo")
	}
	return missing
}

func askForMissingFields(fields []string) string {
	return fmt.Sprintf(
		"Para criar o card preciso de: *%s*.\nResponda neste thread com essas informações.",
		strings.Join(fields, "*, *"),
	)
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

// extractJQLTextQuery extracts 1-3 meaningful keywords from a natural
// language question to use as a Jira text search term.  Common stopwords
// and intent verbs are stripped, so only the topic remains.
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

// fixJQLPrecedence fixes operator precedence when AND OR are mixed without
// explicit parentheses. Converts:
//
//	project in (X) AND text ~ "a" OR text ~ "b" OR text ~ "c"
//
// into:
//
//	a project in (X) AND (text ~ "a" OR text ~ "b" OR text ~ "c")
//
// It is a no-op when the JQL already contains proper grouping (AND (...)) or
// when there is no mixing of AND OR.
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

	// Split off the last AND in the first segment to get the prefix + first condition.
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

// buildJiraContext produces a formatted context summary from a slice
// of Jira issues.  If the number of issues exceeds 'limit,' it will
// group by status and summarize counts.
func buildJiraContext(issues []jira.SearchJQLRespIssue, limit int) string {
	if limit <= 0 {
		limit = 40
	}
	if len(issues) <= limit {
		return buildJiraContextSimple(issues)
	}
	return buildJiraContextGrouped(issues)
}

func buildJiraContextSimple(issues []jira.SearchJQLRespIssue) string {
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

func buildJiraContextGrouped(issues []jira.SearchJQLRespIssue) string {
	byStatus := make(map[string][]jira.SearchJQLRespIssue)
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

// docxBytesToText extracts plain text from a DOCX file (which is a ZIP
// containing word/document.xml). It preserves paragraph breaks.
// Uses only the standard library — no external dependency is required.
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
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String())
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
		sug = append(sug, "Tenta especificar o canal ou termos exatos (ex: 'reunião de equipe' ou '#geral').")
	}
	if len(sug) > 0 {
		base += "\n\nSugestões:\n• " + strings.Join(sug, "\n• ")
	}
	return base
}

// splitIntoChunks divides text into chunks of at most maxLen bytes,
// preferring to cut at newline boundaries to keep lines intact.
func splitIntoChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := maxLen
		// Try to cut at a newline in the latter half of the chunk.
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/3 {
			cut = idx + 1
		}
		chunks = append(chunks, strings.TrimRight(text[:cut], " \t"))
		text = strings.TrimLeft(text[cut:], "\n")
	}
	return chunks
}
