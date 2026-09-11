package gitutil

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// FastImport drives one `git fast-import` process: a whole history rewrite goes
// in as a single stream, and the objects come out as one packfile.
//
// It exists because the index is the wrong tool for rebuilding many commits of a
// journal. Rebuilding a commit through a private index costs a `read-tree` of
// the WHOLE snapshot — 120ms against a 50k-entry index — plus an `update-index`
// and a `write-tree` that re-read and re-write it, all to change a handful of
// paths. fast-import keeps the tree in memory between commits and rewrites only
// the subtrees a path actually touches, which is the same work git does for an
// ordinary commit: ~0.6ms instead of ~113ms per rewritten commit, measured on a
// 51k-file snapshot.
//
// The frontend never reads from fast-import (no `ls`/`cat-blob`), so there is no
// request/response protocol to deadlock: the stream is written, closed, and the
// results are collected afterwards from the marks file.
type FastImport struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	w         *bufio.Writer
	stderr    bytes.Buffer
	marksPath string
	closed    bool
}

// NewFastImport starts fast-import against repoRoot, writing its mark table to
// marksPath (the caller's temp file, read back with Marks after Close).
//
// --date-format=raw takes git's own "<unixts> <tz>" idents verbatim, so an
// author line round-trips byte for byte; --force lets the stream point a ref
// somewhere that is not a fast-forward, which a redaction rewrite always is;
// --done makes a truncated stream an error instead of a silently short import.
func NewFastImport(ctx context.Context, repoRoot, marksPath string) (*FastImport, error) {
	cmd := exec.CommandContext(ctx, gitBin(), gcOff([]string{"fast-import",
		"--date-format=raw", "--force", "--done", "--quiet",
		"--export-marks=" + marksPath})...)
	cmd.Dir = repoRoot
	cmd.Env = append(scrubEnv(cmd.Environ()), "TWIP_SHIM_ACTIVE=1")
	f := &FastImport{cmd: cmd, marksPath: marksPath}
	cmd.Stderr = &f.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fast-import stdin: %w", err)
	}
	f.stdin, f.w = stdin, bufio.NewWriterSize(stdin, 1<<20)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start fast-import: %w", err)
	}
	return f, nil
}

// Commit is one commit to emit. Tree is the tree the commit starts from — the
// ORIGINAL commit's tree, set independently of Parent, which is what lets a
// rewrite re-parent a commit without rebuilding its content. Modify then applies
// on top of it.
type Commit struct {
	Ref     string // the ref the stream advances (a temp ref; the caller moves the real one under CAS)
	Mark    int    // 1-based; identifies this commit in the marks file
	Parent  string // "" for a root commit, ":<mark>" for an earlier commit in this stream, else a sha
	Tree    string // base tree for the commit
	Author  Ident
	Commits Ident // committer
	Message []byte
	Modify  []Modify
}

// Ident is a git identity line split into the parts fast-import wants. Date is
// git's raw "<unixts> <tz>" form.
type Ident struct{ Name, Email, Date string }

// Modify points one path at a blob. Inline carries the bytes when the blob has
// not been written yet (fast-import writes it into the same pack); otherwise SHA
// names an existing blob.
type Modify struct {
	Mode   string // "100644", "100755", "120000", …
	SHA    string // existing blob; ignored when Inline is non-nil
	Path   string
	Inline []byte
}

// WriteCommit emits one commit. Paths go out verbatim: fast-import's own format
// forbids a newline in a path and quotes anything starting with a double quote,
// so those two cases are rejected rather than silently mangled.
func (f *FastImport) WriteCommit(c Commit) error {
	for _, m := range c.Modify {
		if strings.ContainsAny(m.Path, "\n") || strings.HasPrefix(m.Path, `"`) {
			return fmt.Errorf("fast-import cannot express the path %q", m.Path)
		}
	}
	fmt.Fprintf(f.w, "commit %s\nmark :%d\n", c.Ref, c.Mark)
	fmt.Fprintf(f.w, "author %s <%s> %s\n", c.Author.Name, c.Author.Email, c.Author.Date)
	fmt.Fprintf(f.w, "committer %s <%s> %s\n", c.Commits.Name, c.Commits.Email, c.Commits.Date)
	fmt.Fprintf(f.w, "data %d\n", len(c.Message))
	f.w.Write(c.Message)
	f.w.WriteByte('\n')
	if c.Parent != "" {
		fmt.Fprintf(f.w, "from %s\n", c.Parent)
	}
	// An empty path names the root: this replaces the commit's whole tree with
	// Tree, independently of the parent set by `from`. It is what makes a
	// re-parent free — the tree is reused by reference, never rebuilt.
	fmt.Fprintf(f.w, "M 040000 %s \n", c.Tree)
	for _, m := range c.Modify {
		if m.Inline != nil {
			fmt.Fprintf(f.w, "M %s inline %s\ndata %d\n", m.Mode, m.Path, len(m.Inline))
			f.w.Write(m.Inline)
			f.w.WriteByte('\n')
			continue
		}
		fmt.Fprintf(f.w, "M %s %s %s\n", m.Mode, m.SHA, m.Path)
	}
	_, err := f.w.WriteString("\n")
	return err
}

// Close ends the stream and waits for fast-import to finish writing its pack and
// refs. It must be called before Marks.
func (f *FastImport) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if _, err := f.w.WriteString("done\n"); err != nil {
		_ = f.stdin.Close()
		_ = f.cmd.Wait()
		return fmt.Errorf("fast-import: %w", err)
	}
	if err := f.w.Flush(); err != nil {
		_ = f.stdin.Close()
		_ = f.cmd.Wait()
		return fmt.Errorf("fast-import: %w", err)
	}
	if err := f.stdin.Close(); err != nil {
		_ = f.cmd.Wait()
		return fmt.Errorf("fast-import: %w", err)
	}
	if err := f.cmd.Wait(); err != nil {
		return fmt.Errorf("fast-import: %w: %s", err, strings.TrimSpace(f.stderr.String()))
	}
	return nil
}

// Abort kills the import and discards it. A fast-import that never receives its
// `done` leaves the target refs untouched, so an aborted rewrite changes nothing
// a caller has to undo.
func (f *FastImport) Abort() {
	if f.closed {
		return
	}
	f.closed = true
	_ = f.stdin.Close()
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
	_ = f.cmd.Wait()
}

// Marks reads back the mark -> commit sha table the finished import wrote.
func (f *FastImport) Marks() (map[int]string, error) {
	b, err := os.ReadFile(f.marksPath) //nolint:gosec // our own temp file
	if err != nil {
		return nil, fmt.Errorf("read fast-import marks: %w", err)
	}
	marks := map[int]string{}
	for _, line := range strings.Split(string(b), "\n") {
		// ":<mark> SP <sha>"
		mark, sha, ok := strings.Cut(strings.TrimPrefix(line, ":"), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(mark)
		if err != nil {
			continue
		}
		marks[n] = sha
	}
	return marks, nil
}
