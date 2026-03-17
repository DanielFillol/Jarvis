package app

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/outline"
)

// GenerateCompanyContext synthesizes Jira, Metabase, and Outline documentation
// into a compact domain glossary and writes it to cfg.CompanyContextPath.
// Returns the generated string, or "" if there is no source material.
func GenerateCompanyContext(cfg config.Config, outlineClient *outline.Client, llmClient *llm.Client) string {
	jiraDoc := readDocFile(cfg.JiraProjectsPath, 6000)

	compactMetabasePath := strings.TrimSuffix(cfg.MetabaseSchemaPath, ".md") + "_compact.md"
	metabaseDoc := readDocFile(compactMetabasePath, 6000)

	var outlineDocs string
	if outlineClient != nil {
		results, err := outlineClient.ListDocuments(15)
		if err != nil {
			log.Printf("[BOOT] company_context: outline list failed: %v", err)
		} else {
			outlineDocs = outline.FormatContext(results, 3000)
		}
	}

	hubspotDoc := readDocFile(cfg.HubSpotCatalogPath, 4000)

	if strings.TrimSpace(jiraDoc) == "" && strings.TrimSpace(metabaseDoc) == "" && strings.TrimSpace(outlineDocs) == "" && strings.TrimSpace(hubspotDoc) == "" {
		log.Printf("[BOOT] company_context: no source material available, skipping")
		return ""
	}

	ctx := llmClient.GenerateCompanyContext(jiraDoc, metabaseDoc, outlineDocs, hubspotDoc, cfg.OpenAILesserModel)
	if strings.TrimSpace(ctx) == "" {
		return ""
	}

	if err := os.MkdirAll(filepath.Dir(cfg.CompanyContextPath), 0o755); err != nil {
		log.Printf("[BOOT] company_context: mkdir failed: %v", err)
	} else if err := os.WriteFile(cfg.CompanyContextPath, []byte(ctx), 0o644); err != nil {
		log.Printf("[BOOT] company_context: write failed: %v", err)
	} else {
		log.Printf("[BOOT] company context written to %s (%d bytes)", cfg.CompanyContextPath, len(ctx))
	}

	return ctx
}
