// internal/jira/adf.go
package jira

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	reTask    = regexp.MustCompile(`^-\s+\[([xX ])\]\s*(.*)$`)
	reBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reHR      = regexp.MustCompile(`^-{3,}$|^\*{3,}$|^_{3,}$`)
)

// isBulletLine returns true for lines like "- text" or "* text" that are NOT task items.
func isBulletLine(s string) bool {
	if reTask.MatchString(s) {
		return false
	}
	return strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ")
}

// MarkdownToADF converts Markdown text produced by the LLM into Atlassian
// Document Format (ADF) for Jira Cloud API v3.
//
// Supported constructs:
//   - Headings:    ## Title  →  heading level 2
//   - Bullet list: - item    →  bulletList / listItem
//   - Task list:   - [ ] …   →  taskList / taskItem
//   - Bold inline: **text**  →  strong mark
//   - Separator:   ---       →  rule node
//   - Everything else        →  paragraph
func MarkdownToADF(text string) map[string]any {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	counter := 0
	nodes := parseMDBlocks(lines, &counter)
	if len(nodes) == 0 {
		nodes = []any{adfParagraph([]any{adfText(" ")})}
	}
	return map[string]any{"type": "doc", "version": 1, "content": nodes}
}

func parseMDBlocks(lines []string, counter *int) []any {
	var nodes []any
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])

		// Heading: ## Title — add a rule separator before every heading except the first
		if m := reHeading.FindStringSubmatch(trimmed); m != nil {
			if len(nodes) > 0 {
				nodes = append(nodes, map[string]any{"type": "rule"})
			}
			nodes = append(nodes, map[string]any{
				"type":    "heading",
				"attrs":   map[string]any{"level": len(m[1])},
				"content": parseInline(m[2]),
			})
			i++
			continue
		}

		// Horizontal rule: ---
		if reHR.MatchString(trimmed) {
			nodes = append(nodes, map[string]any{"type": "rule"})
			i++
			continue
		}

		// Task list: - [ ] … or - [x] …
		if reTask.MatchString(trimmed) {
			*counter++
			listID := fmt.Sprintf("tl-%d", *counter)
			var items []any
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				m := reTask.FindStringSubmatch(t)
				if m == nil {
					break
				}
				*counter++
				state := "TODO"
				if strings.ToLower(m[1]) == "x" {
					state = "DONE"
				}
				items = append(items, map[string]any{
					"type":    "taskItem",
					"attrs":   map[string]any{"localId": fmt.Sprintf("ti-%d", *counter), "state": state},
					"content": parseInline(m[2]),
				})
				i++
			}
			nodes = append(nodes, map[string]any{
				"type":    "taskList",
				"attrs":   map[string]any{"localId": listID},
				"content": items,
			})
			continue
		}

		// Bullet list: - item or * item (flatten sub-items to same level)
		if isBulletLine(trimmed) {
			var items []any
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if !isBulletLine(t) {
					break
				}
				items = append(items, map[string]any{
					"type": "listItem",
					"content": []any{map[string]any{
						"type":    "paragraph",
						"content": parseInline(t[2:]),
					}},
				})
				i++
			}
			if len(items) > 0 {
				nodes = append(nodes, map[string]any{
					"type":    "bulletList",
					"content": items,
				})
			}
			continue
		}

		// Empty line → skip (blank lines separate blocks in Markdown)
		if trimmed == "" {
			i++
			continue
		}

		// Regular paragraph
		nodes = append(nodes, adfParagraph(parseInline(trimmed)))
		i++
	}
	return nodes
}

// parseInline converts inline Markdown (**bold**) to ADF text content nodes.
func parseInline(s string) []any {
	if strings.TrimSpace(s) == "" {
		return []any{adfText(" ")}
	}
	var result []any
	last := 0
	for _, m := range reBold.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			result = append(result, adfText(s[last:m[0]]))
		}
		result = append(result, map[string]any{
			"type":  "text",
			"text":  s[m[2]:m[3]],
			"marks": []any{map[string]any{"type": "strong"}},
		})
		last = m[1]
	}
	if last < len(s) {
		result = append(result, adfText(s[last:]))
	}
	if len(result) == 0 {
		result = []any{adfText(" ")}
	}
	return result
}

func adfParagraph(content []any) map[string]any {
	return map[string]any{"type": "paragraph", "content": content}
}

func adfText(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// TextToADF is kept for backwards compatibility but now delegates to MarkdownToADF.
func TextToADF(text string) map[string]any {
	return MarkdownToADF(text)
}
