package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/journal"
)

func TestPutCreatesUpdatesAndReplacesTheGeneration(t *testing.T) {
	dir := t.TempDir()
	var ticks int
	entropy := bytes.NewReader(seqBytes(24))
	s := openStore(t, dir, Options{
		Now: func() time.Time {
			ticks++
			return clockAt(ticks)
		},
		Entropy: entropy,
	})
	owner := id(0x11)
	before := entropy.Len()
	info, created, err := s.Put(owner, "chat_a", "feat")
	if err != nil || !created {
		t.Fatal(err, created)
	}
	assertInfo(t, info, owner, "chat_a", "feat", "0102030405060708", clockAt(1), nil, 0, 0)
	if entropy.Len() != before-8 || ticks != 1 {
		t.Fatalf("entropy %d ticks %d", entropy.Len(), ticks)
	}
	raw := fileBytes(t, filepath.Join(dir, "channels", owner, "chat_a", "channel.json"))
	want := `{"title":"feat","generation":"0102030405060708","created":"2026-11-12T09:30:01.000Z","members":[]}` + "\n"
	if string(raw) != want {
		t.Fatalf("meta %s", raw)
	}

	updated, created, err := s.Put(owner, "chat_a", "renamed")
	if err != nil || created || entropy.Len() != before-8 || ticks != 1 {
		t.Fatalf("update created %v err %v ticks %d", created, err, ticks)
	}
	assertInfo(t, updated, owner, "chat_a", "renamed", info.Generation, info.Created, nil, 0, 0)
	if string(fileBytes(t, filepath.Join(dir, "channels", owner, "chat_a", "channel.json"))) == want {
		t.Fatal("title was not rewritten")
	}

	if err := s.Delete(owner, "chat_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(owner, "chat_a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "channels", owner, "chat_a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory remains")
	}
	again, created, err := s.Put(owner, "chat_a", "fresh")
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if again.Generation == info.Generation || again.Generation != "090a0b0c0d0e0f10" || !again.Created.Equal(clockAt(2)) {
		t.Fatalf("regenerated %+v", again)
	}
	entries, _, _, err := s.Read(owner, "chat_a", Mirror, 0)
	if err != nil || len(entries) != 0 || again.MirrorLast != 0 || again.CommentsLast != 0 {
		t.Fatalf("logs not empty %+v %v", again, err)
	}

	// A fractional millisecond is stored at millisecond resolution.
	frac := openStore(t, t.TempDir(), Options{
		Now:     func() time.Time { return time.Date(2026, 11, 12, 9, 30, 0, 1500*1000, time.UTC) },
		Entropy: bytes.NewReader(seqBytes(8)),
	})
	got, _, err := frac.Put(owner, "chat", "t")
	if err != nil || !got.Created.Equal(time.Date(2026, 11, 12, 9, 30, 0, int(time.Millisecond), time.UTC)) {
		t.Fatalf("truncated %+v %v", got.Created, err)
	}
}

