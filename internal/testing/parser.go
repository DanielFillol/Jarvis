package testing

import (
	"bufio"
	"os"
	"strings"
)

// PromptTest represents a single testable prompt extracted from the prompt library.
type PromptTest struct {
	Section  string // e.g. "Gestão Jira"
	Name     string // e.g. "Listar bugs abertos em um projeto"
	Question string // e.g. "Quais são os bugs abertos no projeto GR?"
}

// ParsePromptLibrary reads a prompt library Markdown file and extracts all
// PromptTest entries. It looks for:
//   - `## N. SECTION_NAME` lines to set the current section
//   - `### Prompt: NAME` lines to start a new test
//   - lines matching `> `@Jarvis ...“ to capture the question
func ParsePromptLibrary(path string) ([]PromptTest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tests []PromptTest
	var currentSection, currentName, pendingName string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Section header: ## 1. Análise de Coletas
		if strings.HasPrefix(trimmed, "## ") {
			text := strings.TrimPrefix(trimmed, "## ")
			// Strip leading "N. " numbering if present
			if idx := strings.Index(text, ". "); idx >= 0 && idx < 4 {
				text = text[idx+2:]
			}
			currentSection = strings.TrimSpace(text)
			continue
		}

		// Test name: ### Prompt: Listar bugs abertos em um projeto
		if strings.HasPrefix(trimmed, "### Prompt: ") {
			pendingName = strings.TrimPrefix(trimmed, "### Prompt: ")
			pendingName = strings.TrimSpace(pendingName)
			currentName = pendingName
			continue
		}

		// Question line: > `@Jarvis ...`
		if strings.HasPrefix(trimmed, "> `@Jarvis ") && strings.HasSuffix(trimmed, "`") {
			if currentName == "" {
				continue
			}
			q := strings.TrimPrefix(trimmed, "> `@Jarvis ")
			q = strings.TrimSuffix(q, "`")
			q = strings.TrimSpace(q)
			if q != "" {
				tests = append(tests, PromptTest{
					Section:  currentSection,
					Name:     currentName,
					Question: q,
				})
				currentName = "" // consume — one question per prompt block
			}
			continue
		}

		_ = pendingName // suppress unused warning
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return tests, nil
}
