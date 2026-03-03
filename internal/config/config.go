// internal/config/config.go
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config aggregates all environment variables used by the application.
// It is loaded once at startup via Load.  Fields marked as optional may
// remain empty; the caller is responsible for validating required fields.
type Config struct {
	// ── Required ─────────────────────────────────────────────────────────────
	Port                string
	SlackSigningSecret  string
	SlackBotToken       string
	SlackUserToken      string
	SlackSearchMaxPages int
	OpenAIAPIKey        string
	// OpenAIModel is the primary model used for heavy tasks such as answer
	// generation and bot introduction.  Defaults to "gpt-4o-mini".
	OpenAIModel string
	// OpenAILesserModel is a cheaper/faster model used for lightweight calls
	// (routing decisions, intent detection, SQL generation, issue extraction).
	// Falls back to OpenAIModel when empty.  Set via OPENAI_LESSER_MODEL.
	OpenAILesserModel string
	// BotName is the display name shown in messages and prompts.
	// Defaults to "Jarvis".
	BotName string

	// ── Optional: Jira ───────────────────────────────────────────────────────
	// Configure JIRA_BASE_URL + JIRA_EMAIL + JIRA_API_TOKEN to enable Jira
	// integration (issue lookup, search, creation).
	JiraBaseURL  string
	JiraEmail    string
	JiraAPIToken string
	// JiraProjectKeys is the list of default project keys to search
	// (e.g. ["PROJ", "BACKEND"]).  Set via JIRA_PROJECT_KEYS=PROJ,BACKEND.
	JiraProjectKeys []string
	// JiraProjectNameMap maps human-readable project names (lowercase) to
	// their Jira keys.  Set via JIRA_PROJECT_NAME_MAP=name1:KEY1,name2:KEY2.
	JiraProjectNameMap map[string]string
	// JiraCreateEnabled allows the bot to create Jira issues on behalf of
	// users.  Disabled by default.  Set via JIRA_CREATE_ENABLED=true.
	JiraCreateEnabled bool

	// ── Optional: Metabase ───────────────────────────────────────────────────
	// Configure METABASE_BASE_URL + METABASE_API_KEY to enable Metabase
	// integration (natural-language SQL queries against connected databases).
	// Authentication uses an API key (Admin → Settings → API Keys, Metabase ≥ 0.47).
	MetabaseBaseURL string
	MetabaseAPIKey  string
	// MetabaseSchemaPath is the output path for the generated schema
	// Markdown file.  Defaults to "./docs/metabase_schema.md".
	MetabaseSchemaPath string
	// MetabaseEnv is a free-form label included in the generated schema
	// header (e.g. "production", "staging").  Defaults to "production".
	MetabaseEnv string
	// MetabaseQueryTimeout is the HTTP timeout for SQL execution.
	// Analytical databases can be slow; tune this as needed.
	// Defaults to 5 minutes.  Set via METABASE_QUERY_TIMEOUT=300s.
	MetabaseQueryTimeout time.Duration
}

// LesserModel returns the lesser/auxiliary model identifier.  When
// OPENAI_LESSER_MODEL is not set, it falls back to OpenAIModel so that a
// single-model deployment works without any additional configuration.
func (c Config) LesserModel() string {
	if strings.TrimSpace(c.OpenAILesserModel) != "" {
		return c.OpenAILesserModel
	}
	return c.OpenAIModel
}

// JiraEnabled reports whether Jira credentials have been provided.
func (c Config) JiraEnabled() bool {
	return strings.TrimSpace(c.JiraBaseURL) != ""
}

// MetabaseEnabled reports whether Metabase credentials have been provided.
func (c Config) MetabaseEnabled() bool {
	return strings.TrimSpace(c.MetabaseBaseURL) != ""
}

// Load reads configuration from environment variables.  A .env file in the
// working directory is loaded automatically if present (godotenv); errors are
// ignored because variables may already be set in the process environment.
// Default values are applied where appropriate.
func Load() Config {
	_ = godotenv.Load()

	cfg := Config{}
	cfg.Port = getEnv("PORT", "8080")
	cfg.SlackSigningSecret = os.Getenv("SLACK_SIGNING_SECRET")
	cfg.SlackBotToken = os.Getenv("SLACK_BOT_TOKEN")
	cfg.SlackUserToken = os.Getenv("SLACK_USER_TOKEN")
	cfg.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	cfg.OpenAIModel = getEnv("OPENAI_MODEL", "gpt-4o-mini")
	cfg.OpenAILesserModel = os.Getenv("OPENAI_LESSER_MODEL")
	cfg.BotName = getEnv("BOT_NAME", "Jarvis")

	cfg.JiraBaseURL = os.Getenv("JIRA_BASE_URL")
	cfg.JiraEmail = os.Getenv("JIRA_EMAIL")
	cfg.JiraAPIToken = os.Getenv("JIRA_API_TOKEN")
	cfg.JiraCreateEnabled = strings.EqualFold(strings.TrimSpace(getEnv("JIRA_CREATE_ENABLED", "false")), "true")
	cfg.JiraProjectKeys = parseCSV(getEnv("JIRA_PROJECT_KEYS", ""))
	cfg.JiraProjectNameMap = parseProjectNameMap(os.Getenv("JIRA_PROJECT_NAME_MAP"))

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

// getEnv returns the trimmed value of an environment variable, or def when empty.
func getEnv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// parseProjectNameMap parses "name1:KEY1,name2:KEY2" into a map from
// lowercased name to uppercase Jira project key.
// Malformed or empty entries are silently ignored.
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

// parseCSV splits a comma-separated string into a trimmed, non-empty slice.
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
