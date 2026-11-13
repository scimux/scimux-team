package channel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBlobLimitsAreConstants(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	if s.opts.MaxBlobBytes != defaultMaxBlobBytes || s.opts.MaxBlobsPerChannel != defaultMaxBlobs {
		t.Fatalf("defaults %d %d", s.opts.MaxBlobBytes, s.opts.MaxBlobsPerChannel)
	}
	if defaultMaxBlobBytes != 256<<20 || defaultMaxBlobs != 4096 || maxBlobBytes != 4<<20 {
		t.Fatalf("constants %d %d %d", defaultMaxBlobBytes, defaultMaxBlobs, maxBlobBytes)
	}
	if _, err := Open(dir, Options{MaxBlobBytes: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative bytes %v", err)
	}
	if _, err := Open(dir, Options{MaxBlobsPerChannel: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative count %v", err)
	}
}

func TestRemoveMember(t *testing.T) {
	t.Run("keeps the others and their order", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
		owner := id(0x11)
		high, low, mid := id(0x33), id(0x11), id(0x22)
		if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		meta := filepath.Join(dir, "channels", owner, "chat", metaName)
		plantMembers(t, meta, []Member{{Seat: high, Access: "write"}, {Seat: low, Access: "read"}, {Seat: mid, Access: "write"}})
		mirror := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", mirrorName))
		s = openStore(t, dir, Options{})
		if err := s.RemoveMember(owner, "chat", low); err != nil {
			t.Fatal(err)
		}
		info, err := s.Get(owner, "chat")
		want := []Member{{Seat: high, Access: "write"}, {Seat: mid, Access: "write"}}
		if err != nil || !sameMembers(info.Members, want) {
			t.Fatalf("%+v %v", info.Members, err)
		}
		if !bytes.Equal(mirror, fileBytes(t, filepath.Join(dir, "channels", owner, "chat", mirrorName))) {
			t.Fatal("mirror changed")
		}
		body := fileBytes(t, meta)
		if bytes.Index(body, []byte(high)) > bytes.Index(body, []byte(mid)) || bytes.Contains(body, []byte(low)) {
			t.Fatalf("file order %s", body)
		}
		assertNoFDs(t, dir)
	})

	t.Run("unknown member", func(t *testing.T) {
		s, dir, owner := oneChannel(t)
		before := fileBytes(t, filepath.Join(chatPath(dir, owner, "chat"), metaName))
		if err := s.RemoveMember(owner, "chat", id(0x22)); !errors.Is(err, ErrNotFound) || !bytes.Equal(before, fileBytes(t, filepath.Join(chatPath(dir, owner, "chat"), metaName))) {
			t.Fatal(err)
		}
		assertNoFDs(t, dir)
	})

	t.Run("missing channel", func(t *testing.T) {
		s, dir, owner := oneChannel(t)
		if err := s.RemoveMember(owner, "missing", id(0x22)); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.PutBlob(owner, "missing", sumID([]byte("a")), []byte("a")); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.GetBlob(owner, "missing", sumID([]byte("a"))); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		assertNoFDs(t, dir)
	})

	t.Run("bad names", func(t *testing.T) {
		s, dir, owner := oneChannel(t)
		before := names(t, chatPath(dir, owner, "chat"))
		for _, seat := range []string{"", ".", "..", "nope", strings.Repeat("A", 32)} {
			if err := s.RemoveMember(owner, "chat", seat); !errors.Is(err, ErrInvalid) {
				t.Fatalf("seat %q %v", seat, err)
			}
		}
		if err := s.RemoveMember("..", "chat", id(0x22)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if err := s.RemoveMember(owner, "..", id(0x22)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if names(t, chatPath(dir, owner, "chat")) != before {
			t.Fatalf("names %s", names(t, chatPath(dir, owner, "chat")))
		}
		assertNoFDs(t, dir)
	})

	t.Run("damaged channel", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
		owner, other := id(0x11), id(0x22)
		if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Put(other, "chat", "other"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetMembers(other, "chat", []Member{{Seat: owner, Access: "read"}}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		if err := os.WriteFile(filepath.Join(chatPath(dir, owner, "chat"), metaName), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		s = openStore(t, dir, Options{})
		if err := s.RemoveMember(owner, "chat", id(0x22)); !errors.Is(err, errDamaged) {
			t.Fatal(err)
		}
		info, err := s.Get(other, "chat")
		if err != nil || len(info.Members) != 1 || info.Members[0].Seat != owner {
			t.Fatalf("sibling %+v %v", info, err)
		}
		assertNoFDs(t, dir)
	})

	t.Run("failed rewrite keeps the old list", func(t *testing.T) {
		s, dir, owner := oneChannel(t)
		keep, drop := id(0x22), id(0x33)
		if _, err := s.SetMembers(owner, "chat", []Member{{Seat: keep, Access: "read"}, {Seat: drop, Access: "write"}}); err != nil {
			t.Fatal(err)
		}
		meta := filepath.Join(chatPath(dir, owner, "chat"), metaName)
		before := fileBytes(t, meta)
		saveSeams(t)
		writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write blocked") }
		if err := s.RemoveMember(owner, "chat", drop); err == nil || !strings.Contains(err.Error(), "write blocked") {
			t.Fatal(err)
		}
		if !bytes.Equal(before, fileBytes(t, meta)) {
			t.Fatal("members changed")
		}
		info, err := s.Get(owner, "chat")
		if err != nil || len(info.Members) != 2 || info.Members[1].Seat != drop {
			t.Fatalf("%+v %v", info.Members, err)
		}
		for _, e := range mustReadDir(t, chatPath(dir, owner, "chat")) {
			if strings.HasPrefix(e.Name(), ".channel-") {
				t.Fatalf("temp %s", e.Name())
			}
		}
		assertNoFDs(t, dir)
	})

	t.Run("closed", func(t *testing.T) {
		s, _, owner := oneChannel(t)
		s.Close()
		if err := s.RemoveMember(owner, "chat", id(0x22)); !errors.Is(err, errClosed) {
			t.Fatal(err)
		}
	})
}

func TestBlobRoundTrip(t *testing.T) {
	s, dir, owner := oneChannel(t)
	data := []byte("png-bytes")
	id := sumID(data)
	created, err := s.PutBlob(owner, "chat", id, data)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	got, err := s.GetBlob(owner, "chat", id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%q %v", got, err)
	}
	path := filepath.Join(chatPath(dir, owner, "chat"), blobsName, id)
	if !bytes.Equal(fileBytes(t, path), data) {
		t.Fatal("file bytes")
	}
	if perm(t, filepath.Dir(path)) != 0o700 || perm(t, path) != 0o600 {
		t.Fatalf("modes dir %o file %o", perm(t, filepath.Dir(path)), perm(t, path))
	}
	// A second put writes nothing.
	saveSeams(t)
	var temps int
	createTemp = func(dir, pattern string) (*os.File, error) {
		temps++
		return os.CreateTemp(dir, pattern)
	}
	again, err := s.PutBlob(owner, "chat", id, data)
	if err != nil || again || temps != 0 {
		t.Fatalf("duplicate created %v temps %d err %v", again, temps, err)
	}
	if !bytes.Equal(fileBytes(t, path), data) {
		t.Fatal("duplicate rewrote the file")
	}
	s.Close()
	reopened := openStore(t, dir, Options{})
	got, err = reopened.GetBlob(owner, "chat", id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reopen %q %v", got, err)
	}
	entries, last, _, err := reopened.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 0 || len(entries) != 0 {
		t.Fatalf("log %+v %v", entries, err)
	}
	assertNoFDs(t, dir)
}

func TestBlobIDRules(t *testing.T) {
	s, dir, owner := oneChannel(t)
	data := []byte("a")
	good := sumID(data)
	bads := []string{
		"", ".", "..", "a/b", `a\b`, "a b",
		strings.Repeat("A", 64), strings.Repeat("g", 64),
		strings.Repeat("ab", 31), strings.Repeat("ab", 33),
		good[:63], good + "0",
	}
	chat := chatPath(dir, owner, "chat")
	before := names(t, chat)
	for _, id := range bads {
		if _, err := s.PutBlob(owner, "chat", id, data); !errors.Is(err, ErrInvalid) {
			t.Fatalf("put %q %v", id, err)
		}
		if _, err := s.GetBlob(owner, "chat", id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("get %q %v", id, err)
		}
	}
	if names(t, chat) != before {
		t.Fatalf("rejected ids created %s", names(t, chat))
	}
	if _, err := s.PutBlob("..", "chat", good, data); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(owner, "..", good, data); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.GetBlob(owner, "..", good); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	assertNoFDs(t, dir)
}

func TestBlobSizeAndHash(t *testing.T) {
	s, dir, owner := oneChannel(t)
	if _, err := s.PutBlob(owner, "chat", sumID(nil), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(owner, "chat", sumID([]byte{}), []byte{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	exact := bytes.Repeat([]byte{0x11}, maxBlobBytes)
	created, err := s.PutBlob(owner, "chat", sumID(exact), exact)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	got, err := s.GetBlob(owner, "chat", sumID(exact))
	if err != nil || !bytes.Equal(got, exact) {
		t.Fatalf("exact len %d %v", len(got), err)
	}
	over := bytes.Repeat([]byte{0x11}, maxBlobBytes+1)
	if _, err := s.PutBlob(owner, "chat", sumID(over), over); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(chatPath(dir, owner, "chat"), blobsName, sumID(over))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal(statErr)
	}
	body := []byte("yep")
	if _, err := s.PutBlob(owner, "chat", sumID([]byte("nope")), body); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(chatPath(dir, owner, "chat"), blobsName, sumID([]byte("nope")))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("mismatch created a file")
	}
	assertNoFDs(t, dir)
}

func TestBlobQuotas(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8)), MaxBlobsPerChannel: 1})
		owner := id(0x11)
		if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
		blobDir := filepath.Join(chatPath(dir, owner, "chat"), blobsName)
		if err := os.MkdirAll(blobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, "notes"), []byte("stray"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(blobDir, strings.Repeat("ab", 32)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, strings.Repeat("AB", 32)), []byte("upper"), 0o600); err != nil {
			t.Fatal(err)
		}
		first := []byte("one")
		created, err := s.PutBlob(owner, "chat", sumID(first), first)
		if err != nil || !created {
			t.Fatal(err, created)
		}
		second := []byte("two")
		if _, err := s.PutBlob(owner, "chat", sumID(second), second); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
		if _, statErr := os.Stat(filepath.Join(blobDir, sumID(second))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal(statErr)
		}
		again, err := s.PutBlob(owner, "chat", sumID(first), first)
		if err != nil || again {
			t.Fatalf("duplicate at the limit %v %v", again, err)
		}
		s.Close()
		reopened := openStore(t, dir, Options{MaxBlobsPerChannel: 1})
		if _, err := reopened.PutBlob(owner, "chat", sumID(second), second); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
		got, err := reopened.GetBlob(owner, "chat", sumID(first))
		if err != nil || !bytes.Equal(got, first) {
			t.Fatalf("kept %q %v", got, err)
		}
		assertNoFDs(t, dir)
	})

	t.Run("bytes", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8)), MaxBlobBytes: 4})
		owner := id(0x11)
		if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
		two := []byte("ab")
		if created, err := s.PutBlob(owner, "chat", sumID(two), two); err != nil || !created {
			t.Fatal(err, created)
		}
		rest := []byte("cd")
		if created, err := s.PutBlob(owner, "chat", sumID(rest), rest); err != nil || !created {
			t.Fatal(err, created)
		}
		more := []byte("e")
		if _, err := s.PutBlob(owner, "chat", sumID(more), more); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
		// A single blob under 4 MiB that would pass the channel limit is quota.
		wide := bytes.Repeat([]byte{0x22}, 5)
		fresh := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8)), MaxBlobBytes: 4})
		other := id(0x22)
		if _, _, err := fresh.Put(other, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
		if _, err := fresh.PutBlob(other, "chat", sumID(wide), wide); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
		s.Close()
		reopened := openStore(t, dir, Options{MaxBlobBytes: 4})
		if _, err := reopened.PutBlob(owner, "chat", sumID(more), more); !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
		if again, err := reopened.PutBlob(owner, "chat", sumID(two), two); err != nil || again {
			t.Fatalf("duplicate %v %v", again, err)
		}
		got, err := reopened.GetBlob(owner, "chat", sumID(rest))
		if err != nil || !bytes.Equal(got, rest) {
			t.Fatalf("kept %q %v", got, err)
		}
		assertNoFDs(t, dir)
	})
}

