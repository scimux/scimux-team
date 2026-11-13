package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"

	"github.com/scimux/scimux-team/internal/journal"
)

// logState is one channel log. The index is built on first use and then
// extended by Refresh, so a line is parsed once. It keeps no entry body.
// Read loads the line from the file.
type logState struct {
	path  string
	kind  Log
	j     *journal.File
	idx   []indexEntry
	byKey map[string]uint64
	last  uint64
	size  int64
	bad   error
}

// indexEntry is one indexed line. bodyN is the stored body length, which
// is what the read cap counts. sum is the line without its newline, so a
// later change fails the read.
type indexEntry struct {
	seq   uint64
	off   int64
	bodyN int
	sum   uint64
}

func newLog(path string, kind Log) *logState {
	return &logState{path: path, kind: kind, byKey: map[string]uint64{}}
}

func (lg *logState) create() error {
	j, err := openJournal(lg.path)
	if err != nil {
		return err
	}
	lg.j = j
	return nil
}

func (lg *logState) close() {
	if lg.j != nil {
		_ = lg.j.Close()
		lg.j = nil
	}
	lg.idx = nil
	lg.byKey = map[string]uint64{}
	lg.last = 0
	lg.size = 0
	lg.bad = nil
}

// Append validates every entry, then checks the log limit, then appends
// the new lines in one journal update. A key already stored, or repeated
// in the batch, returns the existing sequence with Duplicate set.
// Mirror keys are unique per channel. Comment keys are unique per author.
func (s *Store) Append(owner, chatID string, kind Log, author string, entries []NewEntry) ([]Result, uint64, error) {
	if !validOwner(owner) || !validChat(chatID) || !validOwner(author) {
		return nil, 0, ErrInvalid
	}
	prepared, err := prepareBodies(entries)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, 0, err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return nil, 0, err
	}
	lg, err := ch.log(kind)
	if err != nil {
		return nil, 0, err
	}
	if err := s.ensureLog(lg); err != nil {
		return nil, 0, err
	}
	if beforeLogUpdate != nil {
		beforeLogUpdate()
	}
	var results []Result
	err = lg.j.Update(func(off int64, line []byte) error {
		return lg.absorb(off, line)
	}, func() ([][]byte, error) {
		lines, res, err := lg.prepare(author, prepared, formatTime(s.opts.Now()), s.opts.MaxLogBytes)
		if err != nil {
			return nil, err
		}
		results = res
		return lines, nil
	})
	if err != nil {
		if errors.Is(err, errDamaged) {
			lg.bad = errDamaged
		}
		return nil, 0, err
	}
	return results, lg.last, nil
}

// Read returns the entries with a sequence number greater than after, in
// order. It returns at most 256 entries and at most 16 MiB of bodies, and
// at least one entry when one exists. more is true when later entries were
// left for a following call. last is the highest sequence number in the log.
func (s *Store) Read(owner, chatID string, kind Log, after uint64) ([]Entry, uint64, bool, error) {
	if !validOwner(owner) || !validChat(chatID) {
		return nil, 0, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, 0, false, err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return nil, 0, false, err
	}
	lg, err := ch.log(kind)
	if err != nil {
		return nil, 0, false, err
	}
	if err := s.ensureLog(lg); err != nil {
		return nil, 0, false, err
	}
	out := []Entry{}
	sum := 0
	more := false
	for _, ix := range lg.idx {
		if ix.seq <= after {
			continue
		}
		if len(out) == maxReadEntries {
			more = true
			break
		}
		if len(out) > 0 && sum+ix.bodyN > readBodyLimit {
			more = true
			break
		}
		line, err := lg.j.ReadLine(ix.off)
		if err != nil {
			return nil, 0, false, err
		}
		e, parseErr := parseEntry(line)
		if parseErr != nil || lineSum(line) != ix.sum || e.Seq != ix.seq || len(e.Body) != ix.bodyN {
			lg.bad = errDamaged
			return nil, 0, false, errDamaged
		}
		out = append(out, Entry{
			Seq:    e.Seq,
			Key:    e.Key,
			Author: e.Author,
			At:     e.At,
			Body:   append(json.RawMessage(nil), e.Body...),
		})
		sum += ix.bodyN
	}
	return out, lg.last, more, nil
}