func TestValidationRejectsBeforeAnyPathIsBuilt(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(24))})
	owner := id(0x11)
	parent := filepath.Dir(dir)
	before := names(t, parent)
	channels := filepath.Join(dir, "channels")

	owners := []string{
		"", ".", "..", "a/b", `a\b`, "a b",
		strings.Repeat("A", 32), strings.Repeat("g", 32),
		strings.Repeat("1", 31), strings.Repeat("1", 33),
		id(0x11) + "\x00",
	}
	for _, bad := range owners {
		if _, _, err := s.Put(bad, "chat", "t"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("owner %q %v", bad, err)
		}
	}
	chats := []string{"", ".", "..", "a/b", `a\b`, "a b", "a.b", strings.Repeat("a", 65), "a/../b"}
	for _, bad := range chats {
		if _, _, err := s.Put(owner, bad, "t"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("chat %q %v", bad, err)
		}
	}
	titles := []string{strings.Repeat("a", 201), "\n", "\x00", "\x7f", "\xff", string([]byte{0xc3, 0x28})}
	for _, bad := range titles {
		if _, _, err := s.Put(owner, "chat", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("title %q %v", bad, err)
		}
	}
	if got := names(t, channels); got != "" {
		t.Fatalf("rejected names created %s", got)
	}
	if _, _, err := s.Put(owner, strings.Repeat("a", 64), strings.Repeat("b", 200)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(owner, "A_b-1", " lead "); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(owner, "empty-title", ""); err != nil {
		t.Fatal(err)
	}
	if names(t, parent) != before {
		t.Fatalf("parent changed %v", names(t, parent))
	}
	if got := names(t, channels); got != owner {
		t.Fatalf("channels %s", got)
	}
	if _, err := s.Get("..", "chat"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.Delete(owner, ".."); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestSetMembersReplacesSortsAndRejects(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	results, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}})
	if err != nil || len(results) != 1 {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "channels", owner, "chat", "channel.json")
	metaBefore := fileBytes(t, meta)
	mirror := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", "mirror.jsonl"))
	low, high := id(0x0a), id(0x22)
	info, err := s.SetMembers(owner, "chat", []Member{{Seat: high, Access: "write"}, {Seat: low, Access: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{{Seat: low, Access: "read"}, {Seat: high, Access: "write"}}
	assertInfo(t, info, owner, "chat", "feat", "0102030405060708", clockAt(0), want, 1, 0)
	info.Members[0].Access = "write"
	again, err := s.Get(owner, "chat")
	if err != nil || again.Members[0].Access != "read" || again.Generation != info.Generation {
		t.Fatalf("aliased %+v %v", again, err)
	}
	if bytes.Equal(metaBefore, fileBytes(t, meta)) {
		t.Fatal("members did not rewrite channel.json")
	}
	if !bytes.Equal(mirror, fileBytes(t, filepath.Join(dir, "channels", owner, "chat", "mirror.jsonl"))) {
		t.Fatal("members rewrote the mirror")
	}
	cleared, err := s.SetMembers(owner, "chat", nil)
	if err != nil || cleared.Members == nil || len(cleared.Members) != 0 {
		t.Fatal(err, cleared.Members)
	}
	if !bytes.Contains(fileBytes(t, meta), []byte(`"members":[]`)) {
		t.Fatalf("cleared meta %s", fileBytes(t, meta))
	}
	empty, err := s.SetMembers(owner, "chat", []Member{})
	if err != nil || empty.Members == nil || len(empty.Members) != 0 {
		t.Fatal(err, empty.Members)
	}
	if _, err := s.SetMembers(owner, "missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	dup := []Member{{Seat: low, Access: "read"}, {Seat: low, Access: "write"}}
	ownerListed := []Member{{Seat: owner, Access: "read"}}
	badAccess := []Member{{Seat: low, Access: "owner"}}
	badSeat := []Member{{Seat: "nope", Access: "read"}}
	tooMany := make([]Member, 201)
	for i := range tooMany {
		tooMany[i] = Member{Seat: id(byte(i + 1)), Access: "read"}
	}
	for _, members := range [][]Member{dup, ownerListed, badAccess, badSeat, tooMany} {
		if _, err := s.SetMembers(owner, "chat", members); !errors.Is(err, ErrInvalid) {
			t.Fatalf("members %+v %v", members, err)
		}
	}
	exact := make([]Member, 200)
	for i := range exact {
		exact[i] = Member{Seat: distinctID(i + 1), Access: "read"}
	}
	if _, err := s.SetMembers(owner, "chat", exact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembers(owner, "chat", append(exact, Member{Seat: distinctID(201), Access: "read"})); !errors.Is(err, ErrInvalid) {
		t.Fatal("201 members")
	}
}

func TestAppendIsContiguousCompactAndAtomic(t *testing.T) {
	dir := t.TempDir()
	var ticks int
	s := openStore(t, dir, Options{
		Now: func() time.Time {
			ticks++
			return clockAt(ticks)
		},
		Entropy: bytes.NewReader(seqBytes(8)),
	})
	owner := id(0x11)
	other := id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	putTicks := ticks
	body := json.RawMessage(" { \"a\" : \"<b>\" } ")
	results, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{
		{Key: "Az9._:/-", Body: body},
		{Key: "Az9._:/-", Body: json.RawMessage(`{"other":true}`)},
		{Key: "second", Body: json.RawMessage(`1`)},
	})
	if err != nil || last != 2 || ticks != putTicks+1 {
		t.Fatalf("append last %d ticks %d err %v", last, ticks, err)
	}
	if len(results) != 3 || results[0].Seq != 1 || results[0].Duplicate || results[1].Seq != 1 || !results[1].Duplicate || results[2].Seq != 2 || results[2].Duplicate {
		t.Fatalf("results %+v", results)
	}
	mirrorPath := filepath.Join(dir, "channels", owner, "chat", "mirror.jsonl")
	text := string(fileBytes(t, mirrorPath))
	first := `{"seq":1,"key":"Az9._:/-","author":"` + owner + `","at":"2026-11-12T09:30:02.000Z","body":{"a":"<b>"}}`
	second := `{"seq":2,"key":"second","author":"` + owner + `","at":"2026-11-12T09:30:02.000Z","body":1}`
	if text != first+"\n"+second+"\n" {
		t.Fatalf("mirror %s", text)
	}
	entries, last, more, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || more || last != 2 || len(entries) != 2 || string(entries[0].Body) != `{"a":"<b>"}` || entries[0].Author != owner {
		t.Fatalf("read %+v more %v last %d err %v", entries, more, last, err)
	}
	entries[0].Body[0] = 'X'
	again, _, _, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || string(again[0].Body) != `{"a":"<b>"}` {
		t.Fatal("body aliased", again, err)
	}
	after, last, more, err := s.Read(owner, "chat", Mirror, 1)
	if err != nil || more || last != 2 || len(after) != 1 || after[0].Seq != 2 {
		t.Fatalf("after %+v %v", after, err)
	}
	none, last, more, err := s.Read(owner, "chat", Mirror, 2)
	if err != nil || more || last != 2 || len(none) != 0 {
		t.Fatalf("tail %+v more %v", none, err)
	}

	// A later call, a different author, and a mixed batch all see the same mirror key.
	dup, last, err := s.Append(owner, "chat", Mirror, other, []NewEntry{{Key: "Az9._:/-", Body: json.RawMessage(`9`)}})
	if err != nil || last != 2 || len(dup) != 1 || dup[0].Seq != 1 || !dup[0].Duplicate {
		t.Fatalf("cross author %+v last %d %v", dup, last, err)
	}
	if string(fileBytes(t, mirrorPath)) != text {
		t.Fatal("duplicate wrote")
	}

	before := fileBytes(t, mirrorPath)
	_, _, err = s.Append(owner, "chat", Mirror, owner, []NewEntry{
		{Key: "third", Body: json.RawMessage(`1`)},
		{Key: "bad key", Body: json.RawMessage(`1`)},
	})
	if !errors.Is(err, ErrInvalid) || !bytes.Equal(before, fileBytes(t, mirrorPath)) {
		t.Fatal(err)
	}
	_, _, err = s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "third", Body: json.RawMessage(`{`)}})
	if !errors.Is(err, ErrInvalid) || !bytes.Equal(before, fileBytes(t, mirrorPath)) {
		t.Fatal("invalid json stored", err)
	}
	_, _, err = s.Append(owner, "chat", Mirror, owner, nil)
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	wide := make([]NewEntry, 257)
	for i := range wide {
		wide[i] = NewEntry{Key: "k", Body: json.RawMessage(`1`)}
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, wide); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Log(9), owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("kind")
	}
	if _, _, err := s.Append(owner, "chat", Mirror, "nope", []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("author")
	}
	longKey := strings.Repeat("a", 200)
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: longKey, Body: json.RawMessage(`null`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: longKey + "a", Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("long key")
	}
	for _, bad := range []string{"", "a b", "a?", "a\\b", "a\n"} {
		if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: bad, Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("key %q %v", bad, err)
		}
	}
	exact := jsonString(maxEntryBody)
	if _, last, err := s.Append(owner, "chat", Comments, owner, []NewEntry{{Key: "fit", Body: exact}}); err != nil || last != 1 {
		t.Fatal(err, last)
	}
	over := jsonString(maxEntryBody + 1)
	comments := fileBytes(t, filepath.Join(dir, "channels", owner, "chat", "comments.jsonl"))
	if _, _, err := s.Append(owner, "chat", Comments, owner, []NewEntry{
		{Key: "ok", Body: json.RawMessage(`true`)},
		{Key: "big", Body: over},
	}); !errors.Is(err, ErrTooLarge) || !bytes.Equal(comments, fileBytes(t, filepath.Join(dir, "channels", owner, "chat", "comments.jsonl"))) {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "missing", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCommentKeysArePerAuthor(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner, a, b := id(0x11), id(0x22), id(0x33)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, last, err := s.Append(owner, "chat", Comments, a, []NewEntry{{Key: "same", Body: json.RawMessage(`{"n":1}`)}}); err != nil || last != 1 {
		t.Fatal(err, last)
	}
	res, last, err := s.Append(owner, "chat", Comments, b, []NewEntry{{Key: "same", Body: json.RawMessage(`{"n":2}`)}})
	if err != nil || last != 2 || res[0].Duplicate || res[0].Seq != 2 {
		t.Fatalf("%+v %d %v", res, last, err)
	}
	res, last, err = s.Append(owner, "chat", Comments, a, []NewEntry{{Key: "same", Body: json.RawMessage(`{"n":3}`)}})
	if err != nil || last != 2 || !res[0].Duplicate || res[0].Seq != 1 {
		t.Fatalf("same author %+v %d %v", res, last, err)
	}
	// The mirror has its own sequence and its own keys.
	if _, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "same", Body: json.RawMessage(`{"n":1}`)}}); err != nil || last != 1 {
		t.Fatal(err, last)
	}
	entries, _, _, err := s.Read(owner, "chat", Comments, 0)
	if err != nil || len(entries) != 2 || entries[0].Author != a || entries[1].Author != b || string(entries[0].Body) != `{"n":1}` {
		t.Fatalf("%+v %v", entries, err)
	}
}

