// internal/parse/jira_commands.go
package parse

import (
	"regexp"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// ParseJiraCreateExplicit parses an explicit Jira create command of the
// form "jira criar | PROJ | Tipo | Título | Descrição...".  It returns
// true if the prefix matches and populates a draft with whatever
// fields are provided.  Missing project or type are left empty and
// should be requested from the user.
func ParseJiraCreateExplicit(q string) (bool, jira.IssueDraft) {
	t := strings.TrimSpace(q)
	low := strings.ToLower(t)
	if !strings.HasPrefix(low, "jira criar") {
		return false, jira.IssueDraft{}
	}
	rest := strings.TrimSpace(t[len("jira criar"):])
	parts := strings.Split(rest, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	var d jira.IssueDraft
	if len(parts) < 3 {
		// If fewer than 3 parts, treat the entire rest as summary
		return true, jira.IssueDraft{Summary: strings.TrimSpace(rest)}
	}
	d.Project = parts[0]
	d.IssueType = parts[1]
	d.Summary = parts[2]
	if len(parts) >= 4 {
		d.Description = strings.TrimSpace(strings.Join(parts[3:], " | "))
	}
	// Extract priority and labels from the description, returning the clean description
	d.Priority, d.Labels, d.Description = ExtractExtrasFromText(d.Description)
	return true, d
}

// ApplyJiraDefine updates an existing draft with fields from a "jira
// definir" command.  The syntax accepted is "jira definir | projeto=X |
// tipo=Y | titulo=Z | prioridade=P | labels=a,b,c".  Returns true if
// at least one field was updated.
func ApplyJiraDefine(q string, d *jira.IssueDraft) bool {
	t := strings.TrimSpace(q)
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "jira definir") {
		t = strings.TrimSpace(t[len("jira definir"):])
	} else if strings.HasPrefix(low, "jira set") {
		t = strings.TrimSpace(t[len("jira set"):])
	} else {
		return false
	}
	parts := strings.Split(t, "|")
	updated := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.TrimSpace(kv[1])
		switch k {
		case "projeto", "project":
			d.Project = v
			updated = true
		case "tipo", "type":
			d.IssueType = v
			updated = true
		case "titulo", "título", "summary":
			d.Summary = v
			updated = true
		case "prioridade", "priority":
			d.Priority = v
			updated = true
		case "labels", "label":
			d.Labels = SplitCSV(v)
			updated = true
		}
	}
	return updated
}

// SplitCSV splits a comma-separated string into a slice of trimmed strings.
// Empty values are omitted.
func SplitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ExtractExtrasFromText looks for priority=... and labels=... in the
// description text, removing them and returning the extracted values
// along with the cleaned description.  This allows users to specify
// these fields inline in the description portion of the explicit creation
// syntax.
func ExtractExtrasFromText(desc string) (priority string, labels []string, cleanDesc string) {
	cleanDesc = strings.TrimSpace(desc)
	if cleanDesc == "" {
		return "", nil, ""
	}

	low := strings.ToLower(cleanDesc)

	// priority=
	if idx := strings.Index(low, "priority="); idx >= 0 {
		after := cleanDesc[idx+len("priority="):]
		val := ReadUntilSpaceOrPipe(after)
		if val != "" {
			priority = strings.TrimSpace(val)
			cleanDesc = strings.TrimSpace(strings.Replace(cleanDesc, cleanDesc[idx:idx+len("priority=")+len(val)], "", 1))
		}
	}

	// labels=
	low2 := strings.ToLower(cleanDesc)
	if idx := strings.Index(low2, "labels="); idx >= 0 {
		after := cleanDesc[idx+len("labels="):]
		val := ReadUntilSpaceOrPipe(after)
		if val != "" {
			labels = SplitCSV(val)
			cleanDesc = strings.TrimSpace(strings.Replace(cleanDesc, cleanDesc[idx:idx+len("labels=")+len(val)], "", 1))
		}
	}

	cleanDesc = strings.ReplaceAll(cleanDesc, "  ", " ")
	cleanDesc = strings.Trim(cleanDesc, " |")
	return priority, labels, strings.TrimSpace(cleanDesc)
}

// ReadUntilSpaceOrPipe returns the substring up to the first space,
// pipe, newline or tab.  It is used by ExtractExtrasFromText to
// delimit values.
func ReadUntilSpaceOrPipe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	out := s
	for i, r := range s {
		if r == ' ' || r == '|' || r == '\n' || r == '\t' {
			out = s[:i]
			break
		}
	}
	return strings.TrimSpace(out)
}

