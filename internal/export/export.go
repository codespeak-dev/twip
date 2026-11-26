package export

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
)

// Options configures an export run.
type Options struct {
	Repos              []string
	Out                string
	SkipPromptPrefixes []string
	Progress           func(clone string, done, total int)
}

// Reasons a session is listed in the index but left out of the archives.
const (
	SkippedEmpty        = "empty"         // no transcript line was ever recorded
	SkippedPromptPrefix = "prompt-prefix" // its first prompt starts with a skipped prefix
)

// Author is one git identity that stamped commits on a clone's journal.
type Author struct {
	Email   string `json:"email"`
	Name    string `json:"name"`
	Commits int    `json:"commits"`
}

// Clone summarizes one journal.
type Clone struct {
	ID       string   `json:"id"`
	RepoURL  string   `json:"repoUrl"`
	Ref      string   `json:"ref"`
	Commits  int      `json:"commits"`
	Authors  []Author `json:"authors"`
	Sessions int      `json:"sessions"`
}

// FileReport describes one rebuilt transcript file.
type FileReport struct {
	Path      string         `json:"path"`
	Lines     int            `json:"lines"`
	Bytes     int64          `json:"bytes"`
	Faithful  bool           `json:"faithful"`
	Issues    []Issue        `json:"issues,omitempty"`
	Qualities map[string]int `json:"qualities,omitempty"`
	Redacted  int            `json:"redactedLines,omitempty"`
}

// SessionReport describes one session and the files rebuilt for it.
type SessionReport struct {
	Clone       string       `json:"clone"`
	SessionID   string       `json:"sessionId"`
	Agent       string       `json:"agent"`
	ProjectPath string       `json:"projectPath,omitempty"`
	FirstTS     string       `json:"firstTs"`
	LastTS      string       `json:"lastTs"`
	Events      int          `json:"events"`
	Archive     string       `json:"archive,omitempty"`
	Skipped     string       `json:"skipped,omitempty"`
	Files       []FileReport `json:"files"`
}

// Index is written next to the archives and lists everything exported.
type Index struct {
	Clones   []Clone         `json:"clones"`
	Archives []ArchiveReport `json:"archives"`
	Sessions []SessionReport `json:"sessions"`
}

// ArchiveReport names one archive and what it holds.
type ArchiveReport struct {
	Name        string `json:"name"`
	Clone       string `json:"clone"`
	RepoURL     string `json:"repoUrl"`
	ProjectPath string `json:"projectPath,omitempty"`
	Incomplete  bool   `json:"incomplete"`
	Sessions    int    `json:"sessions"`
	Bytes       int64  `json:"bytes"`
	FirstTS     string `json:"firstTs"`
	LastTS      string `json:"lastTs"`
}

type session struct {
	clone, id, agent string
	firstTS, lastTS  string
	firstPrompt      string
	events           int
	main             *Transcript
	sidechains       map[string]*Transcript
	order            []string
}

const unit = "\x1f"

// Run rebuilds every session in every journal of opts.Repos and writes one zip
// archive per clone and project directory under opts.Out, plus index.json.
func Run(ctx context.Context, opts Options) (*Index, error) {
	work := filepath.Join(opts.Out, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.Out, "archives"), 0o700); err != nil {
		return nil, err
	}
	index := &Index{}
	seen := map[string]string{}
	for _, repo := range opts.Repos {
		url, err := gitutil.Out(ctx, repo, "remote", "get-url", "origin")
		if err != nil {
			return nil, err
		}
		refs, err := store.New(repo).JournalRefs(ctx)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			id := path.Base(ref)
			if other, dup := seen[id]; dup {
				return nil, fmt.Errorf("clone %s has journals in both %s and %s", id, other, repo)
			}
			seen[id] = repo
			clone, sessions, err := walk(ctx, opts, repo, ref, work)
			if err != nil {
				return nil, err
			}
			clone.RepoURL = strings.TrimSpace(url)
			reports, archives, err := pack(opts.Out, clone, sessions, opts.SkipPromptPrefixes)
			if err != nil {
				return nil, err
			}
			index.Clones = append(index.Clones, clone)
			index.Sessions = append(index.Sessions, reports...)
			index.Archives = append(index.Archives, archives...)
		}
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.Out, "index.json"), data, 0o600); err != nil {
		return nil, err
	}
	return index, os.RemoveAll(work)
}