func TestReadHonorsAfterAndTheCaps(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	var batch []NewEntry
	for i := 1; i <= maxReadEntries; i++ {
		batch = append(batch, NewEntry{Key: keyN(i), Body: json.RawMessage(`1`)})
	}
	if _, last, err := s.Append(owner, "chat", Mirror, owner, batch); err != nil || last != uint64(maxReadEntries) {
		t.Fatal(err, last)
	}
	if _, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: keyN(maxReadEntries + 1), Body: json.RawMessage(`1`)}}); err != nil || last != uint64(maxReadEntries)+1 {
		t.Fatal(err, last)
	}
	page, last, more, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || !more || last != uint64(maxReadEntries)+1 || len(page) != maxReadEntries || page[0].Seq != 1 || page[len(page)-1].Seq != uint64(maxReadEntries) {
		t.Fatalf("page %d more %v last %d err %v", len(page), more, last, err)
	}
	rest, last, more, err := s.Read(owner, "chat", Mirror, uint64(maxReadEntries))
	if err != nil || more || last != uint64(maxReadEntries)+1 || len(rest) != 1 || rest[0].Seq != uint64(maxReadEntries)+1 {
		t.Fatalf("rest %+v more %v %v", rest, more, err)
	}

	saveSeams(t)
	readBodyLimit = 1
	small := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	if _, _, err := small.Put(owner, "chat", "feat"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := small.Append(owner, "chat", Mirror, owner, []NewEntry{
		{Key: "a", Body: json.RawMessage(`{"a":1}`)},
		{Key: "b", Body: json.RawMessage(`{"b":1}`)},
	}); err != nil {
		t.Fatal(err)
	}
	one, last, more, err := small.Read(owner, "chat", Mirror, 0)
	if err != nil || !more || len(one) != 1 || one[0].Key != "a" || last != 2 {
		t.Fatalf("at least one %+v more %v last %d %v", one, more, last, err)
	}
	second, _, more, err := small.Read(owner, "chat", Mirror, 1)
	if err != nil || more || len(second) != 1 || second[0].Key != "b" {
		t.Fatalf("second %+v %v", second, err)
	}

	heavy := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(8))})
	if _, _, err := heavy.Put(owner, "chat", "feat"); err != nil {
		t.Fatal(err)
	}
	big := jsonString(maxEntryBody)
	var large []NewEntry
	for i := 1; i <= 4; i++ {
		large = append(large, NewEntry{Key: keyN(i), Body: big})
	}
	if _, _, err := heavy.Append(owner, "chat", Comments, owner, large); err != nil {
		t.Fatal(err)
	}
	if _, _, err := heavy.Append(owner, "chat", Comments, owner, []NewEntry{{Key: "tail", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	readBodyLimit = maxReadBodyBytes
	got, last, more, err := heavy.Read(owner, "chat", Comments, 0)
	if err != nil || !more || len(got) != 4 || last != 5 || len(got[0].Body) != maxEntryBody {
		t.Fatalf("budget %d more %v last %d err %v", len(got), more, last, err)
	}
	tail, _, more, err := heavy.Read(owner, "chat", Comments, 4)
	if err != nil || more || len(tail) != 1 || tail[0].Key != "tail" {
		t.Fatalf("budget tail %+v %v", tail, err)
	}
}

func TestQuotaOnLogsAndChannelCount(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(32)), MaxLogBytes: 1 << 20})
	owner, other := id(0x11), id(0x22)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "a", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "channels", owner, "chat", "mirror.jsonl")
	size := int64(len(fileBytes(t, mirror)))
	s.Close()

	tight := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8)), MaxLogBytes: size * 2})
	dup, last, err := tight.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "a", Body: json.RawMessage(`9`)}})
	if err != nil || last != 1 || !dup[0].Duplicate {
		t.Fatalf("dup at room %+v %v", dup, err)
	}
	before := fileBytes(t, mirror)
	if _, _, err := tight.Append(owner, "chat", Mirror, owner, []NewEntry{
		{Key: "b", Body: json.RawMessage(`1`)},
		{Key: "c", Body: json.RawMessage(`1`)},
	}); !errors.Is(err, ErrQuota) || !bytes.Equal(before, fileBytes(t, mirror)) {
		t.Fatal("batch crossed the limit", err)
	}
	if _, last, err := tight.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "b", Body: json.RawMessage(`1`)}}); err != nil || last != 2 {
		t.Fatal(err, last)
	}
	filled := fileBytes(t, mirror)
	if int64(len(filled)) != size*2 {
		t.Fatalf("size %d want %d", len(filled), size*2)
	}
	if _, _, err := tight.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "c", Body: json.RawMessage(`1`)}}); !errors.Is(err, ErrQuota) || !bytes.Equal(filled, fileBytes(t, mirror)) {
		t.Fatal(err)
	}
	// A duplicate does not need room.
	if _, last, err := tight.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "a", Body: json.RawMessage(`1`)}}); err != nil || last != 2 {
		t.Fatal(err, last)
	}

	limited := openStore(t, t.TempDir(), Options{Entropy: bytes.NewReader(seqBytes(64)), MaxChannelsPerOwner: 2})
	if _, created, err := limited.Put(owner, "c1", "a"); err != nil || !created {
		t.Fatal(err)
	}
	if _, _, err := limited.Put(other, "c1", "b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.Put(owner, "c2", "c"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.Put(owner, "c3", "d"); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	if _, created, err := limited.Put(owner, "c1", "renamed"); err != nil || created {
		t.Fatal(err, created)
	}
	if err := limited.Delete(owner, "c2"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := limited.Put(owner, "c3", "d"); err != nil || !created {
		t.Fatal(err, created)
	}
}

