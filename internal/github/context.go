// internal/github/context.go
package github

import (
	"fmt"
	"strings"
)

const maxContextChars = 3500

// BuildCodeContext takes search results and builds a formatted string to be
// included as context in an LLM prompt.  For each match it uses text-match
// fragments from the GitHub search response; if there are no fragments it
// falls back to fetching the first maxLinesPerFile lines of the file.
//
// maxFiles caps the number of files included.  The total output is limited
// to maxContextChars characters so we don't inflate the LLM prompt.
func BuildCodeContext(matches []CodeMatch, client *Client, maxFiles, maxLinesPerFile int) string {
	if len(matches) == 0 {
		return ""
	}
	if maxFiles <= 0 {
		maxFiles = 3
	}
	if maxLinesPerFile <= 0 {
		maxLinesPerFile = 60
	}
	if len(matches) > maxFiles {
		matches = matches[:maxFiles]
	}

	var b strings.Builder

	for i, m := range matches {
		header := fmt.Sprintf("### Arquivo %d: `%s/%s`\nURL: %s\n", i+1, m.Repo, m.Path, m.HTMLURL)
		b.WriteString(header)

		if len(m.Fragments) > 0 {
			b.WriteString("Trechos relevantes:\n```\n")
			for j, frag := range m.Fragments {
				if j >= 3 {
					break
				}
				// Limit each fragment to 400 chars
				if len(frag) > 400 {
					frag = frag[:400] + "…"
				}
				b.WriteString(frag)
				if j < len(m.Fragments)-1 && j < 2 {
					b.WriteString("\n---\n")
				}
			}
			b.WriteString("\n```\n\n")
		} else if client != nil && client.Enabled() {
			// Fall back to fetching file content when no text-match fragments
			content, err := client.GetFileContent(m.Repo, m.Path, maxLinesPerFile)
			if err == nil && content != "" {
				b.WriteString("Conteúdo (primeiras linhas):\n```\n")
				b.WriteString(content)
				b.WriteString("\n```\n\n")
			} else {
				b.WriteString("(conteúdo não disponível)\n\n")
			}
		} else {
			b.WriteString("(sem trechos disponíveis)\n\n")
		}

		// Hard limit: stop adding files if we're already near the char budget
		if b.Len() >= maxContextChars {
			remaining := len(matches) - (i + 1)
			if remaining > 0 {
				b.WriteString(fmt.Sprintf("… e mais %d arquivo(s) omitido(s) para não exceder o limite de contexto.\n", remaining))
			}
			break
		}
	}

	result := b.String()
	if len(result) > maxContextChars {
		result = result[:maxContextChars] + "\n… (contexto truncado)"
	}
	return result
}
