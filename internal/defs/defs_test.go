package defs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tgz builds an archive from name -> content. A name ending in "/" is a
// directory; content beginning "->" makes a symlink to the rest.
func tgz(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		h := &tar.Header{Name: name, Mode: 0o644}
		switch {
		case strings.HasSuffix(name, "/"):
			h.Typeflag = tar.TypeDir
		case strings.HasPrefix(content, "->"):
			h.Typeflag, h.Linkname = tar.TypeSymlink, strings.TrimPrefix(content, "->")
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(content))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(content))
		}
	}
	tw.Close()
	gz.Close()
	return bytes.NewReader(buf.Bytes())
}

const goodYAML = `id: basic
label: Basic
stages:
  - {id: start, label: Start, type: do, next: end}
  - {id: end, label: End, type: do, terminal: true}
`

func good() map[string]string {
	return map[string]string{"basic/pipeline.yml": goodYAML}
}

func current(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "current", "basic", "pipeline.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallsAndSwitchesCurrent(t *testing.T) {
	dir := t.TempDir()
	res, err := Install(dir, tgz(t, good()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pipelines) != 1 || res.Pipelines[0].ID != "basic" {
		t.Fatalf("result: %+v", res)
	}
	if current(t, dir) != goodYAML {
		t.Fatal("current does not hold the new definitions")
	}
	// A second, different release replaces it.
	second := strings.Replace(goodYAML, "label: Basic", "label: Basic v2", 1)
	if _, err := Install(dir, tgz(t, map[string]string{"basic/pipeline.yml": second})); err != nil {
		t.Fatal(err)
	}
	if current(t, dir) != second {
		t.Fatal("current was not switched to the second release")
	}
}

func TestBrokenDefinitionsAreRefusedAndLiveSetIsUntouched(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir, tgz(t, good())); err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(goodYAML, "next: end", "next: nowhere", 1)
	_, err := Install(dir, tgz(t, map[string]string{"basic/pipeline.yml": broken}))
	var pe *ProblemsError
	if !errors.As(err, &pe) {
		t.Fatalf("want ProblemsError, got %v", err)
	}
	if !strings.Contains(strings.Join(pe.Problems, "\n"), `"nowhere"`) {
		t.Fatalf("problems should name the mistake: %v", pe.Problems)
	}
	if current(t, dir) != goodYAML {
		t.Fatal("a refused upload changed what is live")
	}
	// No staging directories left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Fatalf("staging left behind: %s", e.Name())
		}
	}
}

func TestUnknownKeyIsAProblemNotACrash(t *testing.T) {
	dir := t.TempDir()
	typo := strings.Replace(goodYAML, "label: Start", "labell: Start", 1)
	_, err := Install(dir, tgz(t, map[string]string{"basic/pipeline.yml": typo}))
	var pe *ProblemsError
	if !errors.As(err, &pe) || !strings.Contains(strings.Join(pe.Problems, " "), "labell") {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyAndDuplicateSets(t *testing.T) {
	dir := t.TempDir()
	_, err := Install(dir, tgz(t, map[string]string{"README.md": "hi"}))
	var pe *ProblemsError
	if !errors.As(err, &pe) || !strings.Contains(pe.Problems[0], "no pipelines") {
		t.Fatalf("empty set: %v", err)
	}
	_, err = Install(dir, tgz(t, map[string]string{"a/pipeline.yml": goodYAML, "b/pipeline.yml": goodYAML}))
	if !errors.As(err, &pe) || !strings.Contains(strings.Join(pe.Problems, " "), "used by both") {
		t.Fatalf("duplicate ids: %v", err)
	}
}

func TestUnsafeArchivesAreRefused(t *testing.T) {
	cases := map[string]map[string]string{
		"parent path":    {"../escape.yml": "x"},
		"absolute path":  {"/etc/passwd": "x"},
		"symlink":        {"basic/pipeline.yml": goodYAML, "basic/link": "->/etc/passwd"},
		"hidden entry":   {"basic/pipeline.yml": goodYAML, ".git/config": "x"},
		"backslash path": {`basic\..\x`: "x"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Install(dir, tgz(t, files))
			var ae *ArchiveError
			if !errors.As(err, &ae) {
				t.Fatalf("want ArchiveError, got %v", err)
			}
			if exists(filepath.Join(dir, "current")) {
				t.Fatal("something was installed")
			}
			if exists(filepath.Join(filepath.Dir(dir), "escape.yml")) {
				t.Fatal("a file escaped the destination")
			}
		})
	}
}

func TestNotAGzipArchive(t *testing.T) {
	_, err := Install(t.TempDir(), strings.NewReader("this is not an archive"))
	var ae *ArchiveError
	if !errors.As(err, &ae) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedFileIsRefused(t *testing.T) {
	big := strings.Repeat("x", maxFileBytes+10)
	_, err := Install(t.TempDir(), tgz(t, map[string]string{"basic/pipeline.yml": big}))
	var ae *ArchiveError
	if !errors.As(err, &ae) {
		t.Fatalf("got %v", err)
	}
}

func TestPruneKeepsNewestAndCurrent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < keepReleases+3; i++ {
		if _, err := Install(dir, tgz(t, good())); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // release names are second-resolution
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "releases"))
	if len(entries) > keepReleases {
		t.Fatalf("kept %d releases, want at most %d", len(entries), keepReleases)
	}
	if current(t, dir) != goodYAML {
		t.Fatal("current was pruned")
	}
}

func TestEmptyCurrentDirectoryIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, tgz(t, good())); err != nil {
		t.Fatal(err)
	}
	if current(t, dir) != goodYAML {
		t.Fatal("current does not hold the new definitions")
	}
}

func TestNonEmptyCurrentDirectoryIsKeptBesideNotLost(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "current", "basic")
	if err := os.MkdirAll(cur, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cur, "pipeline.yml"), []byte("hand placed"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir, tgz(t, good()))
	if err != nil {
		t.Fatal(err)
	}
	if current(t, dir) != goodYAML {
		t.Fatal("current does not hold the new definitions")
	}
	kept, err := os.ReadFile(filepath.Join(dir, "current.before-sync-"+res.Release, "basic", "pipeline.yml"))
	if err != nil || string(kept) != "hand placed" {
		t.Fatalf("what was there was not kept: %q, %v", kept, err)
	}
}
