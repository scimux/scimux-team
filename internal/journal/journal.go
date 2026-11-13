// Package journal is an append-only file of lines that several processes
// may share.
//
// A line is complete when it ends in a newline. A final fragment without
// one is a torn write: Refresh ignores it and Update removes it before
// appending. A complete line that cannot be applied is a hard error and
// is never skipped.
//
// Refresh takes a shared flock and Update an exclusive one, both on the
// file itself. The wait is lockWait. When the file size already equals
// Offset, Refresh returns without taking the lock.
package journal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

const (
	// MaxLineBytes is the most bytes in one line, not counting its newline.
	MaxLineBytes = 8 << 20

	filePerm = 0o600
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

// lockPoll is the pause between non-blocking attempts. Tests replace
// lockSleep, so a contended call does not have to wait on the wall clock.
var lockPoll = 10 * time.Millisecond

var (
	lockSleep = time.Sleep
	timeNow   = time.Now
	flockFn   = syscall.Flock

	// The calls below are the real filesystem operations. Tests replace
	// one of them to force a failure the kernel will not produce on a
	// healthy regular file, such as stat failing after a successful open.
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

// File is one open journal. The flock is not held between calls.
type File struct {
	path   string
	f      *os.File
	offset int64
}

// Open opens the journal at path, creating it with mode 0600 when it is
// missing. A path that is not a regular file is refused.
func Open(path string) (*File, error) {
	if path == "" {
		return nil, errors.New("journal: empty path")
	}
	info, err := lstatPath(path)
	created := false
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("journal: %s is not a regular file", path)
		}
	case errors.Is(err, os.ErrNotExist):
		created = true
	default:
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, err
	}
	if created {
		if err := chmodFile(f, filePerm); err != nil {
			f.Close()
			os.Remove(path)
			return nil, err
		}
	}
	return &File{path: path, f: f}, nil
}

// OpenAt opens an existing journal and starts at offset. offset is the
// first byte that has not been applied, so the byte before it is a newline
// unless offset is zero. A missing path, a path that is not a regular
// file, a negative offset, or an offset past the end is refused.
func OpenAt(path string, offset int64) (*File, error) {
	if path == "" {
		return nil, errors.New("journal: empty path")
	}
	if offset < 0 {
		return nil, errors.New("journal: negative offset")
	}
	info, err := lstatPath(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("journal: %s is not a regular file", path)
	}
	if info.Size() < offset {
		return nil, fmt.Errorf("journal: %s is shorter than offset %d", path, offset)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		var b [1]byte
		if _, err := f.ReadAt(b[:], offset-1); err != nil {
			_ = f.Close()
			return nil, err
		}
		if b[0] != '\n' {
			_ = f.Close()
			return nil, fmt.Errorf("journal: %s offset %d is not after a newline", path, offset)
		}
	}
	return &File{path: path, f: f, offset: offset}, nil
}

// Close closes the file. A second call is a no-op.
func (f *File) Close() error {
	if f == nil || f.f == nil {
		return nil
	}
	err := f.f.Close()
	f.f = nil
	return err
}

// Offset is the byte offset of the first line that has not been passed
// to apply. A torn tail, if there is one, starts here.
func (f *File) Offset() int64 {
	if f == nil {
		return 0
	}
	return f.offset
}

// Refresh passes each unseen complete line to apply. When the file size
// equals Offset it returns without taking a lock and without calling apply.
func (f *File) Refresh(apply func(offset int64, line []byte) error) error {
	if err := f.usable(); err != nil {
		return err
	}
	info, err := statFile(f.f)
	if err != nil {
		return err
	}
	if info.Size() == f.offset {
		return nil
	}
	if err := f.lock(false); err != nil {
		return err
	}
	defer f.unlock()
	return f.replay(apply)
}

