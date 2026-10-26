package file

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/guumaster/hostctl/pkg/parser"
	"github.com/guumaster/hostctl/pkg/types"
)

// faultFs wraps an afero.Fs to inject storage failures during publication.
type faultFs struct {
	afero.Fs

	// shortWrite makes temp files return truncated writes.
	shortWrite bool
	// syncErr is returned by temp file Sync.
	syncErr error
	// renameErr is returned by Rename.
	renameErr error
	// killBeforeRename simulates SIGKILL immediately before publishing.
	killBeforeRename bool
	// killAfterRename simulates SIGKILL immediately after a successful publish.
	killAfterRename bool
}

func (ff *faultFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	h, err := ff.Fs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	return &faultFile{File: h, fs: ff}, nil
}

func (ff *faultFs) Rename(oldname, newname string) error {
	if ff.killBeforeRename {
		panic("simulated SIGKILL before rename")
	}

	if ff.renameErr != nil {
		return ff.renameErr
	}

	if err := ff.Fs.Rename(oldname, newname); err != nil {
		return err
	}

	if ff.killAfterRename {
		panic("simulated SIGKILL after rename")
	}

	return nil
}

type faultFile struct {
	afero.File
	fs *faultFs
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.fs.shortWrite {
		// Pretend the kernel accepted one byte less than requested. The
		// trailing single byte can never make progress, surfacing a short
		// write instead of a silently truncated file.
		if len(p) > 1 {
			return f.File.Write(p[:len(p)-1])
		}

		return 0, io.ErrShortWrite
	}

	return f.File.Write(p)
}

func (f *faultFile) Sync() error {
	if f.fs.syncErr != nil {
		return f.fs.syncErr
	}

	return f.File.Sync()
}

const hostsTarget = "/tmp/etc/hosts"

func setupFaultCommit(t *testing.T, ff *faultFs) (*File, []byte, []byte) {
	t.Helper()

	if ff == nil {
		ff = &faultFs{}
	}

	ff.Fs = createBasicFS(t)

	old, err := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, err)

	f, err := NewWithFs(hostsTarget, ff)
	require.NoError(t, err)

	require.NoError(t, f.AddRoute("profile1", types.NewRoute("9.9.9.9", "new.loc")))

	next, err := f.render()
	require.NoError(t, err)
	require.NotEmpty(t, next)
	require.NoError(t, validateGenerated(f.data, next))
	require.False(t, bytes.Equal(old, next))

	return f, old, next
}

func safeFlush(t *testing.T, f *File) (err error) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("process interrupted: %v", r)
		}
	}()

	return f.Flush()
}

func safeRestore(t *testing.T, f *File, from string) (err error) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("process interrupted: %v", r)
		}
	}()

	return f.Restore(from)
}

func assertNoTempLeftovers(t *testing.T, fs afero.Fs, dir string) {
	t.Helper()

	entries, err := afero.ReadDir(fs, dir)
	require.NoError(t, err)

	for _, e := range entries {
		assert.False(t,
			strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".tmp"),
			"temporary file left behind: %s", e.Name())
	}
}

func assertCompleteOldOrNew(t *testing.T, fs afero.Fs, target string, old, next []byte) {
	t.Helper()

	got, err := afero.ReadFile(fs, target)
	require.NoError(t, err)

	assert.True(t, bytes.Equal(got, old) || bytes.Equal(got, next),
		"target contains a torn write: it must be the complete old or complete new version\nold:\n%s\nnew:\n%s\ngot:\n%s",
		old, next, got)
}

func TestCommit_ShortWriteKeepsOldVersion(t *testing.T) {
	ff := &faultFs{shortWrite: true}
	f, old, next := setupFaultCommit(t, ff)

	err := safeFlush(t, f)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrShortWrite), "expected short write error, got %v", err)

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got, "target must keep the complete old version")
	assert.NotEqual(t, next, got)
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestCommit_SyncFailureKeepsOldVersion(t *testing.T) {
	ff := &faultFs{syncErr: errors.New("disk on fire")}
	f, old, _ := setupFaultCommit(t, ff)

	err := safeFlush(t, f)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "disk on fire")

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got)
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestCommit_KillBeforePublishKeepsOldVersion(t *testing.T) {
	ff := &faultFs{killBeforeRename: true}
	f, old, next := setupFaultCommit(t, ff)

	err := safeFlush(t, f)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "before rename")

	assertCompleteOldOrNew(t, ff.Fs, hostsTarget, old, next)

	got, _ := afero.ReadFile(ff.Fs, hostsTarget)
	assert.Equal(t, old, got, "interruption before rename must leave the old version")
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestCommit_KillAfterPublishKeepsNewVersion(t *testing.T) {
	ff := &faultFs{killAfterRename: true}
	f, old, next := setupFaultCommit(t, ff)

	err := safeFlush(t, f)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "after rename")

	assertCompleteOldOrNew(t, ff.Fs, hostsTarget, old, next)

	got, _ := afero.ReadFile(ff.Fs, hostsTarget)
	assert.Equal(t, next, got, "interruption after rename must leave the new version")
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestCommit_HappyPathPublishesNewVersion(t *testing.T) {
	ff := &faultFs{}
	f, _, next := setupFaultCommit(t, ff)

	require.NoError(t, safeFlush(t, f))

	got, err := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, err)
	assert.Equal(t, next, got)
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")

	// The reopened handle reads committed content (backups after flush are fresh).
	_, err = f.src.Seek(0, io.SeekStart)
	require.NoError(t, err)

	reread, err := io.ReadAll(f.src)
	require.NoError(t, err)
	assert.Equal(t, next, reread)
}

