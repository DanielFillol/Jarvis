// cmd/jarvis/main.go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/DanielFillol/Jarvis/internal/app"
	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/github"
	httpinternal "github.com/DanielFillol/Jarvis/internal/http"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
	"github.com/DanielFillol/Jarvis/internal/state"
)

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
	githubClient := github.NewClient(cfg)
	log.Printf("[BOOT] github_enabled=%t org=%q repos=%v", githubClient.Enabled(), cfg.GitHubOrg, cfg.GitHubRepos)
	// Authenticate Slack bot to get bot user ID
	if id, err := slackClient.AuthTest(); err != nil {
		log.Printf("[SLACK] auth.test failed: %v", err)
	} else {
		slackClient.BotUserID = id
		log.Printf("[SLACK] bot_user_id=%s", id)
	}
	// Create a pending store with 2-hour TTL
	store := state.NewStore(2 * time.Hour)
	// Construct core service
	service := app.NewService(cfg, slackClient, jiraClient, llmClient, githubClient, store)
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
