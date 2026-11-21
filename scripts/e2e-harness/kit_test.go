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

// The pages' built-in fallback is the vendored kit itself: the bundled
// script is kit.js byte for byte, and the fallback stylesheet is kit.css in
// the default theme under its own header, both from the pinned render.
func TestTheFallbackIsTheVendoredKit(t *testing.T) {
	fix := filepath.Join("..", "..", "e2e", "fixtures", "ui-kit", "brass", "v1")
	static := filepath.Join("..", "..", "internal", "server", "web", "static")
	js, _ := os.ReadFile(filepath.Join(fix, "kit.js"))
	got, err := os.ReadFile(filepath.Join(static, "vendor", "muster-ui-kit-v1.js"))
	if err != nil || len(js) == 0 || string(got) != string(js) {
		t.Fatalf("static/vendor/muster-ui-kit-v1.js is not the pinned kit.js (%v): re-run scripts/vendor-ui-kit.sh", err)
	}
	css, _ := os.ReadFile(filepath.Join(fix, "kit.css"))
	fb, err := os.ReadFile(filepath.Join(static, "kit-fallback.css"))
	body := string(css)[strings.Index(string(css), "\n")+1:]
	if err != nil || !strings.HasSuffix(string(fb), body) || !strings.Contains(string(fb), pinnedKit) {
		t.Fatalf("static/kit-fallback.css is not the pinned kit.css under its header (%v): re-run scripts/vendor-ui-kit.sh", err)
	}
}
