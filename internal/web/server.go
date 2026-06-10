// Package web serves the browsable timeline UI over the read model. It is
// server-rendered (html/template) with assets embedded via go:embed, so `twip
// serve` is a single self-contained binary with no JS build step.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/codespeak/twip/internal/readmodel"
)

//go:embed templates/*.html
var templatesFS embed.FS

type server struct {
	repoRoot string
	tmpl     *template.Template
}

// Serve starts the timeline UI on addr, blocking until the context is cancelled.
func Serve(ctx context.Context, repoRoot, addr string) error {
	funcs := template.FuncMap{
		"short": func(s string) string {
			if len(s) > 8 {
				return s[:8]
			}
			return s
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}
	s := &server{repoRoot: repoRoot, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/turn/", s.handleTurn)
	mux.HandleFunc("/api/timeline.json", s.handleTimelineJSON)

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Printf("twip timeline on http://localhost%s  (repo: %s)\n", addr, s.repoRoot)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	entries, err := readmodel.Timeline(r.Context(), s.repoRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "index.html", map[string]any{"Entries": entries})
}

func (s *server) handleTurn(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/turn/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.Error(w, "expected /turn/<session>/<seq>", http.StatusBadRequest)
		return
	}
	seq, err := strconv.Atoi(parts[1])
	if err != nil {
		http.Error(w, "seq must be a number", http.StatusBadRequest)
		return
	}
	detail, err := readmodel.Turn(r.Context(), s.repoRoot, parts[0], seq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "turn.html", map[string]any{"T": detail})
}

func (s *server) handleTimelineJSON(w http.ResponseWriter, r *http.Request) {
	entries, err := readmodel.Timeline(r.Context(), s.repoRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
