// Package channel stores shared chats and their two append-only logs.
//
// Only the server process uses it. Owner, chat, title, key, member and body
// rules are the team protocol's. A violation is ErrInvalid. An entry body
// whose compact JSON exceeds 4 MiB is ErrTooLarge. A channel count or a log
// file past its limit is ErrQuota.
//
// channel.json is the only file that is ever rewritten, and only by writing
// a temporary file in the same directory and renaming it into place. The
// logs are journals: a torn tail is discarded on the next append, and a
// damaged complete line is a hard error for that channel alone. List leaves
// a damaged channel out and loads logs only for channels the seat owns or
// belongs to. The index of a log keeps no entry body. A line that changes
// after it was indexed fails a read of that channel. Reconcile still
// removes a channel whose owner is not active, and leaves a damaged
// channel of an active owner in place without reporting an error.
package channel

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/scimux/scimux-team/internal/journal"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600

	// Limits sized from real session logs. They are constants, not flags.
	maxEntryBody       = 4 << 20
	maxReadEntries     = 256
	maxReadBodyBytes   = 16 << 20
	defaultMaxLogBytes = 256 << 20
	defaultMaxChannels = 1000
	maxMembers         = 200
	maxTitleBytes      = 200
	maxKeyBytes        = 200
	maxChatBytes       = 64
	maxMetaBytes       = 64 << 10
	maxBatch           = 256

	metaName     = "channel.json"
	mirrorName   = "mirror.jsonl"
	commentsName = "comments.jsonl"
	timeLayout   = "2006-01-02T15:04:05.000Z"
)

// readBodyLimit is maxReadBodyBytes. A test lowers it to force the
// at-least-one read rule; production leaves the constant.
var readBodyLimit = maxReadBodyBytes

// Filesystem seams. Tests replace one to force a failure a healthy
// directory will not produce, including when the test runs as root.
var (
	mkdirAll      = os.MkdirAll
	chmodPath     = os.Chmod
	readDir       = os.ReadDir
	removeAll     = os.RemoveAll
	openMeta      = os.Open
	createTemp    = os.CreateTemp
	chmodFile     = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
	writeFile     = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	syncFile      = func(f *os.File) error { return f.Sync() }
	closeFile     = func(f *os.File) error { return f.Close() }
	renameFile    = os.Rename
	openJournal   = journal.Open
	openJournalAt = journal.OpenAt
	closeJournal  = func(f *journal.File) error { return f.Close() }
	readLogLine   = func(f *journal.File, offset int64) ([]byte, error) { return f.ReadLine(offset) }
	marshalValue  = func(v any) ([]byte, error) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
	}
	readFull = io.ReadAll
)

// syncDirFile is the directory sync inside syncDir. A test replaces it to
// force a failure a local directory will not produce.
var syncDirFile = func(f *os.File) error { return f.Sync() }

// beforeLogUpdate, when set, runs after a log has been refreshed and
// before its exclusive update. Production leaves it nil. A test appends a
// line in that window, which is otherwise a race with another process.
var beforeLogUpdate func()

// syncDir pushes a directory entry to disk after a rename. The rename is
// the commit; a failure here is ignored because the new name is already
// visible.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncDirFile(d)
}

var (
	// ErrNotFound is a channel the store does not have.
	ErrNotFound = errors.New("channel not found")

	// ErrInvalid is an owner, chat, title, key, member or body the
	// protocol rejects, or an empty or oversized batch.
	ErrInvalid = errors.New("invalid channel")

	// ErrTooLarge is an entry body whose compact form exceeds 4 MiB.
	ErrTooLarge = errors.New("channel entry too large")

	// ErrQuota is too many channels for one owner, or a log that would
	// pass its byte limit.
	ErrQuota = errors.New("channel quota")

	errClosed  = errors.New("channel: closed")
	errDamaged = errors.New("channel: damaged record")
)