func TestValidateGenerated(t *testing.T) {
	ff := &faultFs{}
	f, _, valid := setupFaultCommit(t, ff)

	t.Run("valid content", func(t *testing.T) {
		assert.NoError(t, validateGenerated(f.data, valid))
	})

	t.Run("torn content is rejected", func(t *testing.T) {
		torn := valid[:len(valid)/2]
		err := validateGenerated(f.data, torn)
		assert.ErrorIs(t, err, ErrInvalidContent)
	})

	t.Run("missing banner is rejected", func(t *testing.T) {
		noBanner := bytes.ReplaceAll(valid, []byte(bannerMark), []byte("manually managed"))
		err := validateGenerated(f.data, noBanner)
		assert.ErrorIs(t, err, ErrInvalidContent)
	})

	t.Run("flipped status is rejected", func(t *testing.T) {
		flipped := bytes.Replace(valid,
			[]byte("# profile.on profile1"), []byte("# profile.off profile1"), 1)
		err := validateGenerated(f.data, flipped)
		assert.ErrorIs(t, err, ErrInvalidContent)
		assert.Contains(t, err.Error(), "status changed")
	})

	t.Run("reordered profiles are rejected", func(t *testing.T) {
		// Reorder profile blocks by swapping the on/off header markers while
		// keeping all bytes present: build data with a different order and
		// validate against it.
		other := *f.data
		other.ProfileNames = []string{"profile2", "profile1"}
		err := validateGenerated(&other, valid)
		assert.ErrorIs(t, err, ErrInvalidContent)
		assert.Contains(t, err.Error(), "order changed")
	})
}

func TestRestore_ShortWriteKeepsOldVersion(t *testing.T) {
	ff := &faultFs{shortWrite: true}
	f, old, _ := setupFaultCommit(t, ff)

	backup := writeBackup(t, ff, "/tmp/backup", fullHostfile)

	err := safeRestore(t, f, backup)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrShortWrite), "expected short write error, got %v", err)

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got, "restore staging failure must keep the live hosts file")
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestRestore_SyncFailureKeepsOldVersion(t *testing.T) {
	ff := &faultFs{syncErr: errors.New("read-only filesystem")}
	f, old, _ := setupFaultCommit(t, ff)

	backup := writeBackup(t, ff, "/tmp/backup", fullHostfile)

	err := safeRestore(t, f, backup)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read-only filesystem")

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got)
}

func TestRestore_MissingBackupFailsBeforeTouchingTarget(t *testing.T) {
	ff := &faultFs{}
	f, old, _ := setupFaultCommit(t, ff)

	err := f.Restore("/tmp/does-not-exist")
	assert.Error(t, err)

	got, rerr := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, rerr)
	assert.Equal(t, old, got)
	assertNoTempLeftovers(t, ff.Fs, "/tmp/etc")
}

func TestRestore_PublishesBackupAndRefreshesState(t *testing.T) {
	ff := &faultFs{}
	f, _, _ := setupFaultCommit(t, ff)

	backup := writeBackup(t, ff, "/tmp/backup", `
127.0.0.1 localhost
10.0.0.1 gateway
`+Banner+`
# profile.off restored-one
# 1.1.1.1 only.loc
# end
`)

	require.NoError(t, f.Restore(backup))

	published, err := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, err)

	stored, err := afero.ReadFile(ff.Fs, backup)
	require.NoError(t, err)
	assert.Equal(t, stored, published, "restored bytes must match backup exactly")

	// In-memory state follows the committed content.
	assert.Equal(t, []string{"restored-one"}, f.GetProfileNames())
	assert.Equal(t, map[string]types.Status{"restored-one": types.Disabled},
		f.GetStatus([]string{"restored-one"}))

	// A later flush round-trips the restored content.
	require.NoError(t, f.Flush())

	again, err := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, err)

	parsed, err := parser.Parse(bytes.NewReader(again))
	require.NoError(t, err)
	assert.Equal(t, canonicalDefaultRows(f.data.DefaultProfile),
		canonicalDefaultRows(parsed.DefaultProfile))
	assert.Equal(t, []string{"restored-one"}, parsed.ProfileNames)
	assert.Equal(t, types.Disabled, parsed.Profiles["restored-one"].Status)
}

func writeBackup(t *testing.T, fs afero.Fs, name, content string) string {
	t.Helper()

	b, err := fs.Create(name)
	require.NoError(t, err)

	_, err = b.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, b.Close())

	return name
}

func TestFlush_PreservesDefaultOrderAndStatus(t *testing.T) {
	ff := &faultFs{}
	f, _, _ := setupFaultCommit(t, ff)

	require.NoError(t, f.Toggle([]string{"profile2"})) // off -> on
	require.NoError(t, f.Flush())

	published, err := afero.ReadFile(ff.Fs, hostsTarget)
	require.NoError(t, err)

	parsed, err := parser.Parse(bytes.NewReader(published))
	require.NoError(t, err)

	// Default section untouched.
	assert.Equal(t, canonicalDefaultRows(f.data.DefaultProfile),
		canonicalDefaultRows(parsed.DefaultProfile))

	// Profile order preserved.
	assert.Equal(t, []string{"profile1", "profile2"}, parsed.ProfileNames)

	// Toggle is reflected.
	assert.Equal(t, types.Enabled, parsed.Profiles["profile1"].Status)
	assert.Equal(t, types.Enabled, parsed.Profiles["profile2"].Status)

	// Routes are all there in order.
	assert.Equal(t, []string{"127.0.0.1", "9.9.9.9"}, parsed.Profiles["profile1"].IPList)
	assert.Equal(t, []string{"new.loc"}, parsed.Profiles["profile1"].Routes["9.9.9.9"].HostNames)
}
