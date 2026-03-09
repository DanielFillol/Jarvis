package main

import (
	"log"
	"net/http"

	"github.com/DanielFillol/Jarvis/internal/app"
	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/fileserver"
	httpinternal "github.com/DanielFillol/Jarvis/internal/http"
	"github.com/DanielFillol/Jarvis/internal/jira"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
	"github.com/DanielFillol/Jarvis/internal/outline"
	"github.com/DanielFillol/Jarvis/internal/slack"
)

func main() {
	// Load configuration (from .env and environment)
	cfg := config.Load()
	log.Printf("[BOOT] env check: SLACK_SIGNING_SECRET=%t SLACK_BOT_TOKEN=%t SLACK_USER_TOKEN=%t OPENAI_API_KEY=%t OPENAI_MODEL=%q OPENAI_LESSER_MODEL=%q JIRA_BASE_URL=%t JIRA_EMAIL=%t JIRA_API_TOKEN=%t JIRA_CREATE_ENABLED=%t JIRA_PROJECT_KEYS=%v BOT_NAME=%q", cfg.SlackSigningSecret != "", cfg.SlackBotToken != "", cfg.SlackUserToken != "", cfg.OpenAIAPIKey != "", cfg.OpenAIModel, cfg.OpenAILesserModel, cfg.JiraBaseURL != "", cfg.JiraEmail != "", cfg.JiraAPIToken != "", cfg.JiraCreateEnabled, cfg.JiraProjectKeys, cfg.BotName)

	// Initialize clients
	slackClient := slack.NewClient(cfg)
	jiraClient := jira.NewClient(cfg)
	llmClient := llm.NewClient(cfg)
	metabaseClient := metabase.NewClient(cfg)
	fs := fileserver.New()

	// Initialize optional Outline client (nil when not configured).
	var outlineClient *outline.Client
	if cfg.OutlineEnabled() {
		outlineClient = outline.NewClient(cfg.OutlineBaseURL, cfg.OutlineAPIKey)
		log.Printf("[BOOT] Outline enabled base_url=%q", cfg.OutlineBaseURL)
	}

	// Generate Jira project catalog asynchronously (enriches CatalogCompact from raw keys).
	if cfg.JiraEnabled() {
		go func() {
			catalog := jiraClient.GenerateCatalog(cfg.JiraProjectsPath)
			jiraClient.CatalogCompact = catalog
		}()
	}

	// Construct core service
	service := app.NewService(cfg, slackClient, jiraClient, llmClient, metabaseClient, fs, outlineClient)

	// Generate company context asynchronously from Jira + Metabase docs + Outline.
	go func() {
		ctx := app.GenerateCompanyContext(cfg, outlineClient, llmClient)
		if ctx != "" {
			service.SetCompanyCtx(ctx)
		}
	}()

	// Slack events endpoint
	slackHandler := httpinternal.NewSlackHandler(slackClient, service)

	mux := http.NewServeMux()
	mux.Handle("/slack/events", slackHandler)
	mux.Handle("/files/", fs)

	// Start HTTP server
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("[BOOT] ListenAndServe: %v", err)
	}
	log.Printf("[BOOT] Listening on :%s", cfg.Port)
}