// projectNameToKey maps human-readable project names (lowercase) to their
// Jira keys.  It is empty by default and populated at startup via
// SetProjectNameMap using values from the JIRA_PROJECT_NAME_MAP env var.
var projectNameToKey = map[string]string{}

// SetProjectNameMap replaces the project name→key lookup table used by
// ParseProjectKeyFromText and LooksLikeJiraCreateIntent.  It should be
// called once during application startup after loading configuration.
// The supplied map must use lowercase names as keys and uppercase Jira
// project keys as values (e.g. {"backend": "BE", "frontend": "FE"}).
func SetProjectNameMap(m map[string]string) {
	projectNameToKey = m
}

// ParseProjectKeyFromText attempts to extract a project key from a
// natural language string.  Keys consist of uppercase letters and
// digits and must appear following a word like "projeto", "project" or
// "prefixo".
func ParseProjectKeyFromText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	low := strings.ToLower(s)

	// 1) Map configured project names: "projeto do backend" -> BE (via SetProjectNameMap)
	reProjectName := regexp.MustCompile(`(?i)\b(?:projeto|prefixo|project)\s+(?:do|da|de|of|the)\s+(\w+)\b`)
	if m := reProjectName.FindStringSubmatch(s); len(m) >= 2 {
		name := strings.ToLower(strings.TrimSpace(m[1]))
		if key, ok := projectNameToKey[name]; ok {
			return key
		}
	}

	// 2) Direct project name mention alongside project/prefixo keyword (uses configured map)
	for name, key := range projectNameToKey {
		if strings.Contains(low, name) && (strings.Contains(low, "projeto") || strings.Contains(low, "prefixo") || strings.Contains(low, "project")) {
			return key
		}
	}

	// 2b) Destination preposition: "no backend", "na OPS", "em INFRA", etc.
	// This covers create-card phrases where "projeto" is absent but the destination is unambiguous.
	for name, key := range projectNameToKey {
		if strings.Contains(low, "no "+name) || strings.Contains(low, "na "+name) || strings.Contains(low, "em "+name) {
			return key
		}
	}

	// 3) Explicit key: "projeto BE", "prefixo=BACKEND" (excludes common articles)
	re := regexp.MustCompile(`(?i)\b(prefixo|projeto|project)\s*[:=]?\s*([A-Z][A-Z0-9]+)\b`)
	if m := re.FindStringSubmatch(s); len(m) >= 3 {
		k := strings.ToUpper(strings.TrimSpace(m[2]))
		// Exclude common articles and "V2"
		if k == "V2" || k == "DO" || k == "DA" || k == "DE" {
			return ""
		}
		return k
	}

	// 4) "roadmap do PROJ" / "roadmap da PROJ" / "roadmap de PROJ"
	if strings.Contains(low, "roadmap") {
		reRoadmap := regexp.MustCompile(`(?i)\broadmap\s+(?:do|da|de)\s+([A-Z][A-Z0-9]+)\b`)
		if m := reRoadmap.FindStringSubmatch(s); len(m) >= 2 {
			k := strings.ToUpper(strings.TrimSpace(m[1]))
			if k == "V2" {
				return ""
			}
			return k
		}

		// 5) Fallback: "roadmap PROJ"
		reRoadmap2 := regexp.MustCompile(`(?i)\broadmap\s+([A-Z][A-Z0-9]+)\b`)
		if m := reRoadmap2.FindStringSubmatch(s); len(m) >= 2 {
			k := strings.ToUpper(strings.TrimSpace(m[1]))
			if k == "V2" {
				return ""
			}
			return k
		}
	}

	return ""
}

// ParseIssueTypeFromText attempts to infer the issue type from a
// natural language string.  It returns one of the allowed values such
// as "Epic", "História", "Bug", "Tarefa", "Subtarefa" or "Spike".
func ParseIssueTypeFromText(s string) string {
	low := strings.ToLower(s)
	if strings.Contains(low, "épico") || strings.Contains(low, "epico") || strings.Contains(low, "epic") {
		return "Epic"
	}
	if strings.Contains(low, "história") || strings.Contains(low, "historia") {
		return "História"
	}
	if strings.Contains(low, "bug") {
		return "Bug"
	}
	if strings.Contains(low, "tarefa") {
		return "Tarefa"
	}
	if strings.Contains(low, "subtarefa") {
		return "Subtarefa"
	}
	if strings.Contains(low, "spike") {
		return "Spike"
	}
	return ""
}

