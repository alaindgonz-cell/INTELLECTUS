package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

// validatePath accepts only canonical relative slash paths: no absolute
// paths, no "." or ".." or empty segments, no backslashes, NUL or control
// characters.
func validatePath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case len(p) > 4096:
		return errors.New("path too long")
	case !utf8.ValidString(p):
		return errors.New("path is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return errors.New("absolute path")
	case strings.Contains(p, `\`):
		return errors.New("backslash in path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("control character in path")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			return errors.New("empty path segment")
		case seg == "." || seg == "..":
			return fmt.Errorf("%q segment", seg)
		case len(seg) > 255:
			return errors.New("path segment too long")
		}
	}
	return nil
}

// validateTree checks every path and the size bounds before anything is
// written.
func validateTree(files map[string]string) error {
	if len(files) > maxTreeFiles {
		return fmt.Errorf("candidate tree has %d files (limit %d)", len(files), maxTreeFiles)
	}
	total := 0
	for p, c := range files {
		if err := validatePath(p); err != nil {
			return fmt.Errorf("invalid candidate path %q: %v", p, err)
		}
		total += len(c)
		if total > maxTreeBytes {
			return fmt.Errorf("candidate tree exceeds %d bytes", maxTreeBytes)
		}
	}
	return nil
}

// writeTree writes files (already validated) under dir as regular files,
// 0644, in 0755 directories, so the unprivileged sandbox uid can read them
// and nobody but the engine can change them. It never follows symlinks and
// never overwrites (a path that is both a file and a directory is refused).
func writeTree(dir string, files map[string]string) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if !underDir(full, dir) {
			return fmt.Errorf("invalid candidate path %q: escapes the tree", p)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("candidate path %q: %w", p, err)
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return fmt.Errorf("candidate path %q: %w", p, err)
		}
		_, werr := f.WriteString(files[p])
		cerr := f.Close()
		if werr != nil {
			return fmt.Errorf("candidate path %q: %w", p, werr)
		}
		if cerr != nil {
			return fmt.Errorf("candidate path %q: %w", p, cerr)
		}
	}
	// Explicit modes: the process umask must not make files unreadable.
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("unexpected symlink %s", p)
		case d.IsDir():
			return os.Chmod(p, 0o755)
		case d.Type().IsRegular():
			return os.Chmod(p, 0o644)
		}
		return fmt.Errorf("unexpected file type at %s", p)
	})
}

// prepareRun creates a fresh run directory under WorkRoot holding the
// candidate tree (work/) and the driver (driver.py). The caller removes it.
func (w *Worker) prepareRun(files map[string]string) (rundir, workDir, driverFile string, err error) {
	rundir, err = os.MkdirTemp(w.cfg.WorkRoot, "intellectus-sbx-")
	if err != nil {
		return "", "", "", fmt.Errorf("sandbox: run directory: %w", err)
	}
	fail := func(e error) (string, string, string, error) {
		os.RemoveAll(rundir)
		return "", "", "", e
	}
	if err := os.Chmod(rundir, 0o755); err != nil {
		return fail(err)
	}
	workDir = filepath.Join(rundir, "work")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		return fail(err)
	}
	if err := writeTree(workDir, files); err != nil {
		return fail(err)
	}
	driverFile = filepath.Join(rundir, "driver.py")
	if err := os.WriteFile(driverFile, driverSource, 0o644); err != nil {
		return fail(err)
	}
	if err := os.Chmod(driverFile, 0o644); err != nil {
		return fail(err)
	}
	return rundir, workDir, driverFile, nil
}
