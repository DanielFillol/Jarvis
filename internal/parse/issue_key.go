// internal/parse/issue_key.go
package parse

import (
	"regexp"
	"strings"
)

// ReIssueKey matches a JIRA issue key of the form ABC-123.  It is
// exported for reuse in other packages.
var ReIssueKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-\d+)\b`)

// ExtractIssueKey finds the first Jira issue key in the string and
// returns it in uppercase.  If none is found, an empty string is
// returned.
func ExtractIssueKey(s string) string {
	s = strings.ToUpper(s)
	m := ReIssueKey.FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
