package testing

import (
	"regexp"
	"strings"
)

var reJiraKey = regexp.MustCompile(`[A-Z][A-Z0-9]+-\d+`)

// ValidatorFunc is a function that checks an answer and returns (passed, reason).
type ValidatorFunc func(answer string) (bool, string)

// SelectValidators picks the appropriate validators for a given question
// based on keyword heuristics.
func SelectValidators(question string) []ValidatorFunc {
	q := strings.ToLower(question)
	var validators []ValidatorFunc

	// Always check for hallucination patterns.
	validators = append(validators, ValidateNoHallucination)

	// Jira-specific validators: require real issue keys or explicit empty declaration.
	if strings.Contains(q, "projeto gr") || strings.Contains(q, "projeto tptdr") ||
		strings.Contains(q, "projeto inv") || strings.Contains(q, "project gr") ||
		strings.Contains(q, "project tptdr") || strings.Contains(q, "project inv") {
		validators = append(validators, ValidateJiraData)
	}

	// Numeric answer validators for aggregation questions.
	if strings.Contains(q, "quantas") || strings.Contains(q, "total") ||
		strings.Contains(q, "ranking") || strings.Contains(q, "quantos") {
		validators = append(validators, ValidateHasNumbers)
	}

	return validators
}

// ValidateNoHallucination detects common hallucination patterns in an answer.
// It fails when patterns that indicate the bot made up information are found.
func ValidateNoHallucination(answer string) (bool, string) {
	lower := strings.ToLower(answer)

	hallucination := []string{
		"não posso listar",
		"não tenho acesso",
		"não consigo acessar",
		"não é possível verificar",
	}
	for _, h := range hallucination {
		if strings.Contains(lower, h) {
			return false, `"` + h + `" detectado na resposta — possível alucinação ou erro de integração`
		}
	}

	// Detect invented email-style user mentions (e.g. @nome.sobrenome) outside of
	// proper Slack mention format (<@USERID>). These indicate hallucinated assignees.
	// Pattern: @word.word (not preceded by <)
	inventedMentionRe := regexp.MustCompile(`(?:^|[^<])@[a-z]+\.[a-z]+`)
	if inventedMentionRe.MatchString(lower) {
		return false, "mention do tipo @nome.sobrenome detectado — possível assignee inventado"
	}

	return true, ""
}

// ValidateJiraData requires the answer to contain at least one real Jira issue key
// (e.g. GR-123) OR an explicit declaration that no issues were found.
// Applied to questions that mention specific Jira projects.
func ValidateJiraData(answer string) (bool, string) {
	if reJiraKey.MatchString(answer) {
		return true, ""
	}

	lower := strings.ToLower(answer)
	emptyDeclarations := []string{
		"não foram encontradas",
		"nenhuma issue",
		"nenhum issue",
		"0 issues",
		"zero issues",
		"não encontrei",
		"sem issues",
		"não há issues",
		"busca falhou",
		"erro ao consultar",
	}
	for _, d := range emptyDeclarations {
		if strings.Contains(lower, d) {
			return true, ""
		}
	}

	return false, "resposta sem chave Jira real (ex: GR-123) nem declaração explícita de resultado vazio"
}

// ValidateHasNumbers requires at least one number in the answer.
// Applied to aggregation/count questions.
func ValidateHasNumbers(answer string) (bool, string) {
	hasDigit := regexp.MustCompile(`\d`)
	if hasDigit.MatchString(answer) {
		return true, ""
	}
	return false, "resposta sem nenhum número — esperado para pergunta de contagem/ranking"
}