// Options configures Open. Zero values select the defaults.
type Options struct {
	// MaxLogBytes is the most bytes in one log file. Zero means 256 MiB.
	MaxLogBytes int64

	// MaxChannelsPerOwner is the most channels one seat may own. Zero means 1000.
	MaxChannelsPerOwner int

	// Now supplies the clock. Nil means time.Now.
	Now func() time.Time

	// Entropy supplies a channel generation. Nil means crypto/rand.
	Entropy io.Reader
}

// Member is one seat allowed to read a channel or to write comments.
type Member struct {
	Seat   string `json:"seat"`
	Access string `json:"access"`
}

// Info is one channel. Members is a copy, ordered by seat.
type Info struct {
	Owner, Chat, Title, Generation string
	Created                        time.Time
	Members                        []Member
	MirrorLast, CommentsLast       uint64
}

// Log selects one of a channel's two journals.
type Log int

const (
	// Mirror is the owner's chat record.
	Mirror Log = iota
	// Comments are what members wrote.
	Comments
)

// NewEntry is one record a caller wants to append. Body is any JSON value.
type NewEntry struct {
	Key  string          `json:"key"`
	Body json.RawMessage `json:"body"`
}

// Result is the sequence number of one submitted key.
type Result struct {
	Key       string `json:"key"`
	Seq       uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate"`
}

// Entry is one stored record. Body is compact JSON.
type Entry struct {
	Seq    uint64          `json:"seq"`
	Key    string          `json:"key"`
	Author string          `json:"author"`
	At     string          `json:"at"`
	Body   json.RawMessage `json:"body"`
}

type metaFile struct {
	Title      string   `json:"title"`
	Generation string   `json:"generation"`
	Created    string   `json:"created"`
	Members    []Member `json:"members"`
}

type chat struct {
	owner    string
	name     string
	dir      string
	meta     *metaFile
	bad      error
	mirror   *logState
	comments *logState
}

// Store is the channels tree under one state directory.
type Store struct {
	root    string
	opts    Options
	mu      sync.Mutex
	byKey   map[string]*chat
	scanned bool
	closed  bool
}

// Open prepares the channels directory inside stateDir. The directory must
// already exist. A missing channels directory is created mode 0700.
func Open(stateDir string, opts Options) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("channel: empty state directory")
	}
	info, err := os.Lstat(stateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("channel: state path is not a directory")
	}
	if opts.MaxLogBytes < 0 || opts.MaxChannelsPerOwner < 0 {
		return nil, ErrInvalid
	}
	if opts.MaxLogBytes == 0 {
		opts.MaxLogBytes = defaultMaxLogBytes
	}
	if opts.MaxChannelsPerOwner == 0 {
		opts.MaxChannelsPerOwner = defaultMaxChannels
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Entropy == nil {
		opts.Entropy = rand.Reader
	}
	root := filepath.Join(stateDir, "channels")
	if err := mkdirAll(root, dirPerm); err != nil {
		return nil, err
	}
	if err := chmodPath(root, dirPerm); err != nil {
		return nil, err
	}
	return &Store{
		root:  root,
		opts:  opts,
		byKey: map[string]*chat{},
	}, nil
}

// Close releases open logs. A second call, and a call on a nil store, is a no-op.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, ch := range s.byKey {
		ch.closeLogs()
	}
	return nil
}

