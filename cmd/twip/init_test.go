package main

import (
	"strings"
	"testing"
)

func TestHookManagerGuidance(t *testing.T) {
	// Unknown manager -> empty, so reportForeignHook falls back to the generic snippet.
	if g := hookManagerGuidance("", "origin", false); g != "" {
		t.Errorf("unknown manager should yield empty guidance, got:\n%s", g)
	}

	cases := []struct {
		manager string
		want    []string
	}{
		{"lefthook", []string{"lefthook.yml", "twip-sync", "twip sync push origin"}},
		{"husky", []string{".husky/pre-push", "twip sync push origin"}},
		{"pre-commit", []string{".pre-commit-config.yaml", "twip sync push origin", "stages: [pre-push]"}},
	}
	for _, c := range cases {
		g := hookManagerGuidance(c.manager, "origin", false)
		for _, w := range c.want {
			if !strings.Contains(g, w) {
				t.Errorf("%s guidance missing %q:\n%s", c.manager, w, g)
			}
		}
		if strings.Contains(g, "check pre-push") {
			t.Errorf("%s non-enforce guidance should not wire the gate:\n%s", c.manager, g)
		}
		if ge := hookManagerGuidance(c.manager, "origin", true); !strings.Contains(ge, "check pre-push") {
			t.Errorf("%s enforce guidance must wire the gate:\n%s", c.manager, ge)
		}
	}

	// An empty remote defaults to origin.
	if g := hookManagerGuidance("lefthook", "", false); !strings.Contains(g, "twip sync push origin") {
		t.Errorf("empty remote should default to origin:\n%s", g)
	}
}
