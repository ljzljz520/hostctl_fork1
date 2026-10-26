//go:build !windows

package actions

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	as "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runnerHosts = `
127.0.0.1 localhost

# profile.on profile1
127.0.0.1 first.loc
127.0.0.1 second.loc
# end
`

// execute runs the command and returns the error plus the captured output.
func execute(root *cobra.Command, args ...string) (error, string) {
	b := bytes.NewBufferString("")
	root.SetOut(b)
	root.SetArgs(args)

	err := root.Execute()

	out, _ := io.ReadAll(b)

	return err, string(out)
}

// TestFlush_FailureIsReported makes sure a failed atomic commit is reported as
// an error: the command must exit non-zero, must not print a success message
// and must leave the complete previous hosts file in place.
func TestFlush_FailureIsReported(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))

	hosts := filepath.Join(locked, "hosts")
	require.NoError(t, os.WriteFile(hosts, []byte(runnerHosts), 0o644))

	before, err := os.ReadFile(hosts)
	require.NoError(t, err)

	require.NoError(t, os.Chmod(locked, 0o555))

	defer func() { _ = os.Chmod(locked, 0o755) }()

	root := NewRootCmd()
	cmdErr, out := execute(root, "toggle", "profile1", "--host-file", hosts)

	as.Error(t, cmdErr, "a failed commit must be reported as failure")
	as.NotContains(t, out, "[✔]", "no success message may be printed on failure")

	after, err := os.ReadFile(hosts)
	require.NoError(t, err)
	as.Equal(t, before, after, "hosts file must be unchanged after a failed commit")
}

// TestRestore_FailureIsReported checks that an unreadable backup aborts before
// touching the hosts file and is reported as an error, not a success.
func TestRestore_FailureIsReported(t *testing.T) {
	dir := t.TempDir()

	hosts := filepath.Join(dir, "hosts")
	require.NoError(t, os.WriteFile(hosts, []byte(runnerHosts), 0o644))

	before, err := os.ReadFile(hosts)
	require.NoError(t, err)

	// A directory cannot be read as a backup file.
	backupDir := filepath.Join(dir, "backup-dir")
	require.NoError(t, os.Mkdir(backupDir, 0o755))

	root := NewRootCmd()
	cmdErr, out := execute(root, "restore", "--from", backupDir, "--host-file", hosts)

	as.Error(t, cmdErr)
	as.NotContains(t, out, "restored", "no restore success message on failure")

	after, err := os.ReadFile(hosts)
	require.NoError(t, err)
	as.Equal(t, before, after)
}
