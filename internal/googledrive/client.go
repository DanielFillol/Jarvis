package googledrive

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/DanielFillol/Jarvis/internal/config"
)

const maxFileSize = 5 * 1024 * 1024 // 5 MB per file

// SearchResult holds a single Google Drive search hit with extracted content.
type SearchResult struct {
	ID       string
	Name     string
	MimeType string
	WebURL   string
	Content  string // extracted plain text
}

// Client provides file search and content extraction for Google Drive.
// Parsers are injected to avoid circular imports with the app package.
type Client struct {
	svc         *drive.Service
	folderID    string
	searchLimit int
	pdfParser   func([]byte) (string, error)
	docxParser  func([]byte) (string, error)
	xlsxParser  func([]byte) (string, error)
}

// NewClient creates an authenticated Google Drive client from config.
// Returns nil when Google Drive is not configured or credentials cannot be loaded.
// pdfParser, docxParser, xlsxParser are injected from app.PdfBytesToText etc. to
// avoid a circular import between this package and the app package.
func NewClient(cfg config.Config, pdfParser, docxParser, xlsxParser func([]byte) (string, error)) *Client {
	if !cfg.GoogleDriveEnabled() {
		return nil
	}
	credsJSON := []byte(cfg.GoogleDriveCredentialsJSON)
	if len(credsJSON) == 0 {
		var readErr error
		credsJSON, readErr = os.ReadFile(cfg.GoogleDriveCredentialsPath)
		if readErr != nil {
			log.Printf("[BOOT] GoogleDrive: failed to read credentials file %q: %v", cfg.GoogleDriveCredentialsPath, readErr)
			return nil
		}
	}
	limit := cfg.GoogleDriveSearchLimit
	if limit <= 0 {
		limit = 5
	}
	ctx := context.Background()
	creds, err := google.CredentialsFromJSON(ctx, credsJSON, drive.DriveReadonlyScope)
	if err != nil {
		log.Printf("[BOOT] GoogleDrive: parse credentials: %v", err)
		return nil
	}
	svc, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		log.Printf("[BOOT] GoogleDrive: create service: %v", err)
		return nil
	}
	log.Printf("[BOOT] GoogleDrive enabled folder_id=%q search_limit=%d", cfg.GoogleDriveFolderID, limit)
	return &Client{
		svc:         svc,
		folderID:    cfg.GoogleDriveFolderID,
		searchLimit: limit,
		pdfParser:   pdfParser,
		docxParser:  docxParser,
		xlsxParser:  xlsxParser,
	}
}

// SearchFiles queries Drive for files whose full-text content contains query.
func (c *Client) SearchFiles(query string) ([]*SearchResult, error) {
	q := fmt.Sprintf("fullText contains %q and trashed = false", query)
	if c.folderID != "" {
		q += fmt.Sprintf(" and %q in parents", c.folderID)
	}
	fl, err := c.svc.Files.List().
		Q(q).
		Fields("files(id, name, mimeType, webViewLink)").
		PageSize(int64(c.searchLimit)).
		Do()
	if err != nil {
		return nil, fmt.Errorf("googledrive: files.list: %w", err)
	}
	results := make([]*SearchResult, 0, len(fl.Files))
	for _, f := range fl.Files {
		results = append(results, &SearchResult{
			ID:       f.Id,
			Name:     f.Name,
			MimeType: f.MimeType,
			WebURL:   f.WebViewLink,
		})
	}
	return results, nil
}

