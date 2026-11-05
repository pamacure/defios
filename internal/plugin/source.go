package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Source is where a Plugin is installed from: a git URL, optionally at a
// ref, or a local path.
type Source struct {
	// Raw is the source as the lock records it: the git URL with its @ref,
	// or the absolute local path.
	Raw string
	// URL and Ref are a git source's; Path is a local one's.
	URL, Ref, Path string
}

// Git reports whether s is a git source.
func (s Source) Git() bool { return s.URL != "" }

// scpLike is git's user@host:path form.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:`)

// ParseSource reads a source as plugin install takes it. Anything with a
// scheme, or in git's user@host:path form, is a git URL, and an @<ref> in its
// path names a branch, tag or commit; anything else is a local path.
func ParseSource(raw string) (Source, error) {
	if raw == "" {
		return Source{}, errors.New("plugin: empty source")
	}
	if !strings.Contains(raw, "://") && !scpLike.MatchString(raw) {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return Source{}, fmt.Errorf("plugin: %w", err)
		}
		return Source{Raw: abs, Path: abs}, nil
	}
	s := Source{Raw: raw, URL: raw}
	// The path starts at the first / after the host, or after the : of the
	// user@host:path form; an @ before it is the URL's user.
	path := strings.IndexByte(raw, ':') + 1
	if scheme := strings.Index(raw, "://"); scheme >= 0 {
		host := scheme + len("://")
		if path = strings.IndexByte(raw[host:], '/'); path < 0 {
			return s, nil
		}
		path += host
	}
	if at := strings.IndexByte(raw[path:], '@'); at >= 0 {
		s.URL, s.Ref = raw[:path+at], raw[path+at+1:]
		if s.Ref == "" || strings.HasPrefix(s.Ref, "-") {
			return Source{}, fmt.Errorf("plugin: %q: %q is not a ref", raw, s.Ref)
		}
	}
	return s, nil
}

// pinPattern is a commit as git prints it, SHA-1 or SHA-256.
var pinPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Fetch puts the source's files at dst, which must not exist, and returns
// the pin: the commit checked out for git, or the content hash of what was
// copied for a local path.
func Fetch(ctx context.Context, s Source, dst string) (pin string, err error) {
	if !s.Git() {
		return copyLocal(s.Path, dst)
	}
	return clone(ctx, s.URL, dst, func() (string, error) { return resolve(ctx, dst, s.Ref) })
}

// FetchAt clones a git source to dst, which must not exist, at the commit
// pin, ignoring the source's ref: what a restore does with a lock entry.
func FetchAt(ctx context.Context, s Source, pin, dst string) error {
	if !s.Git() {
		return fmt.Errorf("plugin: %s is a local path and cannot be fetched again", s.Raw)
	}
	if !pinPattern.MatchString(pin) {
		return fmt.Errorf("plugin: pin %q is not a commit", pin)
	}
	_, err := clone(ctx, s.URL, dst, func() (string, error) { return pin, nil })
	return err
}

// clone clones url to dst and checks out, detached, the commit at returns,
// removing dst on any error.
func clone(ctx context.Context, url, dst string, at func() (string, error)) (string, error) {
	if err := runGit(ctx, "", "clone", "--quiet", "--", url, dst); err != nil {
		return "", err
	}
	commit, err := at()
	if err == nil {
		err = runGit(ctx, dst, "checkout", "--quiet", "--detach", commit+"^{commit}")
	}
	if err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return commit, nil
}

// ManifestDiff is the diff of plugin.toml between two commits of the git
// checkout at dir.
func ManifestDiff(ctx context.Context, dir, from, to string) (string, error) {
	out, err := gitOutput(ctx, dir, "diff", "--no-color", from, to, "--", ManifestFile)
	return string(out), err
}

// resolve is the commit ref names in the clone at dir: a branch on origin, a
// tag or a commit; "" is the commit cloned.
func resolve(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	} else if out, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/remotes/origin/"+ref+"^{commit}"); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	out, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("plugin: no ref %q in the repository", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

// gitOutput runs git in dir and returns its stdout; an error carries its
// stderr.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("plugin: git %s: %s", args[0], msg)
		}
		return nil, fmt.Errorf("plugin: git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// copyLocal copies the tree at src to dst, leaving out any .git, and returns
// its content hash. Only regular files and directories may be in it.
func copyLocal(src, dst string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("plugin: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("plugin: %s is not a directory", src)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return "", fmt.Errorf("plugin: %w", err)
	}
	err = filepath.WalkDir(src, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, full)
		if err != nil || rel == "." {
			return err
		}
		switch {
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case d.IsDir():
			return os.Mkdir(filepath.Join(dst, rel), 0o700)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is neither a file nor a directory", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(full, filepath.Join(dst, rel), info.Mode().Perm()&0o700|0o600)
	})
	if err != nil {
		os.RemoveAll(dst)
		return "", fmt.Errorf("plugin: %w", err)
	}
	pin, err := contentHash(dst)
	if err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return pin, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// contentHash is sha256:<hex> over each file under dir, by slash path, with
// whether it is executable: what a local Plugin's pin is.
func contentHash(dir string) (string, error) {
	type file struct {
		rel  string
		exec bool
	}
	var files []file
	err := filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		files = append(files, file{filepath.ToSlash(rel), info.Mode()&0o100 != 0})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("plugin: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	h := sha256.New()
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.rel)))
		if err != nil {
			return "", fmt.Errorf("plugin: %w", err)
		}
		fmt.Fprintf(h, "%s\x00%t\x00%d\x00", f.rel, f.exec, len(raw))
		h.Write(raw)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
