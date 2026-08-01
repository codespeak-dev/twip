package gitutil

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestBatchReader(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
	} {
		if _, err := Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	// A handful of blobs, including one whose content spans the read boundary.
	contents := []string{"", "a", "hello\nworld\n", "{\"session_id\":\"abc\"}\n"}
	shas := make([]string, len(contents))
	for i, c := range contents {
		sha, err := HashObject(ctx, dir, []byte(c))
		if err != nil {
			t.Fatalf("hash-object %d: %v", i, err)
		}
		shas[i] = sha
	}

	br, err := NewBatchReader(ctx, dir)
	if err != nil {
		t.Fatalf("NewBatchReader: %v", err)
	}
	defer br.Close()

	for i, sha := range shas {
		got, found, err := br.Read(sha)
		if err != nil {
			t.Fatalf("Read(%s): %v", sha, err)
		}
		if !found {
			t.Fatalf("Read(%s): not found", sha)
		}
		if string(got) != contents[i] {
			t.Errorf("Read(%s) = %q, want %q", sha, got, contents[i])
		}
	}

	// A missing object reports found=false, not an error, and the reader stays
	// usable for the next request.
	if _, found, err := br.Read("0000000000000000000000000000000000000000"); err != nil || found {
		t.Errorf("missing object: found=%v err=%v, want found=false err=nil", found, err)
	}
	if got, found, err := br.Read(shas[1]); err != nil || !found || string(got) != contents[1] {
		t.Errorf("read after missing = %q found=%v err=%v", got, found, err)
	}
}

// TestBatchChecker: rev:path specs resolve to the oid+type of the object behind
// them without transferring content, an absent path reports found=false rather
// than erroring, and the SAME blob reached via two different commits reports one
// oid — the dedup redactionPlan relies on to read a carried-forward blob once.
func TestBatchChecker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
	} {
		if _, err := Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	blob, err := HashObject(ctx, dir, []byte("secret bytes\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Two commits sharing one blob at the same path, plus a subdir tree.
	mk := func(parent string) string {
		t.Helper()
		tree, err := MkTree(ctx, dir, []TreeEntry{{Mode: "100644", Type: "blob", SHA: blob, Name: "carried.txt"}})
		if err != nil {
			t.Fatal(err)
		}
		outer, err := MkTree(ctx, dir, []TreeEntry{
			{Mode: "100644", Type: "blob", SHA: blob, Name: "top.txt"},
			{Mode: "040000", Type: "tree", SHA: tree, Name: "sub"},
		})
		if err != nil {
			t.Fatal(err)
		}
		c, err := CommitTree(ctx, dir, outer, parent, "e\n")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := mk("")
	c2 := mk(c1)

	bc, err := NewBatchChecker(ctx, dir)
	if err != nil {
		t.Fatalf("NewBatchChecker: %v", err)
	}
	defer bc.Close()

	for _, spec := range []string{c1 + ":top.txt", c2 + ":top.txt", c2 + ":sub/carried.txt"} {
		oid, objType, found, err := bc.Check(spec)
		if err != nil || !found {
			t.Fatalf("Check(%s): found=%v err=%v", spec, found, err)
		}
		if oid != blob {
			t.Errorf("Check(%s) oid = %s, want the one shared blob %s", spec, oid, blob)
		}
		if objType != "blob" {
			t.Errorf("Check(%s) type = %q, want blob", spec, objType)
		}
	}

	// A tree is reported as such, so a caller can skip what has no bytes to patch.
	if _, objType, found, err := bc.Check(c2 + ":sub"); err != nil || !found || objType != "tree" {
		t.Errorf("Check(:sub) = type %q found=%v err=%v, want tree/true/nil", objType, found, err)
	}
	// An absent path is found=false, not an error, and the process stays usable.
	if _, _, found, err := bc.Check(c2 + ":nope.txt"); err != nil || found {
		t.Errorf("absent path: found=%v err=%v, want false/nil", found, err)
	}
	if oid, _, found, err := bc.Check(c1 + ":top.txt"); err != nil || !found || oid != blob {
		t.Errorf("check after absent = %s found=%v err=%v", oid, found, err)
	}
}

func TestIsWritesBlocked(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			// The real shape: gitutil.Run wraps a child git's stderr.
			"eperm hash-object",
			fmt.Errorf("git hash-object -w --stdin: %w: error: unable to create temporary file: Operation not permitted", errors.New("exit status 128")),
			true,
		},
		{
			"eacces",
			errors.New("git write-tree: exit status 128: error: insufficient permission for adding an object to repository database .git/objects\nfatal: Permission denied"),
			true,
		},
		{"unrelated git failure", errors.New("git commit: exit status 1: nothing to commit, working tree clean"), false},
		{"merge conflict", errors.New("git merge: exit status 1: CONFLICT (content): Merge conflict in foo.go"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWritesBlocked(tt.err); got != tt.want {
				t.Errorf("IsWritesBlocked() = %v, want %v", got, tt.want)
			}
		})
	}
}
