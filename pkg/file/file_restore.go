package file

import (
	"bytes"
	"fmt"
	"io"

	"github.com/guumaster/hostctl/pkg/parser"
)

// Replace hosts file content with the content of a backup file.
//
// The backup is fully read and validated as hosts content first; publication
// goes through the same atomic commit as Flush, so a failure while reading or
// staging the backup can never truncate the live hosts file.
func (f *File) Restore(from string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	fromFile, err := f.fs.Open(from)
	if err != nil {
		return err
	}

	content, err := io.ReadAll(fromFile)
	if err != nil {
		_ = fromFile.Close()

		return fmt.Errorf("reading backup %q: %w", from, err)
	}

	if err := fromFile.Close(); err != nil {
		return fmt.Errorf("closing backup %q: %w", from, err)
	}

	// Validate before publishing: the backup must be parseable hosts content.
	restored, err := parser.Parse(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("%w: backup %q: %v", ErrInvalidContent, from, err)
	}

	name := f.src.Name()

	if err := f.publish(name, content); err != nil {
		return err
	}

	// Keep the in-memory state consistent with the committed bytes.
	f.data = restored
	f.hasBanner = false

	return f.reopenSource()
}
