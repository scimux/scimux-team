package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/journal"
)

func TestListOrdersAndSkipsStrayNames(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(32))})
	low, high := id(0x11), id(0x22)
	if _, created, err := s.Put(high, "b", "high-b"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, created, err := s.Put(low, "b", "low-b"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, created, err := s.Put(low, "a", "low-a"); err != nil || !created {
		t.Fatal(err, created)
	}
	root := filepath.Join(dir, "channels")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "not-an-owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, low, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, low, "bad name"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openStore(t, dir, Options{})
	list, err := s.List(low)
	if err != nil || len(list) != 2 {
		t.Fatalf("low %d %v %+v", len(list), err, list)
	}
	want := []string{low + "/a", low + "/b"}
	for i, info := range list {
		if info.Owner+"/"+info.Chat != want[i] || info.Members == nil {
			t.Fatalf("%d %+v", i, info)
		}
	}
	highList, err := s.List(high)
	if err != nil || len(highList) != 1 || highList[0].Owner+"/"+highList[0].Chat != high+"/b" || highList[0].Members == nil {
		t.Fatalf("high %+v %v", highList, err)
	}
	if stranger, err := s.List(id(0x33)); err != nil || len(stranger) != 0 {
		t.Fatalf("stranger %+v %v", stranger, err)
	}
	empty := openStore(t, t.TempDir(), Options{})
	none, err := empty.List(low)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty %+v %v", none, err)
	}
}

func TestBodiesStayOpaque(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	bodies := []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`true`),
		json.RawMessage(`false`),
		json.RawMessage(`0`),
		json.RawMessage(`"x"`),
		json.RawMessage(` [ 1 , 2 ] `),
		json.RawMessage(` { "z" : 1 , "a" : 2 } `),
	}
	var batch []NewEntry
	for i, body := range bodies {
		batch = append(batch, NewEntry{Key: keyN(i + 1), Body: body})
	}
	if _, last, err := s.Append(owner, "chat", Mirror, owner, batch); err != nil || last != uint64(len(batch)) {
		t.Fatal(err, last)
	}
	entries, _, _, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || len(entries) != len(bodies) {
		t.Fatal(err, len(entries))
	}
	want := []string{`null`, `true`, `false`, `0`, `"x"`, `[1,2]`, `{"z":1,"a":2}`}
	for i, body := range want {
		if string(entries[i].Body) != body {
			t.Fatalf("%d %s", i, entries[i].Body)
		}
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(` `), json.RawMessage(`{`), json.RawMessage(`1 2`), json.RawMessage(`{}{}`)} {
		if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "nope", Body: bad}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("body %q %v", bad, err)
		}
	}
}

