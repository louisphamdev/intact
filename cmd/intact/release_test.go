package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a file at the repository root. The tests of this file guard the
// files a public release needs, so they must read the real tree, not a fixture.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestLicenseIsMIT(t *testing.T) {
	l := repoFile(t, "LICENSE")
	for _, want := range []string{"MIT License", "Copyright (c) 2026 Louis Phạm",
		"Permission is hereby granted, free of charge"} {
		if !strings.Contains(l, want) {
			t.Errorf("LICENSE misses %q", want)
		}
	}
}

func TestReadmeNamesTheLicense(t *testing.T) {
	r := repoFile(t, "README.md")
	if !strings.Contains(r, "## License") {
		t.Error("README.md has no License section")
	}
	if !strings.Contains(r, "`MIT`") {
		t.Error("README.md does not name the SPDX identifier MIT")
	}
	if strings.Contains(r, "Everything the dashboard does is available at") {
		t.Error("README.md still says every dashboard route takes a dashboard key")
	}
}

func TestSecurityPolicyExists(t *testing.T) {
	s := repoFile(t, "SECURITY.md")
	if !strings.Contains(s, "advisor") && !strings.Contains(s, "Advisor") {
		t.Error("SECURITY.md names no private report channel")
	}
}

func TestGitignoreExcludesEnvFiles(t *testing.T) {
	g := repoFile(t, ".gitignore")
	for _, want := range []string{"*.env", "intact.env"} {
		if !strings.Contains(g, want) {
			t.Errorf(".gitignore misses %q", want)
		}
	}
}

// TestDocsDoNotNameThePrivateDeployment guards the disclosure cut of the review:
// the published docs must not name the owner's host, its hardware or his gateway.
func TestDocsDoNotNameThePrivateDeployment(t *testing.T) {
	banned := []string{"copilot", "9router", "Hermes", "E-2236"}
	for _, name := range []string{"docs/verify-phase1.md", "docs/verify-phase3.md",
		"docs/alerts.md", "docs/drift.md"} {
		body := repoFile(t, name)
		for _, b := range banned {
			if strings.Contains(body, b) {
				t.Errorf("%s still names %q", name, b)
			}
		}
	}
}

// TestGodocDoesNotClaimAPassword pins T7-6 (1): login is TOTP-only.
func TestGodocDoesNotClaimAPassword(t *testing.T) {
	for _, name := range []string{"internal/auth/totp.go", "internal/httpapi/server.go"} {
		for _, line := range strings.Split(repoFile(t, name), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") && strings.Contains(line, "password") {
				t.Errorf("%s: comment still names a password: %s", name, strings.TrimSpace(line))
			}
		}
	}
}

// TestFixturesLookLikePlaceholders keeps fake tokens out of a secret scanner's
// Google pattern (`ya29.`).
func TestFixturesLookLikePlaceholders(t *testing.T) {
	for _, name := range []string{"internal/httpapi/antigravity_test.go", "internal/httpapi/variants_test.go"} {
		if strings.Contains(repoFile(t, name), "ya29.") {
			t.Errorf("%s still holds a ya29. fixture", name)
		}
	}
}
