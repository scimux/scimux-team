package journal

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenCreatesRegularFileAndRefusesTheRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if f.Offset() != 0 {
		t.Fatalf("offset %d", f.Offset())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()

	if _, err := Open(""); err == nil {
		t.Fatal("empty path")
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("directory")
	}
	missing := filepath.Join(dir, "no-such", "j")
	if _, err := Open(missing); err == nil {
		t.Fatal("missing parent")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("symlink")
	}

	var nilFile *File
	if err := nilFile.Refresh(func(int64, []byte) error { return nil }); err == nil {
		t.Fatal("nil file refresh")
	}
	if _, err := nilFile.ReadLine(0); err == nil {
		t.Fatal("nil file readline")
	}
	if nilFile.Offset() != 0 {
		t.Fatal("nil offset")
	}
}

func TestChmodFailureRemovesANewFile(t *testing.T) {
	restore := chmodFile
	t.Cleanup(func() { chmodFile = restore })
	chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }

	path := filepath.Join(t.TempDir(), "j")
	if _, err := Open(path); err == nil || err.Error() != "chmod failed" {
		t.Fatalf("Open err = %v, want chmod failed", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("file left behind: %v", statErr)
	}
}

func TestRefreshSkipsTheLockWhenNothingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	f := openJournal(t, path)
	restore := lockWait
	t.Cleanup(func() { lockWait = restore })
	lockWait = 0

	held := lockExclusive(t, path)
	defer held.Close()

	called := false
	err := f.Refresh(func(int64, []byte) error {
		called = true
		return nil
	})
	if err != nil || called {
		t.Fatalf("refresh err=%v called=%v", err, called)
	}
}

func TestLinesArriveInOrderWithOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	var got []string
	var offs []int64
	if err := f.Refresh(func(off int64, line []byte) error {
		got = append(got, string(line))
		offs = append(offs, off)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stringsJoin(got) != "one,two" || offs[0] != 0 || offs[1] != 4 {
		t.Fatalf("lines %v offs %v", got, offs)
	}
	if f.Offset() != 8 {
		t.Fatalf("offset %d", f.Offset())
	}
	for i, off := range offs {
		line, err := f.ReadLine(off)
		if err != nil || string(line) != got[i] {
			t.Fatalf("ReadLine(%d) = %q, %v", off, line, err)
		}
	}
	called := false
	if err := f.Refresh(func(int64, []byte) error {
		called = true
		return nil
	}); err != nil || called {
		t.Fatalf("second refresh err=%v called=%v", err, called)
	}
}

func TestTornTailIsIgnoredThenTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("complete\ntorn"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	var got []string
	if err := f.Refresh(func(_ int64, line []byte) error {
		got = append(got, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stringsJoin(got) != "complete" {
		t.Fatalf("refresh saw %v", got)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "complete\ntorn" {
		t.Fatalf("refresh rewrote the file: %q", body)
	}

	if err := f.Update(func(_ int64, line []byte) error {
		got = append(got, string(line))
		return nil
	}, func() ([][]byte, error) {
		return [][]byte{[]byte("new")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if string(body) != "complete\nnew\n" {
		t.Fatalf("after update %q", body)
	}
	if stringsJoin(got) != "complete,new" {
		t.Fatalf("applied %v", got)
	}
	if _, err := f.ReadLine(0); err != nil {
		t.Fatal(err)
	}
	line, err := f.ReadLine(int64(len("complete\n")))
	if err != nil || string(line) != "new" {
		t.Fatalf("ReadLine new = %q %v", line, err)
	}
}

func TestUpdateAppendsSyncsAndFeedsApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	f := openJournal(t, path)
	syncs := 0
	restore := syncFile
	t.Cleanup(func() { syncFile = restore })
	syncFile = func(file *os.File) error {
		syncs++
		return file.Sync()
	}
	var got []string
	var offs []int64
	err := f.Update(func(off int64, line []byte) error {
		got = append(got, string(line))
		offs = append(offs, off)
		return nil
	}, func() ([][]byte, error) {
		return [][]byte{[]byte("a"), []byte("bb")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stringsJoin(got) != "a,bb" || offs[0] != 0 || offs[1] != 2 {
		t.Fatalf("got %v offs %v", got, offs)
	}
	if syncs != 1 {
		t.Fatalf("syncs %d", syncs)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "a\nbb\n" {
		t.Fatalf("file %q", body)
	}
	other, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	disk, _ := os.ReadFile(path)
	if string(disk) != "a\nbb\n" {
		t.Fatalf("other reader %q", disk)
	}

	// Nothing to append and no torn tail: no further sync.
	if err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("syncs after empty update %d", syncs)
	}
}

func TestApplyAndNextErrorsLeaveTheFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("ok\nbad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	reject := errors.New("reject")
	err := f.Update(func(_ int64, line []byte) error {
		if string(line) == "bad" {
			return reject
		}
		return nil
	}, func() ([][]byte, error) {
		t.Fatal("next called after apply failed")
		return nil, nil
	})
	if !errors.Is(err, reject) {
		t.Fatalf("err %v", err)
	}
	if f.Offset() != int64(len("ok\n")) {
		t.Fatalf("offset %d", f.Offset())
	}
	body, _ := os.ReadFile(path)
	if string(body) != "ok\nbad\n" {
		t.Fatalf("file %q", body)
	}
	err = f.Refresh(func(_ int64, line []byte) error {
		if string(line) == "bad" {
			return reject
		}
		return nil
	})
	if !errors.Is(err, reject) || f.Offset() != int64(len("ok\n")) {
		t.Fatalf("retry err=%v offset=%d", err, f.Offset())
	}

	// Consume the bad line, then fail next, then fail apply of a new line.
	if err := f.Refresh(func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), mustRead(t, path)...)
	off := f.Offset()
	nextErr := errors.New("next failed")
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nextErr
	})
	if !errors.Is(err, nextErr) || f.Offset() != off || !bytes.Equal(mustRead(t, path), before) {
		t.Fatalf("next err=%v offset=%d", err, f.Offset())
	}
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nextErr
	})
	if !errors.Is(err, nextErr) || !bytes.Equal(mustRead(t, path), before) {
		t.Fatal("next did not fail the same way")
	}

	err = f.Update(func(_ int64, line []byte) error {
		if string(line) == "new" {
			return reject
		}
		return nil
	}, func() ([][]byte, error) {
		return [][]byte{[]byte("new")}, nil
	})
	if !errors.Is(err, reject) || f.Offset() != off || !bytes.Equal(mustRead(t, path), before) {
		t.Fatalf("apply-new err=%v offset=%d file=%q", err, f.Offset(), mustRead(t, path))
	}
}

func TestInvalidAndOversizedLinesAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	f := openJournal(t, path)
	err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("ok"), []byte("has\nnewline")}, nil
	})
	if !errors.Is(err, ErrInvalidLine) {
		t.Fatalf("err %v", err)
	}
	if body := mustRead(t, path); len(body) != 0 || f.Offset() != 0 {
		t.Fatalf("file %q offset %d", body, f.Offset())
	}
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{bytes.Repeat([]byte("x"), MaxLineBytes+1)}, nil
	})
	if !errors.Is(err, ErrLineTooLong) || len(mustRead(t, path)) != 0 {
		t.Fatalf("oversize err=%v file=%d", err, len(mustRead(t, path)))
	}

	// A line of exactly the limit is accepted. One byte more on disk is not.
	exact := bytes.Repeat([]byte("y"), MaxLineBytes)
	if err := f.Update(func(_ int64, line []byte) error {
		if len(line) != MaxLineBytes {
			t.Fatalf("len %d", len(line))
		}
		return nil
	}, func() ([][]byte, error) {
		return [][]byte{exact}, nil
	}); err != nil {
		t.Fatal(err)
	}

	longPath := filepath.Join(t.TempDir(), "long")
	payload := append(bytes.Repeat([]byte("z"), MaxLineBytes+1), '\n')
	if err := os.WriteFile(longPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	long := openJournal(t, longPath)
	err = long.Refresh(func(int64, []byte) error {
		t.Fatal("apply of an oversized line")
		return nil
	})
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("refresh err %v", err)
	}
	err = long.Refresh(func(int64, []byte) error { return nil })
	if !errors.Is(err, ErrLineTooLong) || long.Offset() != 0 {
		t.Fatalf("retry err=%v offset=%d", err, long.Offset())
	}

	torn := filepath.Join(t.TempDir(), "torn")
	if err := os.WriteFile(torn, bytes.Repeat([]byte("q"), MaxLineBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	tf := openJournal(t, torn)
	if err := tf.Refresh(func(int64, []byte) error { return nil }); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("torn oversize %v", err)
	}
}

func TestLockContentionReturnsErrBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	held := lockExclusive(t, path)
	defer held.Close()

	restoreWait, restoreSleep, restoreNow := lockWait, lockSleep, timeNow
	t.Cleanup(func() {
		lockWait = restoreWait
		lockSleep = restoreSleep
		timeNow = restoreNow
	})
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	step := 0
	timeNow = func() time.Time {
		step++
		// First call sets the deadline. The next call is still inside
		// the wait, so the loop sleeps once. The call after that is late.
		if step < 3 {
			return start
		}
		return start.Add(time.Hour)
	}
	lockWait = time.Minute
	slept := 0
	lockSleep = func(time.Duration) { slept++ }

	err := f.Refresh(func(int64, []byte) error { return nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err %v", err)
	}
	if slept != 1 {
		t.Fatalf("slept %d", slept)
	}
	if body := mustRead(t, path); string(body) != "x\n" {
		t.Fatalf("file %q", body)
	}
}

func TestLockWaitsThenProceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	f := openJournal(t, path)
	held := lockExclusive(t, path)

	restoreWait, restoreSleep, restoreNow := lockWait, lockSleep, timeNow
	t.Cleanup(func() {
		lockWait = restoreWait
		lockSleep = restoreSleep
		timeNow = restoreNow
	})
	fixed := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixed }
	lockWait = time.Minute
	lockSleep = func(time.Duration) {
		if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
			t.Errorf("unlock: %v", err)
		}
	}

	if err := f.Update(func(_ int64, line []byte) error {
		if string(line) != "later" {
			t.Fatalf("line %q", line)
		}
		return nil
	}, func() ([][]byte, error) {
		return [][]byte{[]byte("later")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	held.Close()
	if string(mustRead(t, path)) != "later\n" {
		t.Fatalf("file %q", mustRead(t, path))
	}
}

func TestFlockErrorIsReturned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	restore := flockFn
	t.Cleanup(func() { flockFn = restore })
	flockFn = func(int, int) error { return errors.New("flock broken") }
	err := f.Refresh(func(int64, []byte) error { return nil })
	if err == nil || err.Error() != "flock broken" {
		t.Fatalf("err %v", err)
	}
}

func TestTwoFilesSeeEachOthersAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	a := openJournal(t, path)
	b := openJournal(t, path)
	if err := a.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("from-a")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := b.Refresh(func(_ int64, line []byte) error {
		got = append(got, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stringsJoin(got) != "from-a" {
		t.Fatalf("b saw %v", got)
	}
	if err := b.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("from-b")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := a.Refresh(func(_ int64, line []byte) error {
		got = append(got, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stringsJoin(got) != "from-b" {
		t.Fatalf("a saw %v", got)
	}
}

func TestClosedFileAndShrunkFileFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	if err := f.Refresh(func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	err := f.Refresh(func(int64, []byte) error { return nil })
	if !errors.Is(err, errShrunk) || f.Offset() != 3 {
		t.Fatalf("shrink err=%v offset=%d", err, f.Offset())
	}
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		t.Fatal("next after shrink")
		return nil, nil
	})
	if !errors.Is(err, errShrunk) {
		t.Fatalf("update shrink %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Refresh(func(int64, []byte) error { return nil }); !errors.Is(err, errClosed) {
		t.Fatalf("closed refresh %v", err)
	}
	if err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) { return nil, nil }); !errors.Is(err, errClosed) {
		t.Fatalf("closed update %v", err)
	}
	if _, err := f.ReadLine(0); !errors.Is(err, errClosed) {
		t.Fatalf("closed readline %v", err)
	}
}

func TestReadLineRejectsAMissingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	if _, err := f.ReadLine(0); err == nil {
		t.Fatal("torn read succeeded")
	}
	if _, err := f.ReadLine(40); err == nil {
		t.Fatal("past end succeeded")
	}
}

func TestNilCallbackAndWriteFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	f := openJournal(t, path)
	if err := f.Update(nil, func() ([][]byte, error) { return nil, nil }); err == nil {
		t.Fatal("nil apply")
	}
	if err := f.Update(func(int64, []byte) error { return nil }, nil); err == nil {
		t.Fatal("nil next")
	}

	restoreW, restoreS, restoreT := writeFile, syncFile, truncateFile
	t.Cleanup(func() {
		writeFile = restoreW
		syncFile = restoreS
		truncateFile = restoreT
	})

	writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write failed") }
	err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("z")}, nil
	})
	if err == nil || err.Error() != "write failed" || f.Offset() != 0 || len(mustRead(t, path)) != 0 {
		t.Fatalf("write err=%v offset=%d", err, f.Offset())
	}

	writeFile = func(file *os.File, p []byte) (int, error) {
		if len(p) == 0 {
			return 0, nil
		}
		return 1, nil
	}
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("z")}, nil
	})
	if !errors.Is(err, errShort) || len(mustRead(t, path)) != 0 {
		t.Fatalf("short err=%v file=%q", err, mustRead(t, path))
	}

	writeFile = restoreW
	syncFile = func(*os.File) error { return errors.New("sync failed") }
	err = f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("z")}, nil
	})
	if err == nil || err.Error() != "sync failed" || len(mustRead(t, path)) != 0 || f.Offset() != 0 {
		t.Fatalf("sync err=%v file=%q offset=%d", err, mustRead(t, path), f.Offset())
	}

	// Torn tail, no new lines, truncate fails: file unchanged.
	syncFile = restoreS
	torn := filepath.Join(t.TempDir(), "torn")
	if err := os.WriteFile(torn, []byte("ok\ntorn"), 0o600); err != nil {
		t.Fatal(err)
	}
	tf := openJournal(t, torn)
	if err := tf.Refresh(func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	truncateFile = func(*os.File, int64) error { return errors.New("truncate failed") }
	err = tf.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nil
	})
	if err == nil || err.Error() != "truncate failed" {
		t.Fatalf("truncate err %v", err)
	}
	if string(mustRead(t, torn)) != "ok\ntorn" {
		t.Fatalf("file %q", mustRead(t, torn))
	}
}

func TestEmptyUpdateTruncatesATornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("ok\ntorn"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	if err := f.Refresh(func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	restore := syncFile
	t.Cleanup(func() { syncFile = restore })
	syncFile = func(file *os.File) error {
		syncs++
		return file.Sync()
	}
	if err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, path)) != "ok\n" || syncs != 1 {
		t.Fatalf("file %q syncs %d", mustRead(t, path), syncs)
	}
}

func TestLongLineIsReassembled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	body := append(bytes.Repeat([]byte("m"), 70*1024), '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	var got []byte
	if err := f.Refresh(func(_ int64, line []byte) error {
		got = append([]byte(nil), line...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body[:len(body)-1]) {
		t.Fatalf("len %d", len(got))
	}
	line, err := f.ReadLine(0)
	if err != nil || !bytes.Equal(line, got) {
		t.Fatalf("ReadLine len %d err %v", len(line), err)
	}
}

func TestReadLineLimitedBoundsAndReadErrors(t *testing.T) {
	br := bufio.NewReaderSize(bytes.NewReader([]byte("123456789\n")), 4)
	if _, err := readLineLimited(br, 8); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("overflow %v", err)
	}
	br = bufio.NewReaderSize(bytes.NewReader([]byte("1234\n")), 4)
	line, err := readLineLimited(br, 8)
	if err != nil || string(line) != "1234" {
		t.Fatalf("line %q %v", line, err)
	}
	br = bufio.NewReaderSize(bytes.NewReader([]byte("12\n")), 4)
	line, err = readLineLimited(br, 8)
	if err != nil || string(line) != "12" {
		t.Fatalf("short %q %v", line, err)
	}
	br = bufio.NewReaderSize(bytes.NewReader([]byte("123456789")), 16)
	if _, err := readLineLimited(br, 8); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("torn overflow %v", err)
	}
	br = bufio.NewReaderSize(bytes.NewReader([]byte("1234")), 16)
	if _, err := readLineLimited(br, 8); !errors.Is(err, io.EOF) {
		t.Fatalf("torn %v", err)
	}
	br = bufio.NewReader(failReader{err: errors.New("read failed")})
	if _, err := readLineLimited(br, 8); err == nil || err.Error() != "read failed" {
		t.Fatalf("read err %v", err)
	}
}

func TestInjectedFilesystemFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)

	restoreL, restoreStat, restoreSeek := lstatPath, statFile, seekFile
	t.Cleanup(func() {
		lstatPath = restoreL
		statFile = restoreStat
		seekFile = restoreSeek
	})

	lstatPath = func(string) (os.FileInfo, error) { return nil, errors.New("lstat failed") }
	if _, err := Open(filepath.Join(t.TempDir(), "other")); err == nil || err.Error() != "lstat failed" {
		t.Fatalf("lstat %v", err)
	}
	lstatPath = restoreL

	statFile = func(*os.File) (os.FileInfo, error) { return nil, errors.New("stat failed") }
	if err := f.Refresh(func(int64, []byte) error { return nil }); err == nil || err.Error() != "stat failed" {
		t.Fatalf("refresh stat %v", err)
	}
	statFile = restoreStat
	if err := f.Refresh(func(int64, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}

	calls := 0
	statFile = func(file *os.File) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return file.Stat()
		}
		return nil, errors.New("stat failed")
	}
	err := f.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("z")}, nil
	})
	if err == nil || err.Error() != "stat failed" || string(mustRead(t, path)) != "ab\n" {
		t.Fatalf("update stat %v file %q", err, mustRead(t, path))
	}
	statFile = restoreStat

	// Refresh's own stat succeeds and replay's stat then fails.
	unread := filepath.Join(t.TempDir(), "unread")
	if err := os.WriteFile(unread, []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uf := openJournal(t, unread)
	calls = 0
	statFile = func(file *os.File) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return file.Stat()
		}
		return nil, errors.New("stat failed")
	}
	if err := uf.Refresh(func(int64, []byte) error { return nil }); err == nil || err.Error() != "stat failed" || uf.Offset() != 0 {
		t.Fatalf("replay stat %v offset %d", err, uf.Offset())
	}
	statFile = restoreStat

	// A new file has nothing to replay, so the first seek is the append.
	fresh := openJournal(t, filepath.Join(t.TempDir(), "fresh"))
	seekFile = func(*os.File, int64, int) (int64, error) { return 0, errors.New("seek failed") }
	err = fresh.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return [][]byte{[]byte("z")}, nil
	})
	if err == nil || err.Error() != "seek failed" || fresh.Offset() != 0 {
		t.Fatalf("append seek %v", err)
	}

	seekFile = func(*os.File, int64, int) (int64, error) { return 0, errors.New("seek failed") }
	grown := filepath.Join(t.TempDir(), "grown")
	if err := os.WriteFile(grown, []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gf := openJournal(t, grown)
	if err := gf.Refresh(func(int64, []byte) error { return nil }); err == nil || err.Error() != "seek failed" {
		t.Fatalf("replay seek %v", err)
	}
	if gf.Offset() != 0 {
		t.Fatalf("offset %d", gf.Offset())
	}
	seekFile = restoreSeek

	// ReadLine and Update surface a flock failure of their own.
	flockRestore := flockFn
	t.Cleanup(func() { flockFn = flockRestore })
	flockFn = func(int, int) error { return errors.New("flock broken") }
	if _, err := f.ReadLine(0); err == nil || err.Error() != "flock broken" {
		t.Fatalf("readline flock %v", err)
	}
	if err := fresh.Update(func(int64, []byte) error { return nil }, func() ([][]byte, error) {
		return nil, nil
	}); err == nil || err.Error() != "flock broken" {
		t.Fatalf("update flock %v", err)
	}
}

func TestReadLineReportsAnOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	body := append(bytes.Repeat([]byte("z"), MaxLineBytes+1), '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	_, err := f.ReadLine(0)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err %v", err)
	}
}

func TestEmptyLineIsALine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(path, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openJournal(t, path)
	var n int
	if err := f.Refresh(func(_ int64, line []byte) error {
		if len(line) != 0 {
			t.Fatalf("line %q", line)
		}
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || f.Offset() != 2 {
		t.Fatalf("n=%d offset=%d", n, f.Offset())
	}
}

func openJournal(t *testing.T, path string) *File {
	t.Helper()
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func lockExclusive(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return f
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type failReader struct{ err error }

func (r failReader) Read([]byte) (int, error) { return 0, r.err }

func stringsJoin(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
