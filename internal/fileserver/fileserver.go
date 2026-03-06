package fileserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"path"
	"sync"
	"time"
)

type entry struct {
	name    string
	content []byte
	expires time.Time
}

// FileServer is an in-memory temporary file store with an HTTP handler.
// Files are served at /files/{id} and expire after their configured TTL.
type FileServer struct {
	mu    sync.RWMutex
	files map[string]entry
}

// New creates a FileServer and starts the background cleanup goroutine.
func New() *FileServer {
	fs := &FileServer{files: make(map[string]entry)}
	go fs.cleanup()
	return fs
}

// Store saves content under a random ID and returns that ID.
func (fs *FileServer) Store(name string, content []byte, ttl time.Duration) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	fs.mu.Lock()
	fs.files[id] = entry{name: name, content: content, expires: time.Now().Add(ttl)}
	fs.mu.Unlock()
	return id
}

// ServeHTTP handles GET /files/{id}.
func (fs *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	if id == "" || id == "." || id == "/" {
		http.NotFound(w, r)
		return
	}
	fs.mu.RLock()
	e, ok := fs.files[id]
	fs.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, e.name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.content)
}

func (fs *FileServer) cleanup() {
	ticker := time.NewTicker(15 * time.Minute)
	for range ticker.C {
		now := time.Now()
		fs.mu.Lock()
		for id, e := range fs.files {
			if now.After(e.expires) {
				delete(fs.files, id)
				log.Printf("[FILESERVER] expired %s", id)
			}
		}
		fs.mu.Unlock()
	}
}
