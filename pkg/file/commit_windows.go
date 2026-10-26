//go:build windows

package file

import "os"

// Device/owner inspection and errno classification are only available on
// unix-like systems. The atomic rename remains the source of truth there.

func sameDevice(_, _ os.FileInfo) bool { return true }

func fileOwner(_ os.FileInfo) (int, int) { return -1, -1 }

func isUnsafeRenameErr(_ error) bool { return false }