// FetchContent downloads or exports the file and populates r.Content with plain text.
func (c *Client) FetchContent(r *SearchResult) error {
	var data []byte
	var err error

	switch r.MimeType {
	case "application/vnd.google-apps.document":
		data, err = c.export(r.ID, "text/plain")
	case "application/vnd.google-apps.spreadsheet":
		data, err = c.export(r.ID, "text/csv")
	case "application/vnd.google-apps.presentation":
		data, err = c.export(r.ID, "text/plain")
	default:
		data, err = c.download(r.ID)
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	nameLower := strings.ToLower(r.Name)
	switch {
	case strings.Contains(r.MimeType, "pdf") || strings.HasSuffix(nameLower, ".pdf"):
		if c.pdfParser != nil {
			if text, pErr := c.pdfParser(data); pErr == nil {
				r.Content = text
				return nil
			} else {
				log.Printf("[GDRIVE] pdfParser failed for %q: %v", r.Name, pErr)
			}
		}
		r.Content = string(data)
	case strings.Contains(r.MimeType, "wordprocessingml") || strings.HasSuffix(nameLower, ".docx"):
		if c.docxParser != nil {
			if text, dErr := c.docxParser(data); dErr == nil {
				r.Content = text
				return nil
			} else {
				log.Printf("[GDRIVE] docxParser failed for %q: %v", r.Name, dErr)
			}
		}
		r.Content = string(data)
	case strings.Contains(r.MimeType, "spreadsheetml") || strings.HasSuffix(nameLower, ".xlsx"):
		if c.xlsxParser != nil {
			if text, xErr := c.xlsxParser(data); xErr == nil {
				r.Content = text
				return nil
			} else {
				log.Printf("[GDRIVE] xlsxParser failed for %q: %v", r.Name, xErr)
			}
		}
		r.Content = string(data)
	default:
		r.Content = string(data)
	}
	return nil
}

func (c *Client) export(fileID, mimeType string) ([]byte, error) {
	resp, err := c.svc.Files.Export(fileID, mimeType).Download()
	if err != nil {
		return nil, fmt.Errorf("googledrive: export %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
	if err != nil {
		return nil, fmt.Errorf("googledrive: read export %s: %w", fileID, err)
	}
	return data, nil
}

func (c *Client) download(fileID string) ([]byte, error) {
	resp, err := c.svc.Files.Get(fileID).Download()
	if err != nil {
		return nil, fmt.Errorf("googledrive: download %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
	if err != nil {
		return nil, fmt.Errorf("googledrive: read download %s: %w", fileID, err)
	}
	return data, nil
}

// SearchAndFetch combines SearchFiles and FetchContent into a single call.
func (c *Client) SearchAndFetch(query string) ([]*SearchResult, error) {
	results, err := c.SearchFiles(query)
	if err != nil {
		return nil, err
	}
	for _, r := range results {
		if fErr := c.FetchContent(r); fErr != nil {
			log.Printf("[GDRIVE] FetchContent %q failed: %v", r.Name, fErr)
		}
	}
	return results, nil
}

// FormatContext formats search results into a Markdown block for LLM context injection.
// maxCharsPerDoc limits how many characters of document content are included; 0 = unlimited.
func FormatContext(results []*SearchResult, maxCharsPerDoc int) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	first := true
	for _, r := range results {
		if r.Content == "" {
			continue
		}
		if !first {
			sb.WriteString("\n---\n\n")
		}
		first = false
		sb.WriteString("## ")
		sb.WriteString(r.Name)
		sb.WriteString("\n")
		if r.WebURL != "" {
			sb.WriteString("URL: ")
			sb.WriteString(r.WebURL)
			sb.WriteString("\n")
		}
		text := r.Content
		if maxCharsPerDoc > 0 && len(text) > maxCharsPerDoc {
			text = text[:maxCharsPerDoc] + "...(truncado)"
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatSources returns a Slack mrkdwn line listing source files with clickable links.
func FormatSources(results []*SearchResult) string {
	var parts []string
	for _, r := range results {
		if r.WebURL == "" || r.Content == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = "Arquivo"
		}
		parts = append(parts, fmt.Sprintf("<%s|%s>", r.WebURL, name))
	}
	if len(parts) == 0 {
		return ""
	}
	return ":file_folder: _Fontes Google Drive: " + strings.Join(parts, " · ") + "_"
}
