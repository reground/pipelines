// Package defs receives pipeline definitions as a tarball, checks them, and
// installs them as a new release that the app then reads. The point is
// that a broken set is refused here, before anything the app is serving
// changes, no matter who sent it.
//
// Layout under the data directory:
//
//	releases/<name>/<pipeline>/pipeline.yml ...   one directory per sync
//	current -> releases/<name>                    what the app reads
//
// The switch is a single atomic rename of the symlink.
package defs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.reground.org/will/pipelines/internal/pipeline"
)

const (
	maxFiles     = 2000
	maxFileBytes = 2 << 20  // 2 MiB per file
	maxTotal     = 10 << 20 // 10 MiB unpacked
	keepReleases = 5
)

// ProblemsError means the archive was readable but the definitions in it
// are not valid. Nothing was installed.
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string {
	return fmt.Sprintf("the definitions have %d problem(s)", len(e.Problems))
}

// ArchiveError means the upload could not be read as a definitions archive.
type ArchiveError struct{ Err error }

func (e *ArchiveError) Error() string { return e.Err.Error() }
func (e *ArchiveError) Unwrap() error { return e.Err }

// Result describes an installed release.
type Result struct {
	Release   string
	Pipelines []*pipeline.Pipeline
}

var installMu sync.Mutex

// Install unpacks, checks and installs a definitions archive (a gzipped
// tar with one folder per pipeline, each containing pipeline.yml).
func Install(dataDir string, archive io.Reader) (*Result, error) {
	installMu.Lock()
	defer installMu.Unlock()

	if err := os.MkdirAll(filepath.Join(dataDir, "releases"), 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(dataDir, ".incoming-*")
	if err != nil {
		return nil, err
	}
	installed := false
	defer func() {
		if !installed {
			os.RemoveAll(staging)
		}
	}()

	if err := extract(archive, staging); err != nil {
		return nil, &ArchiveError{err}
	}
	pls, problems := validate(staging)
	if len(problems) > 0 {
		return nil, &ProblemsError{problems}
	}

	// Rename needs the staging dir and releases/ on one filesystem, which
	// they are: both live under dataDir.
	name := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(dataDir, "releases", name)
	for i := 2; exists(dest); i++ {
		dest = filepath.Join(dataDir, "releases", fmt.Sprintf("%s-%d", name, i))
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(staging, dest); err != nil {
		return nil, err
	}
	installed = true

	tmpLink := filepath.Join(dataDir, ".current-new")
	_ = os.Remove(tmpLink)
	if err := os.Symlink(filepath.Join("releases", filepath.Base(dest)), tmpLink); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpLink, filepath.Join(dataDir, "current")); err != nil {
		return nil, err
	}
	prune(dataDir)
	return &Result{Release: filepath.Base(dest), Pipelines: pls}, nil
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// validate loads and checks every pipeline directory under root.
func validate(root string) ([]*pipeline.Pipeline, []string) {
	loaded, errs, err := pipeline.LoadDirs(root)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var problems []string
	var dirs []string
	for d := range loaded {
		dirs = append(dirs, d)
	}
	for d := range errs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var out []*pipeline.Pipeline
	ids := map[string]string{}
	for _, d := range dirs {
		if e, bad := errs[d]; bad {
			problems = append(problems, e.Error())
			continue
		}
		p := loaded[d]
		pr, _ := p.Check()
		for _, x := range pr {
			problems = append(problems, p.ID+": "+x)
		}
		if other, dup := ids[p.ID]; dup {
			problems = append(problems, fmt.Sprintf("the pipeline id %q is used by both %s and %s", p.ID, other, d))
		}
		ids[p.ID] = d
		out = append(out, p)
	}
	if len(dirs) == 0 {
		problems = append(problems, "the archive contains no pipelines (each pipeline is a folder containing pipeline.yml)")
	}
	return out, problems
}

// extract unpacks a gzipped tar of plain files and directories. It refuses
// anything else (links, devices), anything that would land outside dest,
// and anything over the size limits.
func extract(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errors.New("not a gzip archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, `\`) {
			return fmt.Errorf("unsafe path in the archive: %q", h.Name)
		}
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") {
				return fmt.Errorf("hidden path in the archive: %q (leave .git and similar out of the upload)", h.Name)
			}
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			if files > maxFiles {
				return fmt.Errorf("too many files (limit %d)", maxFiles)
			}
			if h.Size > maxFileBytes {
				return fmt.Errorf("%q is larger than %d bytes", h.Name, maxFileBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("%q appears twice or conflicts with another entry", h.Name)
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxFileBytes+1))
			f.Close()
			if err != nil {
				return err
			}
			if n > maxFileBytes {
				return fmt.Errorf("%q is larger than %d bytes", h.Name, maxFileBytes)
			}
			total += n
			if total > maxTotal {
				return fmt.Errorf("the archive unpacks to more than %d bytes", maxTotal)
			}
		default:
			return fmt.Errorf("%q is not a plain file or folder (links and special files are not allowed)", h.Name)
		}
	}
}

// prune removes old releases, keeping the newest few and never the one
// "current" points at.
func prune(dataDir string) {
	releases := filepath.Join(dataDir, "releases")
	entries, err := os.ReadDir(releases)
	if err != nil {
		return
	}
	cur, _ := os.Readlink(filepath.Join(dataDir, "current"))
	cur = filepath.Base(cur)
	type rel struct {
		name string
		mod  time.Time
	}
	var all []rel
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, rel{e.Name(), info.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	kept := 0
	for _, r := range all {
		if r.name == cur || kept < keepReleases {
			kept++
			continue
		}
		os.RemoveAll(filepath.Join(releases, r.name))
	}
}
