//go:build !windows

package file

import (
	"errors"
	"os"
	"syscall"
)

// sameDevice reports whether two files live on the same mounted filesystem.
// When the platform does not expose stat data, assume they do and rely on the
// atomic rename as the final authority.
func sameDevice(a, b os.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)

	if !oka || !okb {
		return true
	}

	return sa.Dev == sb.Dev
}

// fileOwner returns the uid/gid of an existing target so they can be applied
// to the replacement inode.
func fileOwner(fi os.FileInfo) (int, int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}

	return -1, -1
}

// isUnsafeRenameErr reports whether a rename error means the destination
// cannot be replaced atomically at all (mount points, read-only media, ...).
func isUnsafeRenameErr(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		switch linkErr.Err {
		case syscall.EBUSY,
			syscall.EXDEV,
			syscall.EPERM,
			syscall.EACCES,
			syscall.EROFS,
			syscall.EINVAL:
			return true
		}
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EBUSY,
			syscall.EXDEV,
			syscall.EPERM,
			syscall.EACCES,
			syscall.EROFS,
			syscall.EINVAL:
			return true
		}
	}

	return false
}
