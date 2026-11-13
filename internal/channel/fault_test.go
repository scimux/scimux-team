package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux-team/internal/journal"
)

func TestRejectedCreateLeavesNoDirectory(t *testing.T) {
	owner := id(0x11)
	shortDir := t.TempDir()
	short := openStore(t, shortDir, Options{Entropy: bytes.NewReader(seqBytes(7))})
	if _, _, err := short.Put(owner, "chat", "feat"); err == nil {
		t.Fatal("short entropy created a channel")
	}
	if _, err := os.Stat(filepath.Join(shortDir, "channels", owner, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(64))})
	blocked := filepath.Join(dir, "channels", owner)
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(owner, "chat", "feat"); err == nil {
		t.Fatal("owner path is a file")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}

	saveSeams(t)
	chatDir := filepath.Join(dir, "channels", owner, "chat")
	openJournal = func(path string) (*journal.File, error) {
		if strings.HasSuffix(path, mirrorName) {
			return nil, errors.New("mirror blocked")
		}
		return journal.Open(path)
	}
	if _, _, err := s.Put(owner, "chat", "feat"); err == nil || !strings.Contains(err.Error(), "mirror blocked") {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed mirror create left a directory")
	}

	openJournal = func(path string) (*journal.File, error) {
		if strings.HasSuffix(path, commentsName) {
			return nil, errors.New("comments blocked")
		}
		return journal.Open(path)
	}
	if _, _, err := s.Put(owner, "chat", "feat"); err == nil || !strings.Contains(err.Error(), "comments blocked") {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed comments create left a directory")
	}

	openJournal = journal.Open
	chmodPath = func(path string, mode os.FileMode) error {
		if strings.HasSuffix(path, string(os.PathSeparator)+"chat") {
			return errors.New("mode blocked")
		}
		return os.Chmod(path, mode)
	}
	if _, _, err := s.Put(owner, "chat", "feat"); err == nil || !strings.Contains(err.Error(), "mode blocked") {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed chmod left a directory")
	}

	chmodPath = os.Chmod
	marshalValue = func(any) ([]byte, error) { return nil, errors.New("marshal blocked") }
	if _, _, err := s.Put(owner, "chat", "feat"); err == nil || !strings.Contains(err.Error(), "marshal blocked") {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed meta left a directory")
	}
	marshalValue = json.Marshal
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
}

func TestOpenHonorsDirectorySeams(t *testing.T) {
	saveSeams(t)
	dir := t.TempDir()
	mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir blocked") }
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), "mkdir blocked") {
		t.Fatal(err)
	}
	mkdirAll = os.MkdirAll
	chmodPath = func(string, os.FileMode) error { return errors.New("chmod blocked") }
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), "chmod blocked") {
		t.Fatal(err)
	}
	chmodPath = os.Chmod
	s, err := Open(dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Put(id(0x11), "chat", "feat"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFailureKeepsThePreviousChannel(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	meta := filepath.Join(dir, "channels", owner, "chat", metaName)
	before := fileBytes(t, meta)
	chatDir := filepath.Join(dir, "channels", owner, "chat")

	saveSeams(t)
	restore := func() {
		createTemp = os.CreateTemp
		chmodFile = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
		writeFile = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
		syncFile = func(f *os.File) error { return f.Sync() }
		closeFile = func(f *os.File) error { return f.Close() }
		renameFile = os.Rename
		marshalValue = json.Marshal
	}
	cases := []struct {
		name string
		set  func()
	}{
		{"temp", func() {
			createTemp = func(string, string) (*os.File, error) { return nil, errors.New("temp blocked") }
		}},
		{"chmod", func() {
			chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod blocked") }
		}},
		{"write", func() {
			writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write blocked") }
		}},
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
		{"marshal", func() { marshalValue = func(any) ([]byte, error) { return nil, errors.New("marshal blocked") } }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore()
			tc.set()
			_, created, err := s.Put(owner, "chat", "renamed")
			if err == nil || created {
				t.Fatal(err, created)
			}
			if !bytes.Equal(before, fileBytes(t, meta)) {
				t.Fatal("title changed")
			}
			entries, err := os.ReadDir(chatDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".channel-") {
					t.Fatalf("temp file %s", e.Name())
				}
			}
			info, err := s.Get(owner, "chat")
			if err != nil || info.Title != "feat" {
				t.Fatalf("%+v %v", info, err)
			}
		})
	}
	restore()
	if err := syncDir(filepath.Join(dir, "missing-dir")); err == nil {
		t.Fatal("missing directory synced")
	}
	syncDirFile = func(*os.File) error { return errors.New("dir sync blocked") }
	if err := syncDir(chatDir); err == nil || !strings.Contains(err.Error(), "dir sync blocked") {
		t.Fatal(err)
	}
	syncDirFile = func(f *os.File) error { return f.Sync() }

	updated, created, err := s.Put(owner, "chat", "renamed")
	if err != nil || created || updated.Title != "renamed" {
		t.Fatal(err, created, updated.Title)
	}
	syncDir = func(string) error { return errors.New("dir sync ignored") }
	if _, created, err := s.Put(owner, "chat", "again"); err != nil || created {
		t.Fatal(err, created)
	}
	if !bytes.Contains(fileBytes(t, meta), []byte(`"title":"again"`)) {
		t.Fatalf("ignored sync lost the title %s", fileBytes(t, meta))
	}
}