func TestReopenReturnsTheSameLog(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, err := s.SetMembers(owner, "chat", []Member{{Seat: id(0x22), Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"a":1}`)}}); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, "channels", owner, "chat", "mirror.jsonl")
	f, err := os.OpenFile(torn, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("TORN"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.Close()

	s = openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	entries, last, more, err := s.Read(owner, "chat", Mirror, 0)
	if err != nil || more || last != 1 || len(entries) != 1 || entries[0].Key != "k" || string(entries[0].Body) != `{"a":1}` {
		t.Fatalf("reopen %+v %v", entries, err)
	}
	res, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`{"a":2}`)}})
	if err != nil || last != 1 || !res[0].Duplicate {
		t.Fatalf("dup after reopen %+v %v", res, err)
	}
	if _, last, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "next", Body: json.RawMessage(`2`)}}); err != nil || last != 2 {
		t.Fatal(err, last)
	}
	if bytes.Contains(fileBytes(t, torn), []byte("TORN")) {
		t.Fatal("torn tail survived")
	}
	entries, _, _, err = s.Read(owner, "chat", Mirror, 0)
	if err != nil || len(entries) != 2 || entries[1].Seq != 2 {
		t.Fatalf("after torn %+v %v", entries, err)
	}

	// A second store sees lines the first store already indexed.
	other := openStore(t, dir, Options{})
	if _, _, err := other.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "from-other", Body: json.RawMessage(`3`)}}); err != nil {
		t.Fatal(err)
	}
	entries, last, _, err = s.Read(owner, "chat", Mirror, 0)
	if err != nil || last != 3 || len(entries) != 3 || entries[2].Key != "from-other" {
		t.Fatalf("shared %+v %v", entries, err)
	}
}

func TestIndexIsCached(t *testing.T) {
	saveSeams(t)
	var opens []string
	real := openJournal
	openJournal = func(path string) (*journal.File, error) {
		opens = append(opens, path)
		return real(path)
	}
	dir := t.TempDir()
	s := openStore(t, dir, Options{Entropy: bytes.NewReader(seqBytes(8))})
	owner := id(0x11)
	if _, created, err := s.Put(owner, "chat", "feat"); err != nil || !created {
		t.Fatal(err, created)
	}
	if _, _, err := s.Append(owner, "chat", Mirror, owner, []NewEntry{{Key: "k", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Read(owner, "chat", Mirror, 0); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "channels", owner, "chat", mirrorName)
	comments := filepath.Join(dir, "channels", owner, "chat", commentsName)
	if countPath(opens, mirror) != 1 || countPath(opens, comments) != 1 {
		t.Fatalf("opens %v", opens)
	}
}