// ParseSummaryFromText attempts to extract a title/summary from a
// natural language string.  It looks for quoted content or patterns
// like "título: X".  The returned summary is trimmed and truncated to
// 140 characters.  If no summary can be extracted, an empty string is
// returned.
func ParseSummaryFromText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Quoted title first
	reQ := regexp.MustCompile(`"([^"]+)"`)
	if m := reQ.FindStringSubmatch(s); len(m) >= 2 {
		out := strings.TrimSpace(m[1])
		if len(out) > 140 {
			out = out[:140]
		}
		return out
	}
	// "título: X"
	re := regexp.MustCompile(`(?i)\b(título|titulo|title)\s*[:=]\s*(.+)$`)
	if m := re.FindStringSubmatch(s); len(m) >= 3 {
		out := strings.TrimSpace(m[2])
		if len(out) > 140 {
			out = out[:140]
		}
		return out
	}
	return ""
}

// CleanTitle normalizes and truncates a title to 140 characters.
func CleanTitle(x string) string {
	y := strings.TrimSpace(x)
	reTail := regexp.MustCompile(`(?i)\s*([.,])\s*(do\s+tipo|tipo|no\s+projeto|projeto|prefixo|board)\b.*$`)
	y = reTail.ReplaceAllString(y, "")
	y = strings.Join(strings.Fields(y), " ")
	y = strings.TrimRight(y, ". ")
	if len(y) > 140 {
		y = y[:140]
	}
	return y
}

// jiraCreateVerbs are the verb forms that indicate a Jira creation intent.
var jiraCreateVerbs = []string{"crie", "cria", "criar", "abra", "abre", "abrir"}

// hasCreateVerb returns true if any creation verb is present in the lowercased string.
func hasCreateVerb(low string) bool {
	for _, v := range jiraCreateVerbs {
		if strings.Contains(low, v) {
			return true
		}
	}
	return false
}

// LooksLikeJiraCreateIntent returns true if the string appears to
// express an intent to create a Jira card.  The string should be
// lowercased before calling this function.
func LooksLikeJiraCreateIntent(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	if low == "" {
		return false
	}
	// Explicit artifact words with any creation verb
	artifacts := []string{"card", "ticket", "issue", "história", "historia", "bug", "épico", "epico", "tarefa"}
	if hasCreateVerb(low) {
		for _, a := range artifacts {
			if strings.Contains(low, a) {
				return true
			}
		}
	}
	// "no jira" or known project destinations with any creation verb
	if hasCreateVerb(low) && (strings.Contains(low, " no jira") || strings.Contains(low, " no projeto") || strings.Contains(low, " no portal")) {
		return true
	}
	// Known project key/name mentioned alongside a creation verb
	for name := range projectNameToKey {
		if strings.Contains(low, name) && hasCreateVerb(low) {
			return true
		}
	}
	return false
}

// threadPhrases are the phrases that indicate a thread-based creation context.
var threadPhrases = []string{
	"com base nessa thread",
	"com base na thread",
	"baseado nessa thread",
	"baseado na thread",
	"baseada nessa thread",
	"baseada na thread",
	"a partir dessa thread",
	"a partir da thread",
	"dessa thread",
	"nessa thread",
}

// IsThreadBasedCreate returns true if the user wants to create a card
// based on the current thread.
func IsThreadBasedCreate(q string) bool {
	low := strings.ToLower(strings.TrimSpace(q))
	for _, phrase := range threadPhrases {
		if strings.Contains(low, phrase) &&
			(hasCreateVerb(low) || strings.Contains(low, "card") || strings.Contains(low, "jira")) {
			return true
		}
	}
	return false
}

// IsMultiCardCreate returns true when the user explicitly asks for more than
// one card in a single request (e.g. "crie dois cards", "um sobre X e outro Y").
func IsMultiCardCreate(q string) bool {
	low := strings.ToLower(strings.TrimSpace(q))
	// Explicit numeric qualifiers
	if strings.Contains(low, "dois card") || strings.Contains(low, "duas card") ||
		strings.Contains(low, "dois ticket") || strings.Contains(low, "duas ticket") ||
		strings.Contains(low, "dois issue") || strings.Contains(low, "duas issue") ||
		strings.Contains(low, "múltiplos card") || strings.Contains(low, "vários card") ||
		strings.Contains(low, "multiplos card") || strings.Contains(low, "varios card") {
		return true
	}
	// "um sobre … e outro …" pattern
	if strings.Contains(low, "um sobre") && strings.Contains(low, "outro") {
		return true
	}
	return false
}
