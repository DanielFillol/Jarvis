package hubspot

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PipelineStage represents a single stage within a HubSpot pipeline.
type PipelineStage struct {
	ID           string
	Label        string
	DisplayOrder int
}

// Pipeline represents a HubSpot deal or ticket pipeline with its stages.
type Pipeline struct {
	ID         string
	Label      string
	ObjectType string // "deals" | "tickets"
	Stages     []PipelineStage
}

// FetchPipelines fetches all pipelines for the given objectType ("deals" or "tickets")
// via GET /crm/v3/pipelines/{objectType}.
func (c *Client) FetchPipelines(objectType string) ([]*Pipeline, error) {
	url := fmt.Sprintf("%s/crm/v3/pipelines/%s", c.baseURL, objectType)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hubspot pipelines status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
	}

	var out struct {
		Results []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Stages []struct {
				ID           string `json:"id"`
				Label        string `json:"label"`
				DisplayOrder int    `json:"displayOrder"`
			} `json:"stages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("hubspot pipelines decode: %w", err)
	}

	pipelines := make([]*Pipeline, 0, len(out.Results))
	for _, r := range out.Results {
		stages := make([]PipelineStage, 0, len(r.Stages))
		for _, s := range r.Stages {
			stages = append(stages, PipelineStage{
				ID:           s.ID,
				Label:        s.Label,
				DisplayOrder: s.DisplayOrder,
			})
		}
		sort.Slice(stages, func(i, j int) bool {
			return stages[i].DisplayOrder < stages[j].DisplayOrder
		})
		pipelines = append(pipelines, &Pipeline{
			ID:         r.ID,
			Label:      r.Label,
			ObjectType: objectType,
			Stages:     stages,
		})
	}
	return pipelines, nil
}

// GenerateCatalog fetches deal and ticket pipelines, writes a full Markdown catalog
// to filePath, and returns a compact one-liner string for LLM prompts.
func (c *Client) GenerateCatalog(filePath string) string {
	var allPipelines []*Pipeline
	for _, objType := range []string{"deals", "tickets"} {
		pipelines, err := c.FetchPipelines(objType)
		if err != nil {
			log.Printf("[HUBSPOT] FetchPipelines %s failed: %v", objType, err)
			continue
		}
		allPipelines = append(allPipelines, pipelines...)
	}

	if len(allPipelines) == 0 {
		return ""
	}

	// Store pipelines for name→ID resolution in search queries.
	c.pipelines = allPipelines
	c.CatalogForLLM = formatCatalogForLLM(allPipelines)

	if filePath != "" {
		dir := filepath.Dir(filePath)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			md := renderCatalogMarkdown(allPipelines)
			if err := os.WriteFile(filePath, []byte(md), 0o644); err == nil {
				log.Printf("[HUBSPOT] catalog written to %s", filePath)
			} else {
				log.Printf("[HUBSPOT] catalog write failed: %v", err)
			}
		}
	}

	compact := formatCatalogCompact(allPipelines)
	return compact
}

// formatCatalogForLLM produces a compact ID→label mapping for all pipelines and stages,
// intended to be injected into the LLM context alongside HubSpot search results so the
// LLM can decode numeric dealstage/pipeline IDs.
func formatCatalogForLLM(pipelines []*Pipeline) string {
	var sb strings.Builder
	sb.WriteString("MAPEAMENTO IDs HubSpot (use para decodificar os campos dealstage e pipeline nos resultados):\n")
	for _, p := range pipelines {
		stageLabels := make([]string, 0, len(p.Stages))
		for _, s := range p.Stages {
			stageLabels = append(stageLabels, fmt.Sprintf("%s=%q", s.ID, s.Label))
		}
		sb.WriteString(fmt.Sprintf("%s/pipeline_id=%s %q: %s\n",
			p.ObjectType, p.ID, p.Label, strings.Join(stageLabels, " | ")))
	}
	return strings.TrimSpace(sb.String())
}

// formatCatalogCompact produces a compact one-liner string grouped by objectType.
// Example: "deals: Enterprise 3.0 [Prospect,Negotiation,Closed Won] | tickets: Suporte [Novo,Em andamento]"
func formatCatalogCompact(pipelines []*Pipeline) string {
	grouped := make(map[string][]*Pipeline)
	for _, p := range pipelines {
		grouped[p.ObjectType] = append(grouped[p.ObjectType], p)
	}

	var parts []string
	for _, objType := range []string{"deals", "tickets"} {
		ps, ok := grouped[objType]
		if !ok || len(ps) == 0 {
			continue
		}
		var pipelineParts []string
		for _, p := range ps {
			stageLabels := make([]string, 0, len(p.Stages))
			for _, s := range p.Stages {
				stageLabels = append(stageLabels, s.Label)
			}
			pipelineParts = append(pipelineParts, fmt.Sprintf("%s [%s]", p.Label, strings.Join(stageLabels, ",")))
		}
		parts = append(parts, fmt.Sprintf("%s: %s", objType, strings.Join(pipelineParts, " | ")))
	}
	return strings.Join(parts, " | ")
}

// renderCatalogMarkdown generates a full Markdown document describing all pipelines and their stages.
func renderCatalogMarkdown(pipelines []*Pipeline) string {
	var sb strings.Builder
	sb.WriteString("# Catálogo de Pipelines HubSpot\n\n")
	sb.WriteString(fmt.Sprintf("> Gerado em: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("---\n\n")

	grouped := make(map[string][]*Pipeline)
	for _, p := range pipelines {
		grouped[p.ObjectType] = append(grouped[p.ObjectType], p)
	}

	for _, objType := range []string{"deals", "tickets"} {
		ps, ok := grouped[objType]
		if !ok || len(ps) == 0 {
			continue
		}
		title := "Negociações (deals)"
		if objType == "tickets" {
			title = "Tickets de Suporte"
		}
		sb.WriteString(fmt.Sprintf("## %s\n\n", title))
		for _, p := range ps {
			sb.WriteString(fmt.Sprintf("### %s (pipeline_id: %s)\n\n", p.Label, p.ID))
			sb.WriteString("**Estágios (em ordem):**\n\n")
			for i, s := range p.Stages {
				sb.WriteString(fmt.Sprintf("%d. %s (stage_id: %s)\n", i+1, s.Label, s.ID))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
