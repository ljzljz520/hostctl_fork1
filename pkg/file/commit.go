package file

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/afero"

	"github.com/guumaster/hostctl/pkg/parser"
	"github.com/guumaster/hostctl/pkg/types"
)

const (
	// defaultMode is used when publishing a file that did not exist before.
	defaultMode os.FileMode = 0o644

	// maxSymlinkHops bounds symlink resolution while preflighting the target.
	maxSymlinkHops = 32

	// maxTempTries bounds temp-file name collisions.
	maxTempTries = 100

	// bannerMark is the content marker used to verify the banner was rendered.
	bannerMark = "Content under this line is handled by hostctl"
)

var (
	// ErrInvalidContent is returned when freshly generated content fails the
	// parse round-trip validation, so it must never be published.
	ErrInvalidContent = errors.New("generated hosts content failed validation")

	// ErrUnsafeTarget is returned when the destination cannot be replaced
	// atomically (mount points, special files, broken symlink chains, ...).
	// The target is guaranteed to be untouched.
	ErrUnsafeTarget = errors.New("target cannot be safely replaced atomically")
)

// render builds the complete hosts file content in memory. Nothing is written
// to disk at this stage, so any rendering error leaves the current file alone.
func (f *File) render() ([]byte, error) {
	if f.data == nil {
		return nil, types.ErrNoContent
	}

	buf := bytes.NewBuffer(nil)

	if err := f.data.DefaultProfile.Render(buf); err != nil {
		return nil, fmt.Errorf("rendering default section: %w", err)
	}

	if _, err := buf.WriteString(fmt.Sprintf("%s\n", Banner)); err != nil {
		return nil, fmt.Errorf("rendering banner: %w", err)
	}

	for _, name := range f.data.ProfileNames {
		if name == types.Default {
			continue
		}

		if err := f.data.Profiles[name].Render(buf); err != nil {
			return nil, fmt.Errorf("rendering profile %q: %w", name, err)
		}
	}

	return buf.Bytes(), nil
}

// validateGenerated re-parses the rendered content and verifies that the
// default section, the profile order and the enabled/disabled status of every
// profile survive a full render -> parse round trip. It is the gate between
// content generation and publication: only validated bytes can be committed.
func validateGenerated(want *types.Content, rendered []byte) error {
	if !bytes.Contains(rendered, []byte(bannerMark)) {
		return fmt.Errorf("%w: hostctl banner is missing", ErrInvalidContent)
	}

	got, err := parser.Parse(bytes.NewReader(rendered))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidContent, err)
	}

	if !equalStringSlices(want.ProfileNames, got.ProfileNames) {
		return fmt.Errorf("%w: profile order changed, want %v got %v",
			ErrInvalidContent, want.ProfileNames, got.ProfileNames)
	}

	if !defaultProfilesEqual(want.DefaultProfile, got.DefaultProfile) {
		return fmt.Errorf("%w: default section changed", ErrInvalidContent)
	}

	for _, name := range want.ProfileNames {
		wp := want.Profiles[name]
		gp := got.Profiles[name]

		if gp == nil {
			return fmt.Errorf("%w: profile %q is missing", ErrInvalidContent, name)
		}

		if wp.Status != gp.Status {
			return fmt.Errorf("%w: profile %q status changed: want %s got %s",
				ErrInvalidContent, name, wp.Status, gp.Status)
		}

		if !equalStringSlices(wp.IPList, gp.IPList) {
			return fmt.Errorf("%w: profile %q ip order changed: want %v got %v",
				ErrInvalidContent, name, wp.IPList, gp.IPList)
		}

		for _, ip := range wp.IPList {
			wantHosts := wp.Routes[ip].HostNames
			gotRoute, ok := gp.Routes[ip]

			if !ok {
				return fmt.Errorf("%w: profile %q lost ip %s", ErrInvalidContent, name, ip)
			}

			if !equalStringSlices(wantHosts, gotRoute.HostNames) {
				return fmt.Errorf("%w: profile %q hosts changed for ip %s: want %v got %v",
					ErrInvalidContent, name, ip, wantHosts, gotRoute.HostNames)
			}
		}
	}

	return nil
}

// defaultProfilesEqual compares default sections ignoring purely blank rows,
// which are normalized away while rendering/parsing.
func defaultProfilesEqual(a, b types.DefaultProfile) bool {
	return equalStringSlices(canonicalDefaultRows(a), canonicalDefaultRows(b))
}

// equalStringSlices compares two slices treating nil and empty as equal.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func canonicalDefaultRows(rows types.DefaultProfile) []string {
	out := []string{}

	for _, r := range rows {
		if r == nil {
			continue
		}

		if r.Comment != "" {
			c := strings.TrimSpace(r.Comment)
			if c == "" {
				continue
			}

			out = append(out, "comment:"+c)

			continue
		}

		if r.IP == "" {
			continue
		}

		out = append(out, fmt.Sprintf("route:%s:%s:%s",
			r.Status, strings.TrimSpace(r.IP), strings.TrimSpace(r.Host)))
	}

	return out
}

