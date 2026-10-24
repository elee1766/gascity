package containerhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHostTool puts an executable named name in dir.
func writeHostTool(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The emulated images are Linux images built from host tools, and a Linux
// image has GNU timeout. macOS has none, but Homebrew's coreutils installs it
// as gtimeout: BuildImage links that in under the GNU name.
func TestBuildImageLinksTheGNUPrefixedToolWhenTheHostLacksIt(t *testing.T) {
	path := t.TempDir()
	gtimeout := writeHostTool(t, path, "gtimeout")
	t.Setenv("PATH", path)
	h, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := h.BuildImage("img", ImageSpec{Tools: []string{"timeout"}}, "/self"); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	img, ok := h.LookupImage("img")
	if !ok {
		t.Fatal("image not stored")
	}
	if got, err := os.Readlink(filepath.Join(img.Root, "bin", "timeout")); err != nil || got != gtimeout {
		t.Fatalf("image bin/timeout -> %q (%v), want %q", got, err, gtimeout)
	}
}

// The host's own tool wins over its g-prefixed copy.
func TestBuildImagePrefersTheHostToolOverTheGNUPrefixedOne(t *testing.T) {
	path := t.TempDir()
	timeout := writeHostTool(t, path, "timeout")
	writeHostTool(t, path, "gtimeout")
	t.Setenv("PATH", path)
	h, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := h.BuildImage("img", ImageSpec{Tools: []string{"timeout"}}, "/self"); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	img, _ := h.LookupImage("img")
	if got, err := os.Readlink(filepath.Join(img.Root, "bin", "timeout")); err != nil || got != timeout {
		t.Fatalf("image bin/timeout -> %q (%v), want %q", got, err, timeout)
	}
}

// A tool the host has under neither name fails the build and names the tool.
func TestBuildImageFailsForAToolTheHostLacks(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = h.BuildImage("img", ImageSpec{Tools: []string{"timeout"}}, "/self")
	if err == nil || !strings.Contains(err.Error(), `host tool "timeout"`) {
		t.Fatalf("BuildImage err = %v, want it to name the missing host tool", err)
	}
}