// Put creates a channel or replaces its title. The generation and the
// creation time stay on an update. created is false when the channel
// already existed.
func (s *Store) Put(owner, chatID, title string) (Info, bool, error) {
	if !validOwner(owner) || !validChat(chatID) || !validTitle(title) {
		return Info{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Info{}, false, err
	}
	if err := s.scan(); err != nil {
		return Info{}, false, err
	}
	if ch := s.byKey[chatKey(owner, chatID)]; ch != nil {
		if _, err := s.loaded(ch); err != nil {
			return Info{}, false, err
		}
		m := ch.meta
		next := *m
		next.Title = title
		next.Members = append([]Member{}, m.Members...)
		if err := s.saveMeta(ch, &next); err != nil {
			return Info{}, false, err
		}
		return s.info(ch), false, nil
	}
	if s.countOwner(owner) >= s.opts.MaxChannelsPerOwner {
		return Info{}, false, ErrQuota
	}
	gen, err := randomHex(s.opts.Entropy, 8)
	if err != nil {
		return Info{}, false, err
	}
	ch := newChat(s.root, owner, chatID)
	if err := s.createChat(ch, &metaFile{
		Title:      title,
		Generation: gen,
		Created:    formatTime(s.opts.Now()),
		Members:    []Member{},
	}); err != nil {
		return Info{}, false, err
	}
	s.byKey[chatKey(owner, chatID)] = ch
	return s.info(ch), true, nil
}

// Get returns one channel.
func (s *Store) Get(owner, chatID string) (Info, error) {
	if !validOwner(owner) || !validChat(chatID) {
		return Info{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Info{}, err
	}
	if err := s.scan(); err != nil {
		return Info{}, err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return Info{}, err
	}
	return s.loaded(ch)
}

// List returns the channels seat owns or belongs to, ordered by owner and
// then chat. It loads the logs of those channels only. A damaged channel
// is left out. Any other failure to read one is returned.
func (s *Store) List(seat string) ([]Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.byKey))
	for k := range s.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Info, 0, len(keys))
	for _, k := range keys {
		ch := s.byKey[k]
		m, err := s.loadMeta(ch)
		if errors.Is(err, errDamaged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !visibleTo(seat, ch, m) {
			continue
		}
		info, err := s.loaded(ch)
		if errors.Is(err, errDamaged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// visibleTo reports whether seat owns the channel or is one of its members.
func visibleTo(seat string, ch *chat, m *metaFile) bool {
	if seat == ch.owner {
		return true
	}
	for _, mem := range m.Members {
		if mem.Seat == seat {
			return true
		}
	}
	return false
}

// Delete removes the channel directory. A later Put starts a new generation.
func (s *Store) Delete(owner, chatID string) error {
	if !validOwner(owner) || !validChat(chatID) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if err := s.scan(); err != nil {
		return err
	}
	key := chatKey(owner, chatID)
	ch := s.byKey[key]
	if ch == nil {
		return ErrNotFound
	}
	return s.deleteChat(key, ch)
}

// SetMembers replaces the member list. It sorts by seat. It does not know
// which seats are registered.
func (s *Store) SetMembers(owner, chatID string, members []Member) (Info, error) {
	if !validOwner(owner) || !validChat(chatID) {
		return Info{}, ErrInvalid
	}
	sorted, err := normalizeMembers(owner, members)
	if err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Info{}, err
	}
	if err := s.scan(); err != nil {
		return Info{}, err
	}
	ch := s.byKey[chatKey(owner, chatID)]
	if ch == nil {
		return Info{}, ErrNotFound
	}
	if _, err := s.loaded(ch); err != nil {
		return Info{}, err
	}
	m := ch.meta
	next := *m
	next.Members = sorted
	if err := s.saveMeta(ch, &next); err != nil {
		return Info{}, err
	}
	return s.info(ch), nil
}

// Reconcile removes every channel whose owner is not active and removes
// every member that is not active. It is the only clean-up operation.
func (s *Store) Reconcile(active func(id string) bool) error {
	if active == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if err := s.scan(); err != nil {
		return err
	}
	keys := make([]string, 0, len(s.byKey))
	for k := range s.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var first error
	for _, key := range keys {
		ch := s.byKey[key]
		if !active(ch.owner) {
			remember(&first, s.deleteChat(key, ch))
			continue
		}
		m, err := s.loadMeta(ch)
		if err != nil {
			remember(&first, err)
			continue
		}
		filtered := make([]Member, 0, len(m.Members))
		changed := false
		for _, mem := range m.Members {
			if active(mem.Seat) {
				filtered = append(filtered, mem)
			} else {
				changed = true
			}
		}
		if !changed {
			continue
		}
		next := *m
		next.Members = filtered
		remember(&first, s.saveMeta(ch, &next))
	}
	return first
}

// remember keeps the first error that is not a damaged record. Damage is
// local to that channel: an inactive owner's damaged channel is removed by
// the caller before this is used, and an active owner's damaged channel
// stays where it is.
func remember(first *error, err error) {
	if err == nil || errors.Is(err, errDamaged) {
		return
	}
	if *first == nil {
		*first = err
	}
}

func (s *Store) createChat(ch *chat, m *metaFile) error {
	if err := mkdirAll(ch.dir, dirPerm); err != nil {
		return err
	}
	for _, dir := range []string{s.root, filepath.Dir(ch.dir), ch.dir} {
		if err := chmodPath(dir, dirPerm); err != nil {
			s.discardNew(ch)
			return err
		}
	}
	if err := s.saveMeta(ch, m); err != nil {
		s.discardNew(ch)
		return err
	}
	if err := ch.mirror.create(); err != nil {
		s.discardNew(ch)
		return err
	}
	if err := ch.comments.create(); err != nil {
		s.discardNew(ch)
		return err
	}
	return nil
}

func (s *Store) discardNew(ch *chat) {
	ch.closeLogs()
	_ = removeAll(ch.dir)
}

func (s *Store) deleteChat(key string, ch *chat) error {
	ch.closeLogs()
	if err := removeAll(ch.dir); err != nil {
		return err
	}
	delete(s.byKey, key)
	return nil
}

func (s *Store) saveMeta(ch *chat, m *metaFile) error {
	if m.Members == nil {
		m.Members = []Member{}
	}
	data, err := marshalValue(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeAtomic(ch.dir, metaName, data); err != nil {
		return err
	}
	copied := *m
	copied.Members = append([]Member{}, m.Members...)
	ch.meta = &copied
	ch.bad = nil
	return nil
}

func (s *Store) loadMeta(ch *chat) (*metaFile, error) {
	if ch.bad != nil {
		return nil, ch.bad
	}
	if ch.meta != nil {
		return ch.meta, nil
	}
	m, err := readMeta(filepath.Join(ch.dir, metaName))
	if err != nil {
		if errors.Is(err, errDamaged) {
			ch.bad = err
		}
		return nil, err
	}
	ch.meta = m
	return m, nil
}

func (s *Store) loaded(ch *chat) (Info, error) {
	if _, err := s.loadMeta(ch); err != nil {
		return Info{}, err
	}
	if err := s.ensureLog(ch.mirror); err != nil {
		return Info{}, err
	}
	if err := s.ensureLog(ch.comments); err != nil {
		return Info{}, err
	}
	return s.info(ch), nil
}

func (s *Store) info(ch *chat) Info {
	m := ch.meta
	created, _ := parseStamp(m.Created)
	members := append([]Member{}, m.Members...)
	return Info{
		Owner:        ch.owner,
		Chat:         ch.name,
		Title:        m.Title,
		Generation:   m.Generation,
		Created:      created,
		Members:      members,
		MirrorLast:   ch.mirror.last,
		CommentsLast: ch.comments.last,
	}
}

func (s *Store) scan() error {
	if s.scanned {
		return nil
	}
	owners, err := readDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range owners {
		if !e.IsDir() || !validOwner(e.Name()) {
			continue
		}
		owner := e.Name()
		kids, err := readDir(filepath.Join(s.root, owner))
		if err != nil {
			return err
		}
		for _, k := range kids {
			if !k.IsDir() || !validChat(k.Name()) {
				continue
			}
			id := k.Name()
			s.byKey[chatKey(owner, id)] = newChat(s.root, owner, id)
		}
	}
	s.scanned = true
	return nil
}

func (s *Store) countOwner(owner string) int {
	n := 0
	prefix := owner + "/"
	for k := range s.byKey {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func (s *Store) usable() error {
	if s.closed {
		return errClosed
	}
	return nil
}

func (s *Store) chatFor(owner, chatID string) (*chat, error) {
	if err := s.scan(); err != nil {
		return nil, err
	}
	ch := s.byKey[chatKey(owner, chatID)]
	if ch == nil {
		return nil, ErrNotFound
	}
	if _, err := s.loadMeta(ch); err != nil {
		return nil, err
	}
	return ch, nil
}

func newChat(root, owner, chatID string) *chat {
	dir := filepath.Join(root, owner, chatID)
	return &chat{
		owner:    owner,
		name:     chatID,
		dir:      dir,
		mirror:   newLog(filepath.Join(dir, mirrorName), Mirror),
		comments: newLog(filepath.Join(dir, commentsName), Comments),
	}
}

func (ch *chat) closeLogs() {
	ch.mirror.close()
	ch.comments.close()
}

func (ch *chat) log(kind Log) (*logState, error) {
	if kind == Mirror {
		return ch.mirror, nil
	}
	if kind == Comments {
		return ch.comments, nil
	}
	return nil, ErrInvalid
}

func chatKey(owner, chatID string) string { return owner + "/" + chatID }

func readMeta(path string) (*metaFile, error) {
	f, err := openMeta(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errDamaged
		}
		return nil, err
	}
	defer f.Close()
	data, err := readFull(io.LimitReader(f, maxMetaBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxMetaBytes {
		return nil, errDamaged
	}
	return parseMeta(data)
}

func parseMeta(data []byte) (*metaFile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m metaFile
	if err := dec.Decode(&m); err != nil {
		return nil, errDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errDamaged
	}
	if !validGeneration(m.Generation) || !validTitle(m.Title) {
		return nil, errDamaged
	}
	if _, err := parseStamp(m.Created); err != nil {
		return nil, errDamaged
	}
	if err := checkStoredMembers(m.Members); err != nil {
		return nil, errDamaged
	}
	if m.Members == nil {
		m.Members = []Member{}
	}
	return &m, nil
}

func writeAtomic(dir, name string, data []byte) error {
	f, err := createTemp(dir, ".channel-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tmp)
	}()
	if err := chmodFile(f, filePerm); err != nil {
		return err
	}
	if err := writeFull(f, data); err != nil {
		return err
	}
	if err := syncFile(f); err != nil {
		return err
	}
	if err := closeFile(f); err != nil {
		return err
	}
	closed = true
	if err := renameFile(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	_ = syncDir(dir)
	return nil
}

func writeFull(f *os.File, data []byte) error {
	n, err := writeFile(f, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func normalizeMembers(owner string, members []Member) ([]Member, error) {
	if len(members) > maxMembers {
		return nil, ErrInvalid
	}
	out := append([]Member(nil), members...)
	seen := map[string]bool{}
	for _, m := range out {
		if !validOwner(m.Seat) || m.Seat == owner || (m.Access != "read" && m.Access != "write") {
			return nil, ErrInvalid
		}
		if seen[m.Seat] {
			return nil, ErrInvalid
		}
		seen[m.Seat] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seat < out[j].Seat })
	return out, nil
}

func checkStoredMembers(members []Member) error {
	seen := map[string]bool{}
	for _, m := range members {
		if !validOwner(m.Seat) || (m.Access != "read" && m.Access != "write") || seen[m.Seat] {
			return errDamaged
		}
		seen[m.Seat] = true
	}
	return nil
}

func validOwner(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !hexDigit(s[i]) {
			return false
		}
	}
	return true
}

func validGeneration(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !hexDigit(s[i]) {
			return false
		}
	}
	return true
}

func hexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func validChat(s string) bool {
	if len(s) < 1 || len(s) > maxChatBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func validTitle(s string) bool {
	if len(s) > maxTitleBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validKey(s string) bool {
	if len(s) < 1 || len(s) > maxKeyBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return true
}

func formatTime(t time.Time) string {
	return time.UnixMilli(t.UTC().UnixMilli()).UTC().Format(timeLayout)
}

func parseStamp(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func randomHex(r io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
