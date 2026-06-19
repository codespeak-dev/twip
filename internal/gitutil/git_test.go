package gitutil

import (
	"errors"
	"fmt"
	"testing"
)

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