func (s *Store) ensureLog(lg *logState) error {
	if lg.bad != nil {
		return lg.bad
	}
	if lg.j == nil {
		j, err := openJournal(lg.path)
		if err != nil {
			return err
		}
		lg.j = j
	}
	err := lg.j.Refresh(func(off int64, line []byte) error {
		return lg.absorb(off, line)
	})
	if err != nil {
		if errors.Is(err, errDamaged) {
			lg.bad = errDamaged
			return lg.bad
		}
		return err
	}
	return nil
}

func (lg *logState) absorb(off int64, line []byte) error {
	e, err := parseEntry(line)
	if err != nil {
		return errDamaged
	}
	if e.Seq != lg.last+1 {
		return errDamaged
	}
	k := lg.mapKey(e.Author, e.Key)
	if _, ok := lg.byKey[k]; ok {
		return errDamaged
	}
	lg.byKey[k] = e.Seq
	lg.idx = append(lg.idx, indexEntry{
		seq:   e.Seq,
		off:   off,
		bodyN: len(e.Body),
		sum:   lineSum(line),
	})
	lg.last = e.Seq
	lg.size = off + int64(len(line)) + 1
	return nil
}

func lineSum(line []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(line)
	return h.Sum64()
}

func (lg *logState) prepare(author string, entries []NewEntry, at string, maxBytes int64) ([][]byte, []Result, error) {
	results := make([]Result, len(entries))
	var lines [][]byte
	pending := map[string]uint64{}
	var add int64
	for i, e := range entries {
		k := lg.mapKey(author, e.Key)
		if seq, ok := lg.byKey[k]; ok {
			results[i] = Result{Key: e.Key, Seq: seq, Duplicate: true}
			continue
		}
		if seq, ok := pending[k]; ok {
			results[i] = Result{Key: e.Key, Seq: seq, Duplicate: true}
			continue
		}
		seq := lg.last + uint64(len(lines)) + 1
		line, err := marshalValue(Entry{
			Seq:    seq,
			Key:    e.Key,
			Author: author,
			At:     at,
			Body:   e.Body,
		})
		if err != nil {
			return nil, nil, err
		}
		lines = append(lines, line)
		pending[k] = seq
		results[i] = Result{Key: e.Key, Seq: seq, Duplicate: false}
		add += int64(len(line)) + 1
	}
	if lg.size+add > maxBytes {
		return nil, nil, ErrQuota
	}
	return lines, results, nil
}

func (lg *logState) mapKey(author, key string) string {
	if lg.kind == Comments {
		return author + "\x00" + key
	}
	return key
}

func prepareBodies(entries []NewEntry) ([]NewEntry, error) {
	if len(entries) < 1 || len(entries) > maxBatch {
		return nil, ErrInvalid
	}
	out := make([]NewEntry, len(entries))
	for i, e := range entries {
		if !validKey(e.Key) {
			return nil, ErrInvalid
		}
		body, err := compactBody(e.Body)
		if err != nil {
			return nil, err
		}
		out[i] = NewEntry{Key: e.Key, Body: body}
	}
	return out, nil
}

func compactBody(body json.RawMessage) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, ErrInvalid
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return nil, ErrInvalid
	}
	if buf.Len() > maxEntryBody {
		return nil, ErrTooLarge
	}
	return append(json.RawMessage(nil), buf.Bytes()...), nil
}

func parseEntry(line []byte) (Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return Entry{}, errDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Entry{}, errDamaged
	}
	if e.Seq == 0 || !validKey(e.Key) || !validOwner(e.Author) {
		return Entry{}, errDamaged
	}
	if _, err := parseStamp(e.At); err != nil {
		return Entry{}, errDamaged
	}
	if len(e.Body) == 0 || !json.Valid(e.Body) {
		return Entry{}, errDamaged
	}
	return e, nil
}
