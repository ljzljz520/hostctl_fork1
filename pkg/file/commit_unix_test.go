//go:build !windows

package file

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/guumaster/hostctl/pkg/types"
)

// errRenameUnsafe emulates rename(2) failing on a bind-mounted target, which
// is exactly what happens when replacing /etc/hosts inside a container.
var errRenameUnsafe = &os.LinkError{
	Op:  "rename",
	Old: "/tmp/.hosts.hostctl.tmp",
	New: "/tmp/hosts",
	Err: syscall.EBUSY,
}

func TestCommit_RenameBusyTargetFailsClearly(t *testing.T) {
	ff := &faultFs{renameErr: errRenameUnsafe}
	f, old, _ := setupFaultCommit(t, ff)

	err := safeFlush(t, f)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsafeTarget)
	assert.Contains(t, err.Error(), "cannot be safely replaced")

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got, "EBUSY on rename must leave the mount target untouched")
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func writeOsHostsFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))

	return path
}

const simpleHosts = `
127.0.0.1 localhost
` + Banner + `
# profile.on profile1
127.0.0.1 first.loc
# end
`

func TestFlush_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := writeOsHostsFile(t, dir, "hosts", simpleHosts, 0o600)

	f, err := NewFile(path)
	require.NoError(t, err)

	require.NoError(t, f.AddRoute("profile1", types.NewRoute("2.2.2.2", "kept.loc")))
	require.NoError(t, f.Flush())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"published file must keep the original permissions")

	f.Close()
}

func TestWriteTo_NewFileUsesDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := writeOsHostsFile(t, dir, "hosts", simpleHosts, 0o600)

	f, err := NewFile(path)
	require.NoError(t, err)

	dst := filepath.Join(dir, "new-hosts")
	require.NoError(t, f.WriteTo(dst))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	f.Close()
}

func TestFlush_ReplacesSymlinkTargetNotLink(t *testing.T) {
	dir := t.TempDir()
	target := writeOsHostsFile(t, dir, "real-hosts", simpleHosts, 0o640)

	link := filepath.Join(dir, "hosts-link")
	require.NoError(t, os.Symlink("real-hosts", link))

	f, err := NewFile(link)
	require.NoError(t, err)

	require.NoError(t, f.AddRoute("profile1", types.NewRoute("3.3.3.3", "via-link.loc")))
	require.NoError(t, f.Flush())

	lInfo, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, lInfo.Mode()&os.ModeSymlink != 0, "the symlink itself must be preserved")

	dest, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "real-hosts", dest)

	tInfo, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, tInfo.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o640), tInfo.Mode().Perm(), "target mode must be preserved")

	content, err := os.ReadFile(link)
	require.NoError(t, err)
	assert.Contains(t, string(content), "3.3.3.3 via-link.loc")

	f.Close()
}

func TestWriteTo_DanglingSymlinkCreatesTargetKeepingLink(t *testing.T) {
	dir := t.TempDir()
	source := writeOsHostsFile(t, dir, "source", simpleHosts, 0o644)

	link := filepath.Join(dir, "dangling-link")
	require.NoError(t, os.Symlink("missing-target", link))

	f, err := NewFile(source)
	require.NoError(t, err)
	require.NoError(t, f.WriteTo(link))

	lInfo, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, lInfo.Mode()&os.ModeSymlink != 0)

	resolved := filepath.Join(dir, "missing-target")
	info, err := os.Stat(resolved)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())

	content, err := os.ReadFile(resolved)
	require.NoError(t, err)
	assert.Contains(t, string(content), "first.loc")

	f.Close()
}

func TestPublish_SymlinkToDirectoryFailsBeforeRewrite(t *testing.T) {
	dir := t.TempDir()
	source := writeOsHostsFile(t, dir, "source", simpleHosts, 0o644)
	mountDir := filepath.Join(dir, "a-directory")
	require.NoError(t, os.Mkdir(mountDir, 0o755))

	link := filepath.Join(dir, "dir-link")
	require.NoError(t, os.Symlink("a-directory", link))

	f, err := NewFile(source)
	require.NoError(t, err)

	err = f.WriteTo(link)
	assert.ErrorIs(t, err, ErrUnsafeTarget)
	assert.Contains(t, err.Error(), "not a regular file")

	entries, rerr := os.ReadDir(mountDir)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "directory target must not be modified")

	f.Close()
}

func TestPublish_SpecialFileFailsBeforeRewrite(t *testing.T) {
	_, err := os.Stat("/dev/null")
	if os.IsNotExist(err) {
		t.Skip("/dev/null not available")
	}

	dir := t.TempDir()
	source := writeOsHostsFile(t, dir, "source", simpleHosts, 0o644)

	f, err := NewFile(source)
	require.NoError(t, err)

	err = f.WriteTo("/dev/null")
	assert.ErrorIs(t, err, ErrUnsafeTarget)

	f.Close()
}

func TestPublish_ReadOnlyDirectoryFailsClearlyAndKeepsOld(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))

	path := writeOsHostsFile(t, locked, "hosts", simpleHosts, 0o644)

	// Snapshot before making its directory unwritable.
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, os.Chmod(locked, 0o555))

	defer func() { _ = os.Chmod(locked, 0o755) }()

	f, err := NewFile(path)
	require.NoError(t, err)

	require.NoError(t, f.AddRoute("profile1", types.NewRoute("4.4.4.4", "nope.loc")))

	err = f.Flush()
	assert.Error(t, err, "publishing into a read-only directory must fail")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "failed commit must leave the complete old file")

	f.Close()
}

func TestResolveTarget_SymlinkLoopFails(t *testing.T) {
	dir := t.TempDir()

	a := filepath.Join(dir, "loop-a")
	b := filepath.Join(dir, "loop-b")
	require.NoError(t, os.Symlink("loop-b", a))
	require.NoError(t, os.Symlink("loop-a", b))

	_, _, err := resolveTarget(afero.NewOsFs(), a)
	assert.ErrorIs(t, err, ErrUnsafeTarget)
}
