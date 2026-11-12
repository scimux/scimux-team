// Package journal is an append-only file of lines that several processes
// may share.
package journal

import (
	"bufio"
	"errors"
	"os"
	"syscall"
	"time"
)

const (
	// MaxLineBytes is the most bytes in one line, not counting its newline.
	MaxLineBytes = 8 << 20
)

var (
	// ErrBusy is returned when the flock cannot be taken before lockWait.
	ErrBusy = errors.New("journal busy")

	// ErrLineTooLong is a line, read or written, longer than MaxLineBytes.
	ErrLineTooLong = errors.New("journal line too long")

	// ErrInvalidLine is a line passed to Update that contains a newline.
	ErrInvalidLine = errors.New("journal line contains a newline")

	errClosed = errors.New("journal closed")
	errShrunk = errors.New("journal file shrank")
	errShort  = errors.New("journal short write")
)

// lockWait is how long Refresh and Update wait for flock. Tests shorten it.
var lockWait = 3 * time.Second

// lockPoll is the pause between non-blocking attempts.
var lockPoll = 10 * time.Millisecond

var (
	lockSleep = time.Sleep
	timeNow   = time.Now
	flockFn   = syscall.Flock

	lstatPath = os.Lstat
	statFile  = func(f *os.File) (os.FileInfo, error) { return f.Stat() }
	seekFile  = func(f *os.File, offset int64, whence int) (int64, error) {
		return f.Seek(offset, whence)
	}
	syncFile     = func(f *os.File) error { return f.Sync() }
	writeFile    = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	truncateFile = func(f *os.File, n int64) error { return f.Truncate(n) }
	chmodFile    = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
)

// File is one open journal.
type File struct{}

var errStub = errors.New("stub")

// Open opens the journal at path.
func Open(path string) (*File, error) {
	return &File{}, nil
}

// Close closes the file.
func (f *File) Close() error { return nil }

// Offset is the byte offset of the next unseen line.
func (f *File) Offset() int64 { return 0 }

// Refresh passes unseen complete lines to apply.
func (f *File) Refresh(apply func(offset int64, line []byte) error) error {
	return errStub
}

// Update applies unseen lines and appends the lines next returns.
func (f *File) Update(apply func(offset int64, line []byte) error, next func() ([][]byte, error)) error {
	return errStub
}

// ReadLine returns the complete line that starts at offset.
func (f *File) ReadLine(offset int64) ([]byte, error) {
	return nil, nil
}

func readLineLimited(br *bufio.Reader, max int) ([]byte, error) {
	return nil, errStub
}
