package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedKit is the muster commit the fixtures were rendered from
// (scripts/vendor-ui-kit.sh): the kit PR #946, merged.
const pinnedKit = "ff54356f066d198dacc74e6d8478f04943e4237c"

// The vendored kit is exactly muster's render at the pinned commit: every
// file matches SHA256SUMS, and a changed byte refuses to start the harness.
func TestTheVendoredKitIsPinned(t *testing.T) {
	dir := filepath.Join("..", "..", "e2e", "fixtures", "ui-kit")
	c, err := os.ReadFile(filepath.Join(dir, "COMMIT"))
	if err != nil || strings.TrimSpace(string(c)) != pinnedKit {
		t.Fatalf("COMMIT = %q, %v; want %s", c, err, pinnedKit)
	}
	k, err := loadKit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.files) != len(kitThemes)*len(kitFiles) {
		t.Fatalf("loaded %d files, want %d", len(k.files), len(kitThemes)*len(kitFiles))
	}
	if !strings.Contains(string(k.files["brass/kit.js"]), "window.MusterKit") || strings.Contains(string(k.files["brass/kit.js"]), "__MU_SPRITE__") {
		t.Fatal("brass/kit.js is not muster's served kit.js (the sprite filled in)")
	}
	if string(k.files["brass/kit.css"]) == string(k.files["harbor/kit.css"]) {
		t.Fatal("the two themes' kit.css are the same bytes: a theme test could not see the theme change")
	}

	// A changed byte is refused.
	tmp := t.TempDir()
	for _, f := range []string{"SHA256SUMS", "COMMIT"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		os.WriteFile(filepath.Join(tmp, f), b, 0o644)
	}
	for _, th := range kitThemes {
		os.MkdirAll(filepath.Join(tmp, th, "v1"), 0o755)
		for name := range kitFiles {
			b, _ := os.ReadFile(filepath.Join(dir, th, "v1", name))
			if th == "harbor" && name == "kit.css" {
				b = append(b, ' ')
			}
			os.WriteFile(filepath.Join(tmp, th, "v1", name), b, 0o644)
		}
	}
	if _, err := loadKit(tmp); err == nil || !strings.Contains(err.Error(), "harbor/v1/kit.css does not match SHA256SUMS") {
		t.Fatalf("a changed fixture: loadKit = %v, want a SHA256SUMS refusal", err)
	}
}
