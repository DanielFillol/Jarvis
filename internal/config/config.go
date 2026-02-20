// internal/config/config.go
package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config aggregates all environment variables required by the application.
// It is loaded once on startup via the Load function.  Fields that are
// optional may remain empty; required fields should be validated by the
// caller as appropriate.
type Config struct {
	Port                string
	SlackSigningSecret  string
	SlackBotToken       string
	SlackUserToken      string
	SlackSearchMaxPages int
	OpenAIAPIKey        string
	OpenAIModel         string
	OpenAIFallbackModel string
	JiraBaseURL         string
	JiraEmail           string
	JiraAPIToken        string
	JiraProjectKeys     []string
	JiraCreateEnabled   bool
}

// Load reads configuration from environment variables.  If a .env file
// exists in the working directory it will be loaded automatically via
// godotenv.Load.  Default values are applied for certain fields when
// environment variables are missing.  The returned Config may be
// partially populated; validation should be performed by the caller.
func Load() Config {
	// Attempt to load .env if present; ignore errors since variables may
	// already be set in the process environment.  This mirrors the
	// behavior in the original monolith.
	_ = godotenv.Load()

	cfg := Config{}
	cfg.Port = getEnv("PORT", "8080")
	cfg.SlackSigningSecret = os.Getenv("SLACK_SIGNING_SECRET")
	cfg.SlackBotToken = os.Getenv("SLACK_BOT_TOKEN")
	cfg.SlackUserToken = os.Getenv("SLACK_USER_TOKEN")
	cfg.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	cfg.OpenAIModel = getEnv("OPENAI_MODEL", "gpt-4o-mini")
	cfg.OpenAIFallbackModel = os.Getenv("OPENAI_FALLBACK_MODEL")
	cfg.JiraBaseURL = os.Getenv("JIRA_BASE_URL")
	cfg.JiraEmail = os.Getenv("JIRA_EMAIL")
	cfg.JiraAPIToken = os.Getenv("JIRA_API_TOKEN")
	cfg.JiraCreateEnabled = strings.EqualFold(strings.TrimSpace(getEnv("JIRA_CREATE_ENABLED", "false")), "true")
	cfg.JiraProjectKeys = parseCSV(getEnv("JIRA_PROJECT_KEYS", "TPTDR,INV,GR"))
	pages := getEnv("SLACK_SEARCH_MAX_PAGES", "10")
	if n, err := strconv.Atoi(pages); err == nil {
		cfg.SlackSearchMaxPages = n
	} else {
		cfg.SlackSearchMaxPages = 10
	}
	return cfg
}

// getEnv returns the trimmed value of an environment variable or a default if empty.
func getEnv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// parseCSV splits a comma-separated string into a slice of strings,
// trimming whitespace around each entry and omitting empty entries.
func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
