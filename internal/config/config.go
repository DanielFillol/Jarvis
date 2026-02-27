// internal/config/config.go
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

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
	// JiraProjectKeys is the list of Jira project keys to search by default
	// (e.g. ["PROJ", "BACKEND"]).  Set via JIRA_PROJECT_KEYS=PROJ,BACKEND.
	JiraProjectKeys []string
	// JiraProjectNameMap maps human-readable project names (lowercase) to their
	// Jira keys.  Loaded from JIRA_PROJECT_NAME_MAP=name1:KEY1,name2:KEY2.
	JiraProjectNameMap map[string]string
	JiraCreateEnabled  bool
	// BotName is the display name of the bot used in messages and prompts.
	// Defaults to "Jarvis".  Set via BOT_NAME=MyBot.
	BotName string
	// Metabase connection settings.  Authentication uses an API key
	// (Admin → Settings → Authentication → API Keys in Metabase ≥ 0.47).
	MetabaseBaseURL string
	MetabaseAPIKey  string
	// MetabaseSchemaPath is the output path for the generated schema
	// Markdown file.  Defaults to "./docs/metabase_schema.md".
	MetabaseSchemaPath string
	// MetabaseEnv is a free-form label included in the generated file
	// header (e.g. "production", "staging").  Defaults to "production".
	MetabaseEnv string
	// MetabaseQueryTimeout is the HTTP timeout for SQL execution via
	// /api/dataset.  Analytical databases like Redshift can be slow.
	// Defaults to 5 minutes.  Set via METABASE_QUERY_TIMEOUT=300s.
	MetabaseQueryTimeout time.Duration
}

// Load reads configuration from environment variables.  If a .env file
// exists in the working directory it will be loaded automatically via
// godotenv.Load.  Default values are applied for certain fields when
// environment variables are missing.  The returned Config may be
// partially populated; validation should be performed by the caller.
func Load() Config {
	// Attempt to load .env if present; ignore errors since variables may
	// already be set in the process environment.
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
	cfg.JiraProjectKeys = parseCSV(getEnv("JIRA_PROJECT_KEYS", ""))
	cfg.JiraProjectNameMap = parseProjectNameMap(os.Getenv("JIRA_PROJECT_NAME_MAP"))
	cfg.BotName = getEnv("BOT_NAME", "Jarvis")
	cfg.MetabaseBaseURL = os.Getenv("METABASE_BASE_URL")
	cfg.MetabaseAPIKey = os.Getenv("METABASE_API_KEY")
	cfg.MetabaseSchemaPath = getEnv("METABASE_SCHEMA_PATH", "./docs/metabase_schema.md")
	cfg.MetabaseEnv = getEnv("METABASE_ENV", "production")
	if qt, err := time.ParseDuration(getEnv("METABASE_QUERY_TIMEOUT", "5m")); err == nil {
		cfg.MetabaseQueryTimeout = qt
	} else {
		cfg.MetabaseQueryTimeout = 5 * time.Minute
	}
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

// parseProjectNameMap parses a string of the form "name1:KEY1,name2:KEY2"
// into a map from lowercased name to uppercase Jira key.
// Entries that are malformed or empty are silently ignored.
func parseProjectNameMap(s string) map[string]string {
	m := make(map[string]string)
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(strings.ToLower(parts[0]))
		key := strings.TrimSpace(strings.ToUpper(parts[1]))
		if name != "" && key != "" {
			m[name] = key
		}
	}
	return m
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