// publish atomically replaces dst with the already validated content.
//
// The target is never truncated and never opened for writing: a temp file is
// created in the same directory, fully written, permission-adjusted and
// fsynced, then atomically renamed over the destination. Any failure before
// the rename leaves the destination exactly as it was; rename(2) itself is
// atomic, so observers can only see the complete old or the complete new
// version.
func (f *File) publish(dst string, content []byte) (err error) {
	resolved, targetInfo, err := resolveTarget(f.fs, dst)
	if err != nil {
		return err
	}

	dir := filepath.Dir(resolved)

	dirInfo, err := f.fs.Stat(dir)
	if err != nil {
		return fmt.Errorf("cannot publish %q: %w", dst, err)
	}

	if !dirInfo.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", ErrUnsafeTarget, dir)
	}

	// A target living on another device than its directory is a mount point
	// (e.g. a bind-mounted /etc/hosts): rename(2) would fail with EBUSY and
	// no in-place rewrite can be atomic. Fail before writing anything.
	if targetInfo != nil && !sameDevice(targetInfo, dirInfo) {
		return fmt.Errorf("%w: %q is a mount point on a different device than %q",
			ErrUnsafeTarget, resolved, dir)
	}

	mode := defaultMode
	uid, gid := -1, -1

	if targetInfo != nil {
		mode = targetInfo.Mode().Perm()
		uid, gid = fileOwner(targetInfo)
	}

	tmp, tmpName, err := f.createTemp(dir, resolved, mode)
	if err != nil {
		return fmt.Errorf("creating temporary file for %q: %w", dst, err)
	}

	committed := false

	defer func() {
		if !committed {
			_ = f.fs.Remove(tmpName)
		}
	}()

	if err := writeAll(tmp, content); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("staging %q: %w", dst, err)
	}

	// Preserve target permissions on the replacement inode.
	if err := f.fs.Chmod(tmpName, mode); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("preserving permissions of %q: %w", dst, err)
	}

	// Best effort ownership preservation. It is expected to fail for
	// non-root users; mode is what actually protects the hosts file.
	if uid >= 0 {
		_ = f.fs.Chown(tmpName, uid, gid)
	}

	// Durability barrier: content must be on disk before it is published.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("syncing staged content for %q: %w", dst, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("finishing staged content for %q: %w", dst, err)
	}

	// The first and only mutation of the destination. rename(2) is atomic:
	// it either publishes the complete new content or leaves the old one.
	if err := f.fs.Rename(tmpName, resolved); err != nil {
		if isUnsafeRenameErr(err) {
			return fmt.Errorf("%w: %q: %v", ErrUnsafeTarget, resolved, err)
		}

		return fmt.Errorf("publishing %q: %w", dst, err)
	}

	committed = true

	syncParentDir(f.fs, dir)

	return nil
}

// writeAll writes the whole buffer, treating unsolicited short writes as
// errors instead of silently leaving truncated content behind.
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return fmt.Errorf("invalid write count: %d", n)
		}

		b = b[n:]

		if err != nil {
			return err
		}

		if n == 0 {
			return io.ErrShortWrite
		}
	}

	return nil
}

// resolveTarget follows trailing symlinks so the link itself is preserved and
// only its target is replaced. Dangling links resolve to a not-yet-existing
// regular file; anything else non-regular is rejected up front.
func resolveTarget(fs afero.Fs, dst string) (string, os.FileInfo, error) {
	cur := filepath.Clean(dst)

	for i := 0; i < maxSymlinkHops; i++ {
		fi, isLstat, err := lstat(fs, cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Dangling symlink tail or a brand-new destination.
				return cur, nil, nil
			}

			return "", nil, err
		}

		if isLstat && fi.Mode()&os.ModeSymlink != 0 {
			reader, ok := fs.(afero.LinkReader)
			if !ok {
				return "", nil, fmt.Errorf("%w: %q is a symlink on an unsupported filesystem",
					ErrUnsafeTarget, cur)
			}

			link, err := reader.ReadlinkIfPossible(cur)
			if err != nil {
				return "", nil, fmt.Errorf("reading symlink %q: %w", cur, err)
			}

			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(cur), link)
			}

			cur = filepath.Clean(link)

			continue
		}

		if !fi.Mode().IsRegular() {
			return "", nil, fmt.Errorf("%w: %q is not a regular file", ErrUnsafeTarget, cur)
		}

		return cur, fi, nil
	}

	return "", nil, fmt.Errorf("%w: too many symlink hops at %q", ErrUnsafeTarget, dst)
}

func lstat(fs afero.Fs, name string) (os.FileInfo, bool, error) {
	if ls, ok := fs.(afero.Lstater); ok {
		return ls.LstatIfPossible(name)
	}

	fi, err := fs.Stat(name)

	return fi, false, err
}

// createTemp creates a hidden, exclusively-owned staging file in the same
// directory as the destination, so the final rename stays on one filesystem.
func (f *File) createTemp(dir, resolved string, mode os.FileMode) (afero.File, string, error) {
	base := filepath.Base(resolved)

	var lastErr error

	for i := 0; i < maxTempTries; i++ {
		name := filepath.Join(dir,
			fmt.Sprintf(".%s.hostctl.%d.%d.tmp", base, time.Now().UnixNano(), rand.Int31()))

		tmp, err := f.fs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			return tmp, name, nil
		}

		lastErr = err

		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}

	return nil, "", fmt.Errorf("could not create temp file: %w", lastErr)
}

// syncParentDir fsyncs the directory after a successful rename so the new
// directory entry survives a crash/power loss. Only meaningful on the real OS
// filesystem; failures do not undo the already committed rename.
func syncParentDir(fs afero.Fs, dir string) {
	if _, ok := fs.(*afero.OsFs); !ok {
		return
	}

	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// reopenSource points the file handle at the freshly published inode, so that
// later reads (e.g. backups) see the committed content instead of stale data.
func (f *File) reopenSource() error {
	name := f.src.Name()

	_ = f.src.Close()

	s, err := f.fs.Open(name)
	if err != nil {
		return fmt.Errorf("reopening %q after commit: %w", name, err)
	}

	f.src = s
	f.hasBanner = false

	return nil
}