func TestDeleteAndReconcileKeepTheChannelWhenRemovalFails(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(24))})
	owner, other, third := id(0x11), id(0x22), id(0x33)
	member, dropped := id(0x44), id(0x55)
	for _, id := range []string{owner, other, third} {
		if _, _, err := s.Put(id, "chat", "feat"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(third, "chat", []Member{{Seat: member, Access: "read"}, {Seat: dropped, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	saveSeams(t)
	removeAll = func(string) error { return errors.New("remove blocked") }
	if err := s.Delete(owner, "chat"); err == nil || !strings.Contains(err.Error(), "remove blocked") {
		t.Fatal(err)
	}
	entries, last, _, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || entries[0].Key != "k" {
		t.Fatalf("still readable %+v %v", entries, err)
	}
	err = s.Reconcile(func(id string) bool { return id == third || id == member })
	if err == nil || !strings.Contains(err.Error(), "remove blocked") {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(other, "chat"); err != nil {
		t.Fatal(err)
	}
	info, err := s.Get(third, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Seat != member {
		t.Fatalf("strip continued %+v %v", info, err)
	}

	removeAll = os.RemoveAll
	writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write blocked") }
	if _, err := s.SetMembers(third, "chat", []Member{{Seat: member, Access: "write"}}); err == nil {
		t.Fatal("write succeeded")
	}
	info, err = s.Get(third, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Access != "read" {
		t.Fatalf("members changed %+v %v", info, err)
	}
	writeFile = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	if err := s.Delete(owner, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestScanErrorsAreNotSticky(t *testing.T) {
	owner := id(0x11)
	member := id(0x22)
	call := []struct {
		name string
		fn   func(*Store) error
	}{
		{"put", func(s *Store) error { _, _, err := s.Put(owner, "chat", "feat"); return err }},
		{"get", func(s *Store) error { _, err := s.Get(owner, "chat"); return err }},
		{"list", func(s *Store) error { _, err := s.List(owner); return err }},
		{"delete", func(s *Store) error { return s.Delete(owner, "chat") }},
		{"members", func(s *Store) error {
			_, err := s.SetMembers(owner, "chat", []Member{{Seat: member, Access: "read"}})
			return err
		}},
		{"reconcile", func(s *Store) error { return s.Reconcile(func(string) bool { return true }) }},
		{"append", func(s *Store) error {
			_, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}})
			return err
		}},
		{"read", func(s *Store) error {
			_, _, _, err := s.Read(owner, "chat", Mirror, 0)
			return err
		}},
	}
	for _, tc := range call {
		t.Run(tc.name, func(t *testing.T) {
			saveSeams(t)
			dir := t.TempDir()
			s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
			root := filepath.Join(dir, "channels")
			readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("scan blocked") }
			if err := tc.fn(s); err == nil || !strings.Contains(err.Error(), "scan blocked") {
				t.Fatal(err)
			}
			readDir = os.ReadDir
			if _, err := os.Stat(filepath.Join(root, owner)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed scan created a channel")
			}
			list, err := s.List(owner)
			if err != nil || len(list) != 0 {
				t.Fatalf("retry %+v %v", list, err)
			}
		})
	}

	saveSeams(t)
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	s.Close()
	s = openStore(t, dir, Options{})
	ownerDir := filepath.Join(dir, "channels", owner)
	readDir = func(path string) ([]os.DirEntry, error) {
		if path == ownerDir {
			return nil, errors.New("owner blocked")
		}
		return os.ReadDir(path)
	}
	if _, err := s.List(owner); err == nil || !strings.Contains(err.Error(), "owner blocked") {
		t.Fatal(err)
	}
	readDir = os.ReadDir
	list, err := s.List(owner)
	if err != nil || len(list) != 1 || list[0].Chat != "chat" {
		t.Fatalf("retry %+v %v", list, err)
	}
}

func TestTransientIOIsNotStickyAndDamageIs(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Put(other, "chat", "other"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"a":1}`)}}); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "channels", owner, "chat", metaName)
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	goodMeta := fileBytes(t, meta)
	goodMirror := fileBytes(t, mirror)
	s.Close()

	saveSeams(t)
	s = openStore(t, dir, Options{})
	openMeta = func(path string) (*os.File, error) {
		if path == meta {
			return nil, errors.New("meta blocked")
		}
		return os.Open(path)
	}
	if _, err := s.Get(owner, "chat"); err == nil || !strings.Contains(err.Error(), "meta blocked") || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	openMeta = os.Open
	info, err := s.Get(owner, "chat")
	if err != nil || info.Title != "feat" {
		t.Fatal(err, info.Title)
	}

	readFull = func(io.Reader) ([]byte, error) { return nil, errors.New("read blocked") }
	// The meta is cached, so drop it by using a fresh store.
	s.Close()
	s = openStore(t, dir, Options{})
	if _, err := s.Get(owner, "chat"); err == nil || !strings.Contains(err.Error(), "read blocked") || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	readFull = io.ReadAll
	info, err = s.Get(owner, "chat")
	if err != nil || info.Title != "feat" || info.MirrorLast != 1 {
		t.Fatalf("%+v %v", info, err)
	}

	// A shrink is an I/O failure, not a damaged record, and a restored file works.
	if err := os.Truncate(mirror, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); err == nil || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirror, goodMirror, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, last, _, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || entries[0].Key != "k" {
		t.Fatalf("restored %+v %v", entries, err)
	}

	// Opening the mirror fails and is not remembered.
	s.Close()
	if err := os.Rename(mirror, mirror+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); err == nil || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if err := os.Remove(mirror); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mirror+".bak", mirror); err != nil {
		t.Fatal(err)
	}
	entries, _, _, err = s.Read(owner, "chat", Mirror, 0)
	if err != nil || len(entries) != 1 || string(entries[0].Body) != `{"a":1}` {
		t.Fatalf("reopened %+v %v", entries, err)
	}

	// A line that appears after refresh and before the update damages only this channel.
	secret := "PLANTED-SECRET"
	beforeLogUpdate = func() {
		f, err := os.OpenFile(mirror, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = f.WriteString(`{"secret":"` + secret + `"}` + "\n")
		_ = f.Close()
	}
	_, _, err = s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "new", Body: json.RawMessage(`1`)}})
	if !errors.Is(err, errDamaged) || strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
	if bytes.Contains(fileBytes(t, mirror), []byte(`"key":"new"`)) {
		t.Fatal("partial append")
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "later", Body: json.RawMessage(`1`)}}); !errors.Is(err, errDamaged) || bytes.Contains(fileBytes(t, mirror), []byte(`"key":"later"`)) {
		t.Fatal(err)
	}
	if _, _, err := s.Put(owner, "chat", "renamed"); !errors.Is(err, errDamaged) || bytes.Contains(fileBytes(t, meta), []byte("renamed")) {
		t.Fatal("title changed on a damaged log")
	}
	if _, err := s.SetMembers(owner, "chat", []Member{{Seat: other, Access: "read"}}); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "ok", Body: json.RawMessage(`2`)}}); err != nil {
		t.Fatal(err)
	}
	_ = goodMeta
}

func TestReconcileKeepsTheMemberWhenRewriteFails(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner, member := id(0x11), id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, err := s.SetMembers(owner, "chat", []Member{{Seat: member, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	meta := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", metaName))
	saveSeams(t)
	writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write blocked") }
	err := s.Reconcile(func(id string) bool { return id == owner })
	if err == nil || !strings.Contains(err.Error(), "write blocked") {
		t.Fatal(err)
	}
	info, err := s.Get(owner, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Seat != member {
		t.Fatalf("%+v %v", info, err)
	}
	if !bytes.Equal(meta, fileBytes(t, filepath.Join(dir, "channels", owner, "chat", metaName))) {
		t.Fatal("failed reconcile rewrote channel.json")
	}
}

func TestAppendMarshalFailureStoresNothing(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	before := fileBytes(t, mirror)
	saveSeams(t)
	marshalValue = func(v any) ([]byte, error) {
		if _, ok := v.(Entry); ok {
			return nil, errors.New("entry blocked")
		}
		return json.Marshal(v)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "next", Body: json.RawMessage(`2`)}}); err == nil || !strings.Contains(err.Error(), "entry blocked") {
		t.Fatal(err)
	}
	if !bytes.Equal(before, fileBytes(t, mirror)) {
		t.Fatal("marshal failure wrote")
	}
	marshalValue = json.Marshal
	if _, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "next", Body: json.RawMessage(`2`)}}); err != nil || last != 2 {
		t.Fatal(err, last)
	}
}