func walk(ctx context.Context, opts Options, repo, ref, work string) (Clone, []*session, error) {
	clone := Clone{ID: path.Base(ref), Ref: ref}
	out, err := gitutil.Run(ctx, repo, nil, nil,
		"log", "--reverse", "--format=%H"+unit+"%ae"+unit+"%an", ref)
	if err != nil {
		return clone, nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	br, err := gitutil.NewBatchReader(ctx, repo)
	if err != nil {
		return clone, nil, err
	}
	defer br.Close()

	dir := filepath.Join(work, clone.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return clone, nil, err
	}
	authors := map[string]*Author{}
	byID := map[string]*session{}
	var sessions []*session
	for i, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, unit, 3)
		if len(parts) != 3 {
			return clone, nil, fmt.Errorf("unexpected log line %q", line)
		}
		commit := parts[0]
		clone.Commits++
		key := strings.ToLower(parts[1])
		if authors[key] == nil {
			authors[key] = &Author{Email: key, Name: parts[2]}
		}
		authors[key].Commits++
		if opts.Progress != nil && i%5000 == 0 {
			opts.Progress(clone.ID, i, len(lines))
		}

		raw, found, err := br.Read(commit + ":meta/event.json")
		if err != nil {
			return clone, nil, err
		}
		if !found {
			continue
		}
		var rec store.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return clone, nil, fmt.Errorf("%s: event.json: %w", commit, err)
		}
		if rec.SessionID == "" {
			continue
		}
		s := byID[rec.SessionID]
		if s == nil {
			if !safeName.MatchString(rec.SessionID) {
				return clone, nil, fmt.Errorf("%s: unsafe session id %q", commit, rec.SessionID)
			}
			s = &session{clone: clone.ID, id: rec.SessionID, firstTS: rec.TS, sidechains: map[string]*Transcript{}}
			s.main = NewTranscript(filepath.Join(dir, rec.SessionID+".jsonl"))
			byID[rec.SessionID] = s
			sessions = append(sessions, s)
		}
		if s.agent == "" {
			s.agent = rec.Agent
		}
		if s.firstPrompt == "" {
			s.firstPrompt = rec.Prompt
		}
		s.lastTS = rec.TS
		s.events++
		if rec.Transcript != nil {
			data, _, err := br.Read(commit + ":meta/transcript.jsonl")
			if err != nil {
				return clone, nil, err
			}
			if err := s.main.Apply(rec.Transcript.From, rec.Transcript.To, data, rec.Transcript.Quality); err != nil {
				return clone, nil, err
			}
		}
		for _, side := range rec.Sidechains {
			if !safeName.MatchString(side.ID) {
				return clone, nil, fmt.Errorf("%s: unsafe sidechain id %q", commit, side.ID)
			}
			t := s.sidechains[side.ID]
			if t == nil {
				t = NewTranscript(filepath.Join(dir, rec.SessionID+".agent-"+side.ID+".jsonl"))
				s.sidechains[side.ID] = t
				s.order = append(s.order, side.ID)
			}
			data, _, err := br.Read(commit + ":meta/sidechains/agent-" + side.ID + ".jsonl")
			if err != nil {
				return clone, nil, err
			}
			if err := t.Apply(side.From, side.To, data, side.Quality); err != nil {
				return clone, nil, err
			}
		}
	}
	for _, s := range sessions {
		if err := s.main.Finish(); err != nil {
			return clone, nil, err
		}
		for _, id := range s.order {
			if err := s.sidechains[id].Finish(); err != nil {
				return clone, nil, err
			}
		}
	}
	for _, a := range authors {
		clone.Authors = append(clone.Authors, *a)
	}
	sort.Slice(clone.Authors, func(i, j int) bool { return clone.Authors[i].Commits > clone.Authors[j].Commits })
	clone.Sessions = len(sessions)
	return clone, sessions, nil
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type firstLine struct {
	Type      string `json:"type"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	} `json:"payload"`
}

// scanJSONL calls fn for each JSON line of the file until fn returns true.
func scanJSONL(p string, fn func(firstLine) bool) error {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var l firstLine
			if json.Unmarshal(line, &l) == nil && fn(l) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func claudeCwd(p string) (string, error) {
	var cwd string
	err := scanJSONL(p, func(l firstLine) bool {
		cwd = l.Cwd
		return cwd != ""
	})
	return cwd, err
}

func codexMeta(p string) (id, cwd string, ts time.Time, err error) {
	err = scanJSONL(p, func(l firstLine) bool {
		if l.Type != "session_meta" {
			return false
		}
		id, cwd = l.Payload.ID, l.Payload.Cwd
		stamp := l.Payload.Timestamp
		if stamp == "" {
			stamp = l.Timestamp
		}
		ts, _ = time.Parse(time.RFC3339Nano, stamp)
		return true
	})
	return id, cwd, ts, err
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// ClaudeProjectDir is the directory name Claude Code keeps a project's sessions under.
func ClaudeProjectDir(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

// CodexRolloutPath is where Codex keeps a rollout, relative to the home directory.
func CodexRolloutPath(id string, ts time.Time) string {
	ts = ts.UTC()
	return path.Join(".codex/sessions", ts.Format("2006"), ts.Format("01"), ts.Format("02"),
		"rollout-"+ts.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
}

type packed struct {
	src, name string
	t         *Transcript
}

func layout(s *session) (project string, files []packed, err error) {
	switch s.agent {
	case "codex":
		id, cwd, ts, err := codexMeta(s.main.Path)
		if err != nil {
			return "", nil, err
		}
		if id == "" {
			id = s.id
		}
		project = cwd
		files = append(files, packed{s.main.Path, CodexRolloutPath(id, ts), s.main})
		for _, sid := range s.order {
			t := s.sidechains[sid]
			subID, _, subTS, err := codexMeta(t.Path)
			if err != nil {
				return "", nil, err
			}
			if subID == "" {
				subID = sid
			}
			if subTS.IsZero() {
				subTS = ts
			}
			files = append(files, packed{t.Path, CodexRolloutPath(subID, subTS), t})
		}
	default:
		project, err = claudeCwd(s.main.Path)
		if err != nil {
			return "", nil, err
		}
		for _, sid := range s.order {
			if project != "" {
				break
			}
			if project, err = claudeCwd(s.sidechains[sid].Path); err != nil {
				return "", nil, err
			}
		}
		dir := path.Join(".claude/projects", ClaudeProjectDir(project))
		files = append(files, packed{s.main.Path, path.Join(dir, s.id+".jsonl"), s.main})
		for _, sid := range s.order {
			files = append(files, packed{s.sidechains[sid].Path,
				path.Join(dir, s.id, "subagents", "agent-"+sid+".jsonl"), s.sidechains[sid]})
		}
	}
	return project, files, nil
}

// Archive layout: transcripts live under sessions/ at the path the agent keeps
// them relative to the home directory. A session with any transcript that did
// not rebuild faithfully goes to its own archive marked incomplete, so whoever
// imports it can tell it apart from the agent's own files.
const sessionsDir = "sessions"

func faithful(files []packed) bool {
	for _, f := range files {
		if f.t.Size() > 0 && !f.t.Faithful() {
			return false
		}
	}
	return true
}

func skipReason(s *session, prefixes []string) string {
	if s.main.Size() == 0 && len(s.order) == 0 {
		return SkippedEmpty
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(strings.TrimLeft(s.firstPrompt, " \t\r\n"), prefix) {
			return SkippedPromptPrefix
		}
	}
	return ""
}

func pack(out string, clone Clone, sessions []*session, skip []string) ([]SessionReport, []ArchiveReport, error) {
	type group struct {
		project    string
		incomplete bool
		sessions   []*session
		files      map[*session][]packed
	}
	groups := map[string]*group{}
	var order []string
	var reports []SessionReport
	for _, s := range sessions {
		project, files, err := layout(s)
		if err != nil {
			return nil, nil, err
		}
		if reason := skipReason(s, skip); reason != "" {
			reports = append(reports, SessionReport{
				Clone: s.clone, SessionID: s.id, Agent: s.agent, ProjectPath: project,
				FirstTS: s.firstTS, LastTS: s.lastTS, Events: s.events, Skipped: reason,
			})
			continue
		}
		incomplete := !faithful(files)
		key := fmt.Sprintf("%t\x00%s", incomplete, project)
		g := groups[key]
		if g == nil {
			g = &group{project: project, incomplete: incomplete, files: map[*session][]packed{}}
			groups[key] = g
			order = append(order, key)
		}
		g.sessions = append(g.sessions, s)
		g.files[s] = files
	}
	var archives []ArchiveReport
	for _, key := range order {
		g := groups[key]
		sum := sha256.Sum256([]byte(g.project))
		name := clone.ID + "-" + hex.EncodeToString(sum[:])[:12]
		if g.incomplete {
			name += "-incomplete"
		}
		name += ".zip"
		var sessionReports []SessionReport
		for _, s := range g.sessions {
			r := SessionReport{
				Clone: s.clone, SessionID: s.id, Agent: s.agent, ProjectPath: g.project,
				FirstTS: s.firstTS, LastTS: s.lastTS, Events: s.events, Archive: name,
			}
			for _, f := range g.files[s] {
				if f.t.Size() == 0 {
					continue
				}
				r.Files = append(r.Files, FileReport{
					Path: f.name, Lines: f.t.Lines(), Bytes: f.t.Size(), Faithful: f.t.Faithful(),
					Issues: f.t.Issues, Qualities: f.t.Qualities, Redacted: f.t.Redacted,
				})
			}
			sessionReports = append(sessionReports, r)
		}
		manifest := Manifest{Version: 1, ProjectPath: g.project, Incomplete: g.incomplete, Clone: clone, Sessions: sessionReports}
		size, err := writeArchive(filepath.Join(out, "archives", name), manifest, g.sessions, g.files)
		if err != nil {
			return nil, nil, err
		}
		archive := ArchiveReport{
			Name: name, Clone: clone.ID, RepoURL: clone.RepoURL, ProjectPath: g.project, Incomplete: g.incomplete,
			Sessions: len(g.sessions), Bytes: size,
		}
		for _, s := range g.sessions {
			if archive.FirstTS == "" || s.firstTS < archive.FirstTS {
				archive.FirstTS = s.firstTS
			}
			if s.lastTS > archive.LastTS {
				archive.LastTS = s.lastTS
			}
		}
		archives = append(archives, archive)
		reports = append(reports, sessionReports...)
	}
	return reports, archives, nil
}

// Manifest is manifest.json inside each archive.
type Manifest struct {
	Version     int             `json:"version"`
	ProjectPath string          `json:"projectPath,omitempty"`
	Incomplete  bool            `json:"incomplete"`
	Clone       Clone           `json:"clone"`
	Sessions    []SessionReport `json:"sessions"`
}

func writeArchive(dest string, manifest Manifest, sessions []*session, files map[*session][]packed) (int64, error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, err
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		return 0, err
	}
	if _, err := w.Write(data); err != nil {
		return 0, err
	}
	for _, s := range sessions {
		for _, p := range files[s] {
			if p.t.Size() == 0 {
				continue
			}
			if err := addFile(zw, path.Join(sessionsDir, p.name), p.src); err != nil {
				return 0, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), f.Close()
}

func addFile(zw *zip.Writer, name, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}