func TestBlobWriteFailuresLeaveNoBlob(t *testing.T) {
	s, dir, owner := oneChannel(t)
	saveSeams(t)
	data := []byte("blob-a")
	id := sumID(data)
	cases := []struct {
		name string
		set  func()
	}{
		{"mkdir", func() { mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir blocked") } }},
		{"chmod", func() { chmodPath = func(string, os.FileMode) error { return errors.New("chmod blocked") } }},
		{"readdir", func() {
			readDir = func(path string) ([]os.DirEntry, error) {
				if strings.HasSuffix(path, blobsName) {
					return nil, errors.New("readdir blocked")
				}
				return os.ReadDir(path)
			}
		}},
		{"lstat", func() { lstatPath = func(string) (os.FileInfo, error) { return nil, errors.New("lstat blocked") } }},
		{"usage lstat", func() {
			blobDir := filepath.Join(chatPath(dir, owner, "chat"), blobsName)
			if err := os.MkdirAll(blobDir, 0o700); err != nil {
				t.Fatal(err)
			}
			existing := []byte("old")
			if err := os.WriteFile(filepath.Join(blobDir, sumID(existing)), existing, 0o600); err != nil {
				t.Fatal(err)
			}
			lstatPath = func(path string) (os.FileInfo, error) {
				if filepath.Base(path) == sumID(existing) {
					return nil, errors.New("lstat blocked")
				}
				return os.Lstat(path)
			}
		}},
		{"temp", func() { createTemp = func(string, string) (*os.File, error) { return nil, errors.New("temp blocked") } }},
		{"file chmod", func() { chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod blocked") } }},
		{"write", func() { writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write blocked") } }},
		{"short", func() {
			writeFile = func(_ *os.File, p []byte) (int, error) {
				if len(p) == 0 {
					return 0, nil
				}
				return 1, nil
			}
		}},
		{"sync", func() { syncFile = func(*os.File) error { return errors.New("sync blocked") } }},
		{"close", func() { closeFile = func(*os.File) error { return errors.New("close blocked") } }},
		{"rename", func() { renameFile = func(string, string) error { return errors.New("rename blocked") } }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreSeams()
			tc.set()
			if _, err := s.PutBlob(owner, "chat", id, data); err == nil {
				t.Fatal("put succeeded")
			}
			blobDir := filepath.Join(chatPath(dir, owner, "chat"), blobsName)
			if _, statErr := os.Stat(filepath.Join(blobDir, id)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal(statErr)
			}
			if entries, err := os.ReadDir(blobDir); err == nil {
				for _, e := range entries {
					if e.Name() == id || strings.HasPrefix(e.Name(), ".channel-") {
						t.Fatalf("left %s", e.Name())
					}
				}
			}
			assertNoFDs(t, dir)
		})
	}
	restoreSeams()
	created, err := s.PutBlob(owner, "chat", id, data)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	openBlob = func(string) (*os.File, error) { return nil, errors.New("open blocked") }
	if _, err := s.GetBlob(owner, "chat", id); err == nil || !strings.Contains(err.Error(), "open blocked") {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(owner, "chat", id, data); err == nil || !strings.Contains(err.Error(), "open blocked") {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBytes(t, filepath.Join(chatPath(dir, owner, "chat"), blobsName, id)), data) {
		t.Fatal("failed reread changed the blob")
	}
	openBlob = os.Open
	readFull = func(io.Reader) ([]byte, error) { return nil, errors.New("read blocked") }
	if _, err := s.GetBlob(owner, "chat", id); err == nil || !strings.Contains(err.Error(), "read blocked") {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(owner, "chat", id, data); err == nil || !strings.Contains(err.Error(), "read blocked") {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBytes(t, filepath.Join(chatPath(dir, owner, "chat"), blobsName, id)), data) {
		t.Fatal("failed reread changed the blob")
	}
	assertNoFDs(t, dir)
}

func TestDamagedBlobFailsOnlyThatBlob(t *testing.T) {
	s, dir, owner := oneChannel(t)
	other := id(0x22)
	if _, _, err := s.Put(other, "chat", "other"); err != nil {
		t.Fatal(err)
	}
	keep := []byte("keep")
	if created, err := s.PutBlob(owner, "chat", sumID(keep), keep); err != nil || !created {
		t.Fatal(err, created)
	}
	if created, err := s.PutBlob(other, "chat", sumID(keep), keep); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(chatPath(dir, owner, "chat"), blobsName)
	kinds := []struct {
		name  string
		id    string
		plant func(path string)
	}{
		{"directory", sumID([]byte("dir")), func(path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty", sumID([]byte("empty")), func(path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"over 4MiB", sumID([]byte("over")), func(path string) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(maxBlobBytes + 1); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}},
		{"hash", sumID([]byte("yep")), func(path string) {
			if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range kinds {
		t.Run(tc.name, func(t *testing.T) {
			tc.plant(filepath.Join(blobDir, tc.id))
			got, err := s.GetBlob(owner, "chat", tc.id)
			if err := damagedBlob(t, got, err); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetBlob(owner, "chat", sumID(keep))
			if err != nil || !bytes.Equal(got, keep) {
				t.Fatalf("sibling blob %q %v", got, err)
			}
			got, err = s.GetBlob(other, "chat", sumID(keep))
			if err != nil || !bytes.Equal(got, keep) {
				t.Fatalf("other channel %q %v", got, err)
			}
			info, err := s.Get(owner, "chat")
			if err != nil || info.Title != "feat" || info.MirrorLast != 1 {
				t.Fatalf("channel %+v %v", info, err)
			}
			entries, _, _, err := s.Read(owner, "chat", Mirror, 0)
			if err != nil || len(entries) != 1 || entries[0].Key != "k" {
				t.Fatalf("log %+v %v", entries, err)
			}
			assertNoFDs(t, dir)
		})
	}
	if _, err := s.GetBlob(owner, "chat", sumID([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// The damage is not remembered: repairing the file makes the next read work.
	hashPath := filepath.Join(blobDir, sumID([]byte("yep")))
	if err := os.WriteFile(hashPath, []byte("yep"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBlob(owner, "chat", sumID([]byte("yep")))
	if err != nil || !bytes.Equal(got, []byte("yep")) {
		t.Fatalf("repaired %q %v", got, err)
	}
	if _, err := s.PutBlob(owner, "chat", sumID([]byte("yep")), []byte("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// A file whose name is the hash but whose bytes differ is damaged.
	// A put must not overwrite it.
	planted := []byte("real-bytes")
	wrongPath := filepath.Join(blobDir, sumID(planted))
	if err := os.WriteFile(wrongPath, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(owner, "chat", sumID(planted), planted); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBytes(t, wrongPath), []byte("other")) {
		t.Fatal("put overwrote a damaged blob")
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(chatPath(dir, owner, "chat"), metaName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, dir, Options{})
	if _, err := reopened.GetBlob(owner, "chat", sumID(keep)); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if _, err := reopened.PutBlob(owner, "chat", sumID([]byte("z")), []byte("z")); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	got, err = reopened.GetBlob(other, "chat", sumID(keep))
	if err != nil || !bytes.Equal(got, keep) {
		t.Fatalf("other after damage %q %v", got, err)
	}
	assertNoFDs(t, dir)
}

func TestDeleteAndReconcileRemoveBlobs(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other, member := id(0x11), id(0x22), id(0x33)
	for _, id := range []string{owner, other} {
		if _, _, err := s.Put(id, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetMembers(other, "chat", []Member{{Seat: member, Access: "write"}, {Seat: owner, Access: "read"}}); err != nil {
		t.Fatal(err)
	}
	owned := []byte("owned")
	shared := []byte("shared")
	if _, err := s.PutBlob(owner, "chat", sumID(owned), owned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(other, "chat", sumID(shared), shared); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(owner, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatPath(dir, owner, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := s.Reconcile(func(id string) bool { return id == other || id == owner }); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBlob(other, "chat", sumID(shared))
	if err != nil || !bytes.Equal(got, shared) {
		t.Fatalf("member cleanup %q %v", got, err)
	}
	info, err := s.Get(other, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Seat != owner {
		t.Fatalf("members %+v %v", info.Members, err)
	}
	if err := s.Reconcile(func(id string) bool { return id == owner }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatPath(dir, other, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inactive owner's blobs remain")
	}
	assertNoFDs(t, dir)
}

func TestBlobCallsLeaveNoFileOpen(t *testing.T) {
	s, dir, owner := oneChannel(t)
	tracked := &blobOpens{}
	tracked.install(t)
	check := func() {
		t.Helper()
		tracked.check(t, dir)
	}
	if _, err := s.PutBlob(owner, "chat", ".", []byte("a")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	check()
	if _, err := s.PutBlob(owner, "chat", sumID(nil), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	check()
	if _, err := s.GetBlob(owner, "missing", sumID([]byte("a"))); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	check()
	data := []byte("zz")
	if created, err := s.PutBlob(owner, "chat", sumID(data), data); err != nil || !created {
		t.Fatal(err, created)
	}
	check()
	if got, err := s.GetBlob(owner, "chat", sumID(data)); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	check()
	if created, err := s.PutBlob(owner, "chat", sumID(data), data); err != nil || created {
		t.Fatal(err, created)
	}
	check()
	if _, err := s.GetBlob(owner, "chat", sumID([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	check()
	s.Close()
	check()
	if _, err := s.PutBlob(owner, "chat", sumID(data), data); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, err := s.GetBlob(owner, "chat", sumID(data)); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	check()
}

func oneChannel(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner := id(0x11)
	if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
		t.Fatal(err)
	}
	return s, dir, owner
}

func chatPath(dir, owner, chat string) string {
	return filepath.Join(dir, "channels", owner, chat)
}

func sumID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sameMembers(got, want []Member) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func plantMembers(t *testing.T, path string, members []Member) {
	t.Helper()
	var m metaFile
	if err := json.Unmarshal(fileBytes(t, path), &m); err != nil {
		t.Fatal(err)
	}
	m.Members = members
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func damagedBlob(t *testing.T, got []byte, err error) error {
	t.Helper()
	if err == nil || len(got) != 0 {
		t.Fatalf("got %q %v", got, err)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrTooLarge) || errors.Is(err, ErrQuota) {
		t.Fatalf("exported error %v", err)
	}
	if !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	return nil
}

func assertNoFDs(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if target == dir || strings.HasPrefix(target, dir+"/") {
			hits = append(hits, target)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("open files %v", hits)
	}
}

type blobOpens struct{ n int }

func (b *blobOpens) install(t *testing.T) {
	t.Helper()
	saveSeams(t)
	openBlob = func(path string) (*os.File, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		b.n++
		return f, nil
	}
	closeBlob = func(f *os.File) error {
		b.n--
		return f.Close()
	}
}

func (b *blobOpens) check(t *testing.T, dir string) {
	t.Helper()
	if b.n != 0 {
		t.Fatalf("blob files open %d", b.n)
	}
	assertNoFDs(t, dir)
}

func restoreSeams() {
	mkdirAll = os.MkdirAll
	chmodPath = os.Chmod
	readDir = os.ReadDir
	createTemp = os.CreateTemp
	chmodFile = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
	writeFile = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	syncFile = func(f *os.File) error { return f.Sync() }
	closeFile = func(f *os.File) error { return f.Close() }
	renameFile = os.Rename
	lstatPath = os.Lstat
	openBlob = os.Open
	closeBlob = func(f *os.File) error { return f.Close() }
	readFull = io.ReadAll
}
