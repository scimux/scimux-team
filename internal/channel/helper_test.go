package channel

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/journal"
)

func id(b byte) string {
	return strings.Repeat(hex.EncodeToString([]byte{b}), 16)
}

func distinctID(n int) string {
	return fmt.Sprintf("%032x", n)
}

func clockAt(sec int) time.Time {
	return time.Date(2026, 11, 12, 9, 30, sec, 0, time.UTC)
}

func seqBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func keyN(n int) string {
	return fmt.Sprintf("k%04d", n)
}

func jsonString(n int) json.RawMessage {
	b := make([]byte, n)
	b[0] = '"'
	b[n-1] = '"'
	for i := 1; i < n-1; i++ {
		b[i] = 'a'
	}
	return b
}

func openStore(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return clockAt(0) }
	}
	s, err := Open(dir, opts)
	if err != nil || s == nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func assertInfo(t *testing.T, got Info, owner, chat, title, gen string, created time.Time, members []Member, mirror, comments uint64) {
	t.Helper()
	if got.Owner != owner || got.Chat != chat || got.Title != title || got.Generation != gen ||
		!got.Created.Equal(created) || got.MirrorLast != mirror || got.CommentsLast != comments {
		t.Fatalf("info %+v", got)
	}
	if got.Created.Location() != time.UTC {
		t.Fatalf("location %v", got.Created.Location())
	}
	if members == nil {
		members = []Member{}
	}
	if got.Members == nil || len(got.Members) != len(members) {
		t.Fatalf("members %+v", got.Members)
	}
	for i := range members {
		if got.Members[i] != members[i] {
			t.Fatalf("members %+v", got.Members)
		}
	}
	// The returned slice is a copy.
	if len(got.Members) > 0 {
		got.Members[0].Access = "mutated"
		if members[0].Access == "mutated" {
			t.Fatal("members share the caller's slice")
		}
	}
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func names(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func countPath(paths []string, want string) int {
	n := 0
	for _, p := range paths {
		if p == want {
			n++
		}
	}
	return n
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func saveSeams(t *testing.T) {
	t.Helper()
	limit := readBodyLimit
	prev := seams{
		mkdirAll: mkdirAll, chmodPath: chmodPath, readDir: readDir, removeAll: removeAll,
		openMeta: openMeta, createTemp: createTemp, chmodFile: chmodFile, writeFile: writeFile,
		syncFile: syncFile, closeFile: closeFile, renameFile: renameFile, openJournal: openJournal,
		openJournalAt: openJournalAt, closeJournal: closeJournal, readLogLine: readLogLine,
		marshalValue: marshalValue, readFull: readFull, syncDir: syncDir, syncDirFile: syncDirFile,
		beforeLogUpdate: beforeLogUpdate, openBlob: openBlob, closeBlob: closeBlob, lstatPath: lstatPath,
	}
	t.Cleanup(func() {
		readBodyLimit = limit
		mkdirAll = prev.mkdirAll
		chmodPath = prev.chmodPath
		readDir = prev.readDir
		removeAll = prev.removeAll
		openMeta = prev.openMeta
		createTemp = prev.createTemp
		chmodFile = prev.chmodFile
		writeFile = prev.writeFile
		syncFile = prev.syncFile
		closeFile = prev.closeFile
		renameFile = prev.renameFile
		openJournal = prev.openJournal
		openJournalAt = prev.openJournalAt
		closeJournal = prev.closeJournal
		readLogLine = prev.readLogLine
		marshalValue = prev.marshalValue
		readFull = prev.readFull
		syncDir = prev.syncDir
		syncDirFile = prev.syncDirFile
		beforeLogUpdate = prev.beforeLogUpdate
		openBlob = prev.openBlob
		closeBlob = prev.closeBlob
		lstatPath = prev.lstatPath
	})
}

type seams struct {
	mkdirAll        func(string, os.FileMode) error
	chmodPath       func(string, os.FileMode) error
	readDir         func(string) ([]os.DirEntry, error)
	removeAll       func(string) error
	openMeta        func(string) (*os.File, error)
	createTemp      func(string, string) (*os.File, error)
	chmodFile       func(*os.File, os.FileMode) error
	writeFile       func(*os.File, []byte) (int, error)
	syncFile        func(*os.File) error
	closeFile       func(*os.File) error
	renameFile      func(string, string) error
	openJournal     func(string) (*journal.File, error)
	openJournalAt   func(string, int64) (*journal.File, error)
	closeJournal    func(*journal.File) error
	readLogLine     func(*journal.File, int64) ([]byte, error)
	marshalValue    func(any) ([]byte, error)
	readFull        func(io.Reader) ([]byte, error)
	syncDir         func(string) error
	syncDirFile     func(*os.File) error
	beforeLogUpdate func()
	openBlob        func(string) (*os.File, error)
	closeBlob       func(*os.File) error
	lstatPath       func(string) (os.FileInfo, error)
}
