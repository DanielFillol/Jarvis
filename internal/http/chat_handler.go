package http

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/app"
)

// ChatHandler handles POST /api/chat requests.  It accepts either
// application/json or multipart/form-data, calls ProcessDirect synchronously,
// and writes the answer as JSON.
type ChatHandler struct {
	Service *app.Service
	APIKey  string
}

// NewChatHandler constructs a new ChatHandler.
func NewChatHandler(svc *app.Service, apiKey string) *ChatHandler {
	return &ChatHandler{Service: svc, APIKey: apiKey}
}

type chatRequest struct {
	Message  string `json:"message"`
	UserID   string `json:"user_id"`
	ThreadID string `json:"thread_id"`
	History  string `json:"history"`
}

type chatResponse struct {
	Answer   string `json:"answer"`
	ThreadID string `json:"thread_id"`
}

type chatError struct {
	Error string `json:"error"`
}

// ServeHTTP implements http.Handler.
func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Printf("[CHAT] %s %s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, chatError{"method not allowed"})
		return
	}

	// Optional authentication.
	if h.APIKey != "" {
		auth := r.Header.Get("Authorization")
		expected := "Bearer " + h.APIKey
		if auth != expected {
			writeJSON(w, http.StatusUnauthorized, chatError{"unauthorized"})
			return
		}
	}

	var req chatRequest
	var files []app.DirectFile

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, chatError{"failed to read body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, chatError{"invalid JSON"})
			return
		}

	case strings.HasPrefix(ct, "multipart/form-data"):
		const maxMemory = 32 * 1024 * 1024 // 32 MB in memory
		if err := r.ParseMultipartForm(maxMemory); err != nil {
			writeJSON(w, http.StatusBadRequest, chatError{"failed to parse multipart form"})
			return
		}
		req.Message = r.FormValue("message")
		req.UserID = r.FormValue("user_id")
		req.ThreadID = r.FormValue("thread_id")
		req.History = r.FormValue("history")

		if r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					f, err := fh.Open()
					if err != nil {
						log.Printf("[CHAT][WARN] failed to open uploaded file %q: %v", fh.Filename, err)
						continue
					}
					data, err := io.ReadAll(io.LimitReader(f, 20*1024*1024))
					f.Close()
					if err != nil {
						log.Printf("[CHAT][WARN] failed to read uploaded file %q: %v", fh.Filename, err)
						continue
					}
					mime := fh.Header.Get("Content-Type")
					if mime == "" {
						mime = "application/octet-stream"
					}
					files = append(files, app.DirectFile{
						Name:     fh.Filename,
						Mimetype: mime,
						Data:     data,
					})
				}
			}
		}

	default:
		writeJSON(w, http.StatusUnsupportedMediaType, chatError{"unsupported content type; use application/json or multipart/form-data"})
		return
	}

	if strings.TrimSpace(req.Message) == "" && len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, chatError{"message is required"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		req.Message = "Analise o(s) arquivo(s) anexado(s) e me dê um resumo."
	}
	if strings.TrimSpace(req.UserID) == "" {
		req.UserID = "api-user"
	}
	if strings.TrimSpace(req.ThreadID) == "" {
		req.ThreadID = fmt.Sprintf("direct-%d", time.Now().UnixNano())
	}

	answer, err := h.Service.ProcessDirect(req.Message, req.UserID, req.ThreadID, req.History, files)
	if err != nil {
		log.Printf("[CHAT][ERR] ProcessDirect: %v dur=%s", err, time.Since(start))
		writeJSON(w, http.StatusInternalServerError, chatError{"internal error"})
		return
	}

	log.Printf("[CHAT] done dur=%s answer_len=%d", time.Since(start), len(answer))
	writeJSON(w, http.StatusOK, chatResponse{Answer: answer, ThreadID: req.ThreadID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