// Update takes the exclusive lock, passes unseen lines to apply, then
// calls next. The lines next returns are appended, each followed by a
// newline, and synced. Each appended line is then passed to apply.
// State changes only inside apply. An error from apply or next appends
// nothing, and Offset stays before the line that failed.
func (f *File) Update(apply func(offset int64, line []byte) error, next func() ([][]byte, error)) error {
	if err := f.usable(); err != nil {
		return err
	}
	if apply == nil || next == nil {
		return errors.New("journal: nil callback")
	}
	if err := f.lock(true); err != nil {
		return err
	}
	defer f.unlock()
	if err := f.replay(apply); err != nil {
		return err
	}
	lines, err := next()
	if err != nil {
		return err
	}
	for _, line := range lines {
		if bytes.IndexByte(line, '\n') >= 0 {
			return ErrInvalidLine
		}
		if len(line) > MaxLineBytes {
			return ErrLineTooLong
		}
	}
	info, err := statFile(f.f)
	if err != nil {
		return err
	}
	if info.Size() > f.offset {
		if err := truncateFile(f.f, f.offset); err != nil {
			return err
		}
	}
	if len(lines) == 0 {
		if info.Size() != f.offset {
			return syncFile(f.f)
		}
		return nil
	}

	var buf []byte
	type span struct {
		off  int64
		data []byte
	}
	spans := make([]span, 0, len(lines))
	pos := f.offset
	for _, line := range lines {
		data := append([]byte(nil), line...)
		spans = append(spans, span{off: pos, data: data})
		buf = append(buf, data...)
		buf = append(buf, '\n')
		pos += int64(len(data)) + 1
	}
	if _, err := seekFile(f.f, 0, io.SeekEnd); err != nil {
		return err
	}
	n, err := writeFile(f.f, buf)
	if err != nil {
		_ = truncateFile(f.f, f.offset)
		return err
	}
	if n != len(buf) {
		_ = truncateFile(f.f, f.offset)
		return errShort
	}
	if err := syncFile(f.f); err != nil {
		_ = truncateFile(f.f, f.offset)
		return err
	}
	for _, sp := range spans {
		if err := apply(sp.off, sp.data); err != nil {
			_ = truncateFile(f.f, sp.off)
			_ = syncFile(f.f)
			return err
		}
		f.offset = sp.off + int64(len(sp.data)) + 1
	}
	return nil
}

// ReadLine returns the complete line that starts at offset, without its
// newline. offset is a value previously given to apply.
func (f *File) ReadLine(offset int64) ([]byte, error) {
	if err := f.usable(); err != nil {
		return nil, err
	}
	if err := f.lock(false); err != nil {
		return nil, err
	}
	defer f.unlock()
	br := bufio.NewReaderSize(io.NewSectionReader(f.f, offset, MaxLineBytes+1), 64*1024)
	line, err := readLineLimited(br, MaxLineBytes)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return line, nil
}

func (f *File) usable() error {
	if f == nil || f.f == nil {
		return errClosed
	}
	return nil
}

func (f *File) replay(apply func(offset int64, line []byte) error) error {
	info, err := statFile(f.f)
	if err != nil {
		return err
	}
	size := info.Size()
	if size < f.offset {
		return errShrunk
	}
	if size == f.offset {
		return nil
	}
	if _, err := seekFile(f.f, f.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(io.LimitReader(f.f, size-f.offset), 64*1024)
	pos := f.offset
	for {
		line, err := readLineLimited(br, MaxLineBytes)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if applyErr := apply(pos, line); applyErr != nil {
			return applyErr
		}
		pos += int64(len(line)) + 1
		f.offset = pos
	}
}

// readLineLimited returns the next complete line without its newline.
// A fragment that ends at EOF is a torn tail: it returns io.EOF and the
// caller leaves Offset where the fragment starts. A line longer than max
// is ErrLineTooLong.
func readLineLimited(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if err == nil {
			// err == nil means frag ends in a newline.
			content := frag[:len(frag)-1]
			if len(buf)+len(content) > max {
				return nil, ErrLineTooLong
			}
			if len(buf) == 0 {
				return append([]byte(nil), content...), nil
			}
			return append(buf, content...), nil
		}
		if len(frag) > 0 {
			if len(buf)+len(frag) > max {
				return nil, ErrLineTooLong
			}
			buf = append(buf, frag...)
		}
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

func (f *File) lock(exclusive bool) error {
	flag := syscall.LOCK_SH
	if exclusive {
		flag = syscall.LOCK_EX
	}
	flag |= syscall.LOCK_NB
	deadline := timeNow().Add(lockWait)
	for {
		err := flockFn(int(f.f.Fd()), flag)
		if err == nil {
			return nil
		}
		if !isAgain(err) {
			return err
		}
		if !timeNow().Before(deadline) {
			return ErrBusy
		}
		lockSleep(lockPoll)
	}
}

func (f *File) unlock() {
	_ = flockFn(int(f.f.Fd()), syscall.LOCK_UN)
}

func isAgain(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}
