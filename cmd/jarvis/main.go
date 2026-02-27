// cmd/jarvis/main.go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/DanielFillol/Jarvis/internal/app"
	"github.com/DanielFillol/Jarvis/internal/config"
	httpinternal "github.com/DanielFillol/Jarvis/internal/http"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

// initMetabase creates the Metabase client, lists available databases and
// saved questions, discovers accessible schemas per database, and starts
// schema generation in the background.
// Returns a nil client when Metabase is not configured or credentials are missing.
func initMetabase(cfg config.Config) (*metabase.Client, []metabase.Database, []metabase.Card, map[int][]string) {
	if cfg.MetabaseBaseURL == "" || cfg.MetabaseAPIKey == "" {
		log.Printf("[METABASE] not configured — set METABASE_BASE_URL + METABASE_API_KEY to enable")
		return nil, nil, nil, nil
	}

	client := metabase.NewClientAPIKey(cfg.MetabaseBaseURL, cfg.MetabaseAPIKey, cfg.MetabaseQueryTimeout)

	dbs, err := client.ListDatabases()
	if err != nil {
		log.Printf("[METABASE] ListDatabases failed: %v — Metabase integration disabled", err)
		return nil, nil, nil, nil
	}
	for _, db := range dbs {
		log.Printf("[METABASE] database id=%d name=%q engine=%s", db.ID, db.Name, db.Engine)
	}

	// Discover which schemas are actually queryable per database so we can
	// filter the compact schema and prevent the LLM from using phantom schemas.
	accessibleSchemas := make(map[int][]string)
	for _, db := range dbs {
		schemas, err := client.ListAccessibleSchemas(db.ID)
		if err != nil {
			log.Printf("[METABASE] ListAccessibleSchemas db=%d failed: %v — schema filtering disabled for this db", db.ID, err)
			continue
		}
		accessibleSchemas[db.ID] = schemas
		log.Printf("[METABASE] db=%d accessible schemas: %v", db.ID, schemas)
	}

	// Load saved questions (cards) for use as SQL examples during query generation.
	// The list endpoint returns names/IDs only; individual card SQL is fetched on demand.
	cards, err := client.ListCards()
	if err != nil {
		log.Printf("[METABASE] ListCards failed: %v — saved questions will not be used as examples", err)
	} else {
		log.Printf("[METABASE] loaded %d saved questions for keyword matching (SQL fetched on demand)", len(cards))
	}

	// Generate schema documentation asynchronously so startup is not blocked.
	go func() {
		if err := metabase.GenerateSchemaDoc(client, cfg.MetabaseSchemaPath, cfg.MetabaseEnv); err != nil {
			log.Printf("[METABASE] schema generation failed: %v", err)
		}
	}()

	return client, dbs, cards, accessibleSchemas
}

func main() {
	// Load configuration (from .env and environment)
	cfg := config.Load()
	log.Printf("[BOOT] env check: SLACK_SIGNING_SECRET=%t SLACK_BOT_TOKEN=%t SLACK_USER_TOKEN=%t OPENAI_API_KEY=%t OPENAI_MODEL=%q OPENAI_FALLBACK_MODEL=%q JIRA_BASE_URL=%t JIRA_EMAIL=%t JIRA_API_TOKEN=%t JIRA_CREATE_ENABLED=%t JIRA_PROJECT_KEYS=%v BOT_NAME=%q", cfg.SlackSigningSecret != "", cfg.SlackBotToken != "", cfg.SlackUserToken != "", cfg.OpenAIAPIKey != "", cfg.OpenAIModel, cfg.OpenAIFallbackModel, cfg.JiraBaseURL != "", cfg.JiraEmail != "", cfg.JiraAPIToken != "", cfg.JiraCreateEnabled, cfg.JiraProjectKeys, cfg.BotName)
	// Register project name→key mapping for natural language parsing
	parse.SetProjectNameMap(cfg.JiraProjectNameMap)
	// Initialize clients
	slackClient := slack.NewClient(cfg)
	jiraClient := jira.NewClient(cfg)
	llmClient := llm.NewClient(cfg)
	// Authenticate Slack bot to get bot user ID
	if id, err := slackClient.AuthTest(); err != nil {
		log.Printf("[SLACK] auth.test failed: %v", err)
	} else {
		slackClient.BotUserID = id
		log.Printf("[SLACK] bot_user_id=%s", id)
	}
	// Identify user token owner so we can resolve their user ID without users:read scope
	if uid, uname, err := slackClient.AuthTestUserToken(); err != nil {
		log.Printf("[SLACK] auth.test (user token) failed: %v", err)
	} else {
		log.Printf("[SLACK] user_token_owner=%s username=%s", uid, uname)
	}
	// Initialize Metabase (creates client, lists databases, discovers schemas, loads saved questions, starts async schema generation).
	metabaseClient, metabaseDatabases, metabaseCards, metabaseAccessibleSchemas := initMetabase(cfg)
	// Create a pending store with 2-hour TTL
	store := state.NewStore(2 * time.Hour)
	// Construct core service
	service := app.NewService(cfg, slackClient, jiraClient, llmClient, metabaseClient, metabaseDatabases, metabaseCards, metabaseAccessibleSchemas, store)
	// Register HTTP handlers
	mux := http.NewServeMux()
	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] /health method=%s remote=%s", r.Method, r.RemoteAddr)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	// Slack events endpoint
	slackHandler := httpinternal.NewSlackHandler(slackClient, service)
	mux.Handle("/slack/events", slackHandler)
	// Start HTTP server
	port := cfg.Port
	log.Printf("[BOOT] starting Jarvis port=%s", port)
	log.Printf("[BOOT] Listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("[BOOT] ListenAndServe: %v", err)
	}
}
