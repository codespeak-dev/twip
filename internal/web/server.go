// Package web serves the browsable timeline UI: a single-page app (vanilla JS +
// CSS, no build step) backed by JSON endpoints. Everything is embedded via
// go:embed, so `twip serve` stays one self-contained binary.
//
//	GET /                  the app shell (timeline + detail panel)
//	GET /event/<commit>    same shell, deep-linked to an event
//	GET /api/timeline.json the merged event timeline
//	GET /api/event/<commit> one event's full detail
//	GET /static/*          embedded css/js
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/readmodel"
)

//go:embed templates/index.html static/*
var assets embed.FS

type server struct {
	repoRoot string
	shell    []byte
}

// Serve starts the timeline UI on addr, blocking until the context is cancelled.
func Serve(ctx context.Context, repoRoot, addr string) error {
	shell, err := assets.ReadFile("templates/index.html")
	if err != nil {
		return fmt.Errorf("load shell: %w", err)
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}
	s := &server{repoRoot: repoRoot, shell: shell}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/api/timeline.json", s.handleTimelineJSON)
	mux.HandleFunc("/api/event/", s.handleEventJSON)
	mux.HandleFunc("/api/commit/", s.handleCommit)
	mux.HandleFunc("/api/blob", s.handleBlob)
	mux.HandleFunc("/api/filediff", s.handleFileDiff)
	mux.HandleFunc("/event/", s.handleApp)
	mux.HandleFunc("/", s.handleApp)

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

// handleApp serves the SPA shell for "/" and "/event/<commit>"; the JS reads the
// path and deep-links to the event.
func (s *server) handleApp(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/event/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.shell)
}

func (s *server) handleTimelineJSON(w http.ResponseWriter, r *http.Request) {
	entries, err := readmodel.Timeline(r.Context(), s.repoRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []readmodel.Entry{}
	}
	writeJSON(w, entries)
}

func (s *server) handleEventJSON(w http.ResponseWriter, r *http.Request) {
	commit := strings.TrimPrefix(r.URL.Path, "/api/event/")
	if commit == "" {
		http.Error(w, "expected /api/event/<commit>", http.StatusBadRequest)
		return
	}
	detail, err := readmodel.Event(r.Context(), s.repoRoot, commit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, detail)
}

// --- on-demand git queries (so the UI doesn't preload diffs/blobs) ---

var revRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

func validRev(s string) bool { return s == "HEAD" || revRe.MatchString(s) }

// handleCommit returns `git show` (stat + patch) for a real commit sha.
func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	sha := strings.TrimPrefix(r.URL.Path, "/api/commit/")
	if !validRev(sha) {
		http.Error(w, "bad sha", http.StatusBadRequest)
		return
	}
	out, err := gitutil.Out(r.Context(), s.repoRoot, "show", "--stat", "-p", "--no-color", sha)
	if err != nil {
		http.Error(w, "unknown commit", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"sha": sha, "text": out})
}

// handleBlob returns a file's content at a tree/commit (rev:path).
func (s *server) handleBlob(w http.ResponseWriter, r *http.Request) {
	rev, path := r.URL.Query().Get("rev"), r.URL.Query().Get("path")
	if !validRev(rev) || path == "" {
		http.Error(w, "bad rev/path", http.StatusBadRequest)
		return
	}
	b, err := gitutil.CatFile(r.Context(), s.repoRoot, rev+":"+path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"path": path, "text": string(b)})
}

// handleFileDiff returns the unified diff of one path between two trees.
func (s *server) handleFileDiff(w http.ResponseWriter, r *http.Request) {
	base, tree, path := r.URL.Query().Get("base"), r.URL.Query().Get("tree"), r.URL.Query().Get("path")
	if !validRev(base) || !validRev(tree) || path == "" {
		http.Error(w, "bad args", http.StatusBadRequest)
		return
	}
	out, err := gitutil.Out(r.Context(), s.repoRoot, "diff", "--no-color", base, tree, "--", path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"path": path, "diff": out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