func TestListDoesNotOpenHiddenLogs(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(24))})
	owner, other, stranger := id(0x11), id(0x22), id(0x33)
	if _, _, err := s.Put(owner, "own", "owned"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(other, "secret", "hidden"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(other, "shared", "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(other, "secret", []Member{{Seat: stranger, Access: "read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(other, "shared", []Member{{Seat: owner, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "own", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"own":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "secret", Mirror, other, []NewEntry{{Key: "k", Body: json.RawMessage(`{"secret":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "shared", Comments, other, []NewEntry{{Key: "k", Body: json.RawMessage(`{"shared":1}`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = openStore(t, dir, Options{})
	saveSeams(t)
	var opened []string
	openJournal = func(path string) (*journal.File, error) {
		opened = append(opened, path)
		return journal.Open(path)
	}
	list, err := s.List(owner)
	if err != nil || len(list) != 2 || list[0].Owner+"/"+list[0].Chat != owner+"/own" || list[1].Owner+"/"+list[1].Chat != other+"/shared" {
		t.Fatalf("list %+v %v", list, err)
	}
	if list[0].MirrorLast != 1 || list[1].CommentsLast != 1 || list[1].MirrorLast != 0 {
		t.Fatalf("seq %+v", list)
	}
	secret := filepath.Join(dir, "channels", other, "secret")
	for _, path := range opened {
		if strings.HasPrefix(path, secret) {
			t.Fatalf("opened hidden log %s in %v", path, opened)
		}
	}
	want := []string{
		filepath.Join(dir, "channels", owner, "own", mirrorName),
		filepath.Join(dir, "channels", owner, "own", commentsName),
		filepath.Join(dir, "channels", other, "shared", mirrorName),
		filepath.Join(dir, "channels", other, "shared", commentsName),
	}
	if strings.Join(opened, "\n") != strings.Join(want, "\n") {
		t.Fatalf("opened %s", strings.Join(opened, "\n"))
	}
}

func TestReadReturnsStoredBodiesAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
		t.Fatal(err)
	}
	bodies := []json.RawMessage{
		json.RawMessage(`{"z":1,"a":2}`),
		json.RawMessage(` [1, 2] `),
		json.RawMessage(`"kept"`),
	}
	var batch []NewEntry
	for i, body := range bodies {
		batch = append(batch, NewEntry{Key: keyN(i + 1), Body: body})
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, batch); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openStore(t, dir, Options{})
	entries, last, more, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || more || last != 3 || len(entries) != 3 {
		t.Fatalf("%+v more %v last %d %v", entries, more, last, err)
	}
	want := []string{`{"z":1,"a":2}`, `[1,2]`, `"kept"`}
	for i, body := range want {
		if string(entries[i].Body) != body || entries[i].Seq != uint64(i+1) || entries[i].Key != keyN(i+1) {
			t.Fatalf("%d %+v", i, entries[i])
		}
	}
}

func TestListReturnsALogOpenError(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, _, err := s.Put(owner, "chat", "feat"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"n":1}`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	if err := os.Rename(mirror, mirror+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, err := s.List(owner); err == nil || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if err := os.Remove(mirror); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mirror+".bak", mirror); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(owner)
	if err != nil || len(list) != 1 || list[0].MirrorLast != 1 || list[0].Title != "feat" {
		t.Fatalf("restored %+v %v", list, err)
	}
}

func TestChangedLineFailsOnlyThatChannel(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, _, err := s.Put(owner, "chat", "owned"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(other, "chat", "theirs"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"n":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "k", Body: json.RawMessage(`{"n":2}`)}}); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	original := fileBytes(t, mirror)
	planted := bytes.Replace(original, []byte(`{"n":1}`), []byte(`{"n":9}`), 1)
	if bytes.Equal(planted, original) {
		t.Fatal("plant failed")
	}
	if err := os.WriteFile(mirror, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); !errors.Is(err, errDamaged) || strings.Contains(err.Error(), "n") {
		t.Fatal(err)
	}
	entries, last, _, err := s.Read(other, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || string(entries[0].Body) != `{"n":2}` {
		t.Fatalf("sibling %+v %v", entries, err)
	}
}

func TestUnreadableIndexedLineIsNotDamage(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, _, err := s.Put(owner, "chat", "owned"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(other, "chat", "theirs"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"n":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "k", Body: json.RawMessage(`{"n":2}`)}}); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	original := fileBytes(t, mirror)
	if len(original) == 0 || original[len(original)-1] != '\n' {
		t.Fatalf("line %q", original)
	}
	torn := append([]byte(nil), original...)
	torn[len(torn)-1] = 'x'
	if err := os.WriteFile(mirror, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); err == nil || errors.Is(err, errDamaged) || strings.Contains(err.Error(), "n") {
		t.Fatal(err)
	}
	entries, _, _, err := s.Read(other, "chat", Mirror, 0)
	if err != nil || len(entries) != 1 || string(entries[0].Body) != `{"n":2}` {
		t.Fatalf("sibling %+v %v", entries, err)
	}
	if err := os.WriteFile(mirror, original, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, last, _, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || string(entries[0].Body) != `{"n":1}` {
		t.Fatalf("restored %+v %v", entries, err)
	}
}

func TestOpenCloseAndDefaults(t *testing.T) {
	var none *Store
	if err := none.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	if s.opts.MaxLogBytes != defaultMaxLogBytes || s.opts.MaxChannelsPerOwner != defaultMaxChannels {
		t.Fatalf("defaults %d %d", s.opts.MaxLogBytes, s.opts.MaxChannelsPerOwner)
	}
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(owner, "other", "t"); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, err := s.List(owner); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if err := s.Delete(owner, "chat"); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(owner, "chat", nil); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if err := s.Reconcile(func(string) bool { return true }); !errors.Is(err, errClosed) {
		t.Fatal(err)
	}
	if _, _, err := s.Put("..", "chat", "t"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.Reconcile(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	if _, err := Open("", Options{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	if _, err := Open(missing, Options{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, Options{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "channels"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blocked, Options{}); err == nil {
		t.Fatal("channels file accepted")
	}
	if _, err := Open(dir, Options{MaxLogBytes: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{MaxChannelsPerOwner: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	bare, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Close() })
	info, created, err := bare.Put(owner, "chat", "feat")
	if err != nil || !created || len(info.Generation) != 16 {
		t.Fatal(err, created, info.Generation)
	}
	for _, c := range info.Generation {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("generation %s", info.Generation)
		}
	}
	if info.Created.IsZero() || info.Created.Location() != time.UTC {
		t.Fatal(info.Created)
	}
	stamp := info.Created.Format(timeLayout)
	if stamp != formatTime(info.Created) || len(stamp) != len("2026-11-12T09:30:00.000Z") {
		t.Fatal(stamp)
	}
}

func TestFileModes(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "channels")
	chat := filepath.Join(root, owner, "chat")
	for _, path := range []string{root, filepath.Join(root, owner), chat} {
		if perm(t, path) != 0o700 {
			t.Fatalf("%s mode %o", path, perm(t, path))
		}
	}
	for _, name := range []string{metaName, mirrorName, commentsName} {
		path := filepath.Join(chat, name)
		if perm(t, path) != 0o600 {
			t.Fatalf("%s mode %o", name, perm(t, path))
		}
	}
	if names(t, chat) != "channel.json,comments.jsonl,mirror.jsonl" {
		t.Fatalf("names %s", names(t, chat))
	}
}

func TestReconcileRemovesInactiveOwnersAndMembers(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(64))})
	owner := id(0x11)
	gone := id(0x22)
	kept := id(0x33)
	member := id(0x44)
	dropped := id(0x55)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Put(gone, "chat", "gone"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(kept, "chat", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(owner, "chat", []Member{{Seat: dropped, Access: "write"}, {Seat: member, Access: "read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(kept, "chat", []Member{{Seat: member, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(gone, "chat", Mirror, gone, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	keptMeta := filepath.Join(dir, "channels", kept, "chat", metaName)
	planted := bytes.Replace(fileBytes(t, keptMeta), []byte(`"title":`), []byte(`"title": `), 1)
	if bytes.Equal(planted, fileBytes(t, keptMeta)) {
		t.Fatal("plant failed")
	}
	s.Close()
	if err := os.WriteFile(keptMeta, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	// A damaged channel of an inactive owner is removed without parsing.
	secret := []byte(`{"title":"PLANTED-SECRET"}`)
	if err := os.WriteFile(filepath.Join(dir, "channels", gone, "chat", metaName), secret, 0o600); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir, Options{})
	active := map[string]bool{owner: true, kept: true, member: true}
	if err := s.Reconcile(func(id string) bool { return active[id] }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(gone, "chat"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "channels", gone, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inactive owner's channel remains")
	}
	info, err := s.Get(owner, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Seat != member || info.MirrorLast != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	if !bytes.Equal(fileBytes(t, keptMeta), planted) {
		t.Fatalf("untouched file rewritten %s", fileBytes(t, keptMeta))
	}
	keptInfo, err := s.Get(kept, "chat")
	if err != nil || len(keptInfo.Members) != 1 || keptInfo.Members[0].Seat != member {
		t.Fatalf("%+v %v", keptInfo, err)
	}
	// A second pass with nothing inactive leaves the rewritten list in place.
	metaAfter := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", metaName))
	if err := s.Reconcile(func(id string) bool { return active[id] }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(metaAfter, fileBytes(t, filepath.Join(dir, "channels", owner, "chat", metaName))) {
		t.Fatal("clean reconcile rewrote a channel")
	}
	if err := s.Reconcile(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestDamagedRecordFailsOnlyThatChannel(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Put(other, "chat", "other"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "ok", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	good := fileBytes(t, mirror)
	secret := "PLANTED-SECRET"
	line := `{"seq":2,"key":"bad","author":"` + owner + `","at":"2026-11-12T09:30:00.000Z","body":"` + secret + `","extra":true}`
	if err := os.WriteFile(mirror, append(append([]byte(nil), good...), []byte(line+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); !errors.Is(err, errDamaged) || strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
	list, err := s.List(other)
	if err != nil || len(list) != 1 || list[0].Owner != other || list[0].Chat != "chat" || list[0].MirrorLast != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	entries, last, _, err := s.Read(other, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || entries[0].Key != "ok" {
		t.Fatalf("sibling %+v %v", entries, err)
	}
	if _, _, err := s.Append(other, "chat", Comments, other, []NewEntry{{Key: "c", Body: json.RawMessage(`true`)}}); err != nil {
		t.Fatal(err)
	}
	// Damage sticks for this process even after the file is repaired.
	if err := os.WriteFile(mirror, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if err := s.Delete(owner, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "channels", owner, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("damaged channel was not removed")
	}

	// A fresh store reads the repaired sibling and a repaired copy of the log.
	repaired := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	entries, _, _, err = repaired.Read(other, "chat", Mirror, 0)
	if err != nil || len(entries) != 1 {
		t.Fatal(err, len(entries))
	}
}

func TestDamagedMetaAndLogShapes(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"a":1}`)}}); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "channels", owner, "chat", metaName)
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	goodMeta := fileBytes(t, meta)
	goodMirror := fileBytes(t, mirror)
	s.Close()

	cases := []string{
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[],"extra":1}`,
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[]}{"x":1}`,
		`{"title":"feat","generation":"GGGGGGGGGGGGGGGG","created":"2026-11-12T09:30:00.000Z","members":[]}`,
		`{"title":"bad` + "\n" + `","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[]}`,
		`{"title":"feat","generation":"0102030405060708","created":"yesterday","members":[]}`,
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[{"seat":"nope","access":"read"}]}`,
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[{"seat":"` + id(0x22) + `","access":"owner"}]}`,
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":[{"seat":"` + id(0x22) + `","access":"read"},{"seat":"` + id(0x22) + `","access":"write"}]}`,
		`{`,
		``,
	}
	for _, raw := range cases {
		if err := os.WriteFile(meta, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s = openStore(t, dir, Options{})
		_, err := s.Get(owner, "chat")
		s.Close()
		if !errors.Is(err, errDamaged) || strings.Contains(err.Error(), "PLANTED") {
			t.Fatalf("meta %s -> %v", raw, err)
		}
	}
	// Omitted members and a null list are an empty list, not damage.
	for _, raw := range []string{
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z"}`,
		`{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:00.000Z","members":null}`,
	} {
		if err := os.WriteFile(meta, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s = openStore(t, dir, Options{})
		info, err := s.Get(owner, "chat")
		s.Close()
		if err != nil || info.Members == nil || len(info.Members) != 0 || info.Title != "feat" {
			t.Fatalf("null members %+v %v", info, err)
		}
	}
	over := bytes.Repeat([]byte("x"), maxMetaBytes+1)
	if err := os.WriteFile(meta, over, 0o600); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(meta, goodMeta, 0o600); err != nil {
		t.Fatal(err)
	}

	author := owner
	at := "2026-11-12T09:30:00.000Z"
	logs := []string{
		string(goodMirror) + `{"seq":0,"key":"k2","author":"` + author + `","at":"` + at + `","body":1}` + "\n",
		string(goodMirror) + `{"seq":3,"key":"k2","author":"` + author + `","at":"` + at + `","body":1}` + "\n",
		string(goodMirror) + `{"seq":2,"key":"k","author":"` + author + `","at":"` + at + `","body":2}` + "\n",
		string(goodMirror) + `{"seq":2,"key":"k2","author":"nope","at":"` + at + `","body":1}` + "\n",
		string(goodMirror) + `{"seq":2,"key":"k2","author":"` + author + `","at":"nope","body":1}` + "\n",
		string(goodMirror) + `{"seq":2,"key":"k2","author":"` + author + `","at":"` + at + `"}` + "\n",
		string(goodMirror) + `{"seq":2,"key":"bad key","author":"` + author + `","at":"` + at + `","body":1}` + "\n",
		string(goodMirror) + `not json` + "\n",
		string(goodMirror) + `{"seq":2,"key":"k2","author":"` + author + `","at":"` + at + `","body":1}{"x":1}` + "\n",
	}
	for _, raw := range logs {
		if err := os.WriteFile(mirror, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s = openStore(t, dir, Options{})
		_, _, _, err := s.Read(owner, "chat", Mirror, 0)
		s.Close()
		if !errors.Is(err, errDamaged) || strings.Contains(err.Error(), "PLANTED") {
			t.Fatalf("log %s -> %v", raw, err)
		}
	}
	if err := os.WriteFile(mirror, goodMirror, 0o600); err != nil {
		t.Fatal(err)
	}
	// A missing channel.json is damage, and it sticks for the open store.
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, goodMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) {
		t.Fatal("repaired meta was accepted by the same store")
	}
	s.Close()
	fresh := openStore(t, dir, Options{})
	entries, last, _, err := fresh.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 1 || len(entries) != 1 || string(entries[0].Body) != `{"a":1}` {
		t.Fatalf("repaired %+v %v", entries, err)
	}
}

func TestReconcileContinuesAfterADamagedActiveOwner(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(40))})
	first, second, gone, kept := id(0x11), id(0x22), id(0x33), id(0x44)
	member, dropped := id(0x55), id(0x66)
	for _, owner := range []string{first, second, gone, kept} {
		if _, created, err := s.Put(owner, "chat", owner[:4]); err != nil || !created {
			t.Fatal(err, created)
		}
	}
	if _, err := s.SetMembers(kept, "chat", []Member{{Seat: member, Access: "read"}, {Seat: dropped, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, owner := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, "channels", owner, "chat", metaName), []byte(`{"title":"PLANTED-SECRET"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s = openStore(t, dir, Options{})
	err := s.Reconcile(func(id string) bool {
		return id == first || id == second || id == kept || id == member
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "channels", gone, "chat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inactive owner survived a sibling error")
	}
	info, err := s.Get(kept, "chat")
	if err != nil || len(info.Members) != 1 || info.Members[0].Seat != member {
		t.Fatalf("%+v %v", info, err)
	}
	for _, owner := range []string{first, second} {
		if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) {
			t.Fatal(owner, err)
		}
		planted := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", metaName))
		if !bytes.Equal(planted, []byte(`{"title":"PLANTED-SECRET"}`)) {
			t.Fatalf("damaged file rewritten %s", planted)
		}
	}

	saveSeams(t)
	blocked := filepath.Join(dir, "channels", kept, "chat", metaName)
	openMeta = func(path string) (*os.File, error) {
		if path == blocked {
			return nil, errors.New("meta blocked")
		}
		return os.Open(path)
	}
	s.Close()
	s = openStore(t, dir, Options{})
	err = s.Reconcile(func(id string) bool { return id == kept || id == member })
	if err == nil || !strings.Contains(err.Error(), "meta blocked") || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "channels", gone, "chat")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("inactive owner survived a read error")
	}
}

func TestListAndGetIsolateMetaAndCommentDamage(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Put(other, "chat", "other"); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "channels", owner, "chat", metaName)
	goodMeta := fileBytes(t, meta)
	s.Close()
	if err := os.WriteFile(meta, []byte(`{"title":"PLANTED-SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	list, err := s.List(other)
	if err != nil || len(list) != 1 || list[0].Owner != other || list[0].Chat != "chat" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.Get(other, "chat"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(meta, goodMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	comments := filepath.Join(dir, "channels", owner, "chat", commentsName)
	if err := os.WriteFile(comments, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) || strings.Contains(err.Error(), "not-json") {
		t.Fatal(err)
	}
	list, err = s.List(other)
	if err != nil || len(list) != 1 || list[0].Owner != other || list[0].Chat != "chat" {
		t.Fatalf("comment list %+v %v", list, err)
	}
	entries, last, _, err := s.Read(other, "chat", Comments, 0)
	if err != nil || last != 0 || len(entries) != 0 {
		t.Fatalf("sibling comments %+v %v", entries, err)
	}
}

func TestListReturnsAReadErrorAndSkipsDamage(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
	owner, other := id(0x11), id(0x22)
	if _, _, err := s.Put(owner, "chat", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(other, "chat", "hidden"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(dir, "channels", other, "chat", metaName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir, Options{})
	saveSeams(t)
	blocked := filepath.Join(dir, "channels", owner, "chat", metaName)
	openMeta = func(path string) (*os.File, error) {
		if path == blocked {
			return nil, errors.New("meta blocked")
		}
		return os.Open(path)
	}
	if _, err := s.List(owner); err == nil || !strings.Contains(err.Error(), "meta blocked") || errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
	openMeta = os.Open
	list, err := s.List(owner)
	if err != nil || len(list) != 1 || list[0].Owner != owner || list[0].Chat != "chat" || list[0].Title != "kept" {
		t.Fatalf("skip %+v %v", list, err)
	}
	if _, err := s.Get(other, "chat"); !errors.Is(err, errDamaged) {
		t.Fatal(err)
	}
}

func TestSecondSeatUsesItsChannelWhileAnotherIsDamaged(t *testing.T) {
	owner, other := id(0x11), id(0x22)
	member := id(0x33)
	kinds := []struct {
		name  string
		plant func(t *testing.T, dir string)
	}{
		{"meta", func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, "channels", owner, "chat", metaName)
			if err := os.WriteFile(path, []byte(`{"title":"PLANTED-SECRET"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"mirror", func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, "channels", owner, "chat", mirrorName)
			if err := os.WriteFile(path, []byte(string(fileBytes(t, path))+"not-json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"comments", func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, "channels", owner, "chat", commentsName)
			if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(16))})
			if _, created, err := s.Put(owner, "chat", "owned"); err != nil || !created {
				t.Fatal(err, created)
			}
			if _, created, err := s.Put(other, "chat", "theirs"); err != nil || !created {
				t.Fatal(err, created)
			}
			if _, err := s.SetMembers(owner, "chat", []Member{{Seat: other, Access: "read"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "ok", Body: json.RawMessage(`{"n":1}`)}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Append(other, "chat", Comments, other, []NewEntry{{Key: "c", Body: json.RawMessage(`true`)}}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			kind.plant(t, dir)

			s = openStore(t, dir, Options{})
			list, err := s.List(other)
			if err != nil || len(list) != 1 || list[0].Owner != other || list[0].Chat != "chat" || list[0].Title != "theirs" || list[0].MirrorLast != 1 || list[0].CommentsLast != 1 {
				t.Fatalf("list %+v %v", list, err)
			}
			info, err := s.Get(other, "chat")
			if err != nil || info.Title != "theirs" || info.MirrorLast != 1 {
				t.Fatalf("get %+v %v", info, err)
			}
			entries, last, more, err := s.Read(other, "chat", Mirror, 0)
			if err != nil || more || last != 1 || len(entries) != 1 || entries[0].Key != "ok" || string(entries[0].Body) != `{"n":1}` {
				t.Fatalf("read %+v %v", entries, err)
			}
			res, last, err := s.Append(other, "chat", Mirror, other, []NewEntry{{Key: "next", Body: json.RawMessage(`2`)}})
			if err != nil || last != 2 || len(res) != 1 || res[0].Duplicate || res[0].Seq != 2 {
				t.Fatalf("append %+v %d %v", res, last, err)
			}
			if _, err := s.SetMembers(other, "chat", []Member{{Seat: member, Access: "write"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(owner, "chat"); !errors.Is(err, errDamaged) || strings.Contains(err.Error(), "PLANTED-SECRET") || strings.Contains(err.Error(), "not-json") {
				t.Fatal(err)
			}
			damagedLog := Mirror
			if kind.name == "comments" {
				damagedLog = Comments
			}
			if _, _, _, err := s.Read(owner, "chat", damagedLog, 0); !errors.Is(err, errDamaged) {
				t.Fatal(err)
			}
			if _, _, err := s.Append(owner, "chat", damagedLog, owner, []NewEntry{{Key: "nope", Body: json.RawMessage(`1`)}}); !errors.Is(err, errDamaged) {
				t.Fatal(err)
			}
			if _, err := s.SetMembers(owner, "chat", []Member{{Seat: member, Access: "read"}}); !errors.Is(err, errDamaged) {
				t.Fatal(err)
			}
			if _, _, err := s.Put(owner, "chat", "renamed"); !errors.Is(err, errDamaged) {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentAppendsAreContiguous(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	const goroutines = 8
	const each = 20
	var wg sync.WaitGroup
	errc := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				key := keyN(g*each + i + 1)
				_, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: key, Body: json.RawMessage(`1`)}})
				if err != nil {
					errc <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	entries, last, more, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || more || last != goroutines*each || len(entries) != goroutines*each {
		t.Fatalf("len %d last %d more %v %v", len(entries), last, more, err)
	}
	seen := map[string]bool{}
	for i, e := range entries {
		if e.Seq != uint64(i+1) || seen[e.Key] {
			t.Fatalf("seq %d key %s", e.Seq, e.Key)
		}
		seen[e.Key] = true
	}
}

func TestMarshalKeepsCompactBytes(t *testing.T) {
	raw, err := marshalValue(map[string]string{"a": "<b>&"})
	if err != nil || string(raw) != `{"a":"<b>&"}` {
		t.Fatalf("%s %v", raw, err)
	}
	if _, err := marshalValue(make(chan int)); err == nil {
		t.Fatal("channel encoded")
	}
}

func TestReadRejectsABadLogAndAMissingChannel(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, _, err := s.Read(owner, "chat", Log(9), 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read("..", "chat", Mirror, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "missing", Mirror, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.SetMembers("..", "chat", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Delete(owner, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
