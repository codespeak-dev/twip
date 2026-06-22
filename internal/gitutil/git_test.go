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
