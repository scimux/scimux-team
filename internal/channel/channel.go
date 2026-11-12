// Package channel stores shared chats and their two append-only logs.
package channel

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/scimux/scimux-team/internal/journal"
)

const (
	maxEntryBody       = 4 << 20
	maxReadEntries     = 256
	maxReadBodyBytes   = 16 << 20
	defaultMaxLogBytes = 256 << 20
	defaultMaxChannels = 1000
	maxMetaBytes       = 64 << 10

	metaName     = "channel.json"
	mirrorName   = "mirror.jsonl"
	commentsName = "comments.jsonl"
	timeLayout   = "2006-01-02T15:04:05.000Z"
)

var readBodyLimit = maxReadBodyBytes

var (
	mkdirAll        = os.MkdirAll
	chmodPath       = os.Chmod
	readDir         = os.ReadDir
	removeAll       = os.RemoveAll
	openMeta        = os.Open
	createTemp      = os.CreateTemp
	chmodFile       = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
	writeFile       = func(f *os.File, p []byte) (int, error) { return f.Write(p) }
	syncFile        = func(f *os.File) error { return f.Sync() }
	closeFile       = func(f *os.File) error { return f.Close() }
	renameFile      = os.Rename
	openJournal     = journal.Open
	marshalValue    = func(any) ([]byte, error) { return nil, nil }
	readFull        = io.ReadAll
	syncDirFile     = func(f *os.File) error { return f.Sync() }
	beforeLogUpdate func()
	syncDir         = func(string) error { return nil }
)

var (
	ErrNotFound = errors.New("channel not found")
	ErrInvalid  = errors.New("invalid channel")
	ErrTooLarge = errors.New("channel entry too large")
	ErrQuota    = errors.New("channel quota")
	errClosed   = errors.New("channel: closed")
	errDamaged  = errors.New("channel: damaged record")
)

type Options struct {
	MaxLogBytes         int64
	MaxChannelsPerOwner int
	Now                 func() time.Time
	Entropy             io.Reader
}

type Member struct {
	Seat   string `json:"seat"`
	Access string `json:"access"`
}

type Info struct {
	Owner, Chat, Title, Generation string
	Created                        time.Time
	Members                        []Member
	MirrorLast, CommentsLast       uint64
}

type Log int

const (
	Mirror Log = iota
	Comments
)

type NewEntry struct {
	Key  string          `json:"key"`
	Body json.RawMessage `json:"body"`
}

type Result struct {
	Key       string `json:"key"`
	Seq       uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate"`
}

type Entry struct {
	Seq    uint64          `json:"seq"`
	Key    string          `json:"key"`
	Author string          `json:"author"`
	At     string          `json:"at"`
	Body   json.RawMessage `json:"body"`
}

type Store struct {
	opts Options
}

func Open(string, Options) (*Store, error) { return &Store{}, nil }

func (s *Store) Close() error { return nil }

func (s *Store) Put(string, string, string) (Info, bool, error) { return Info{}, false, nil }

func (s *Store) Get(string, string) (Info, error) { return Info{}, nil }

func (s *Store) List() ([]Info, error) { return nil, nil }

func (s *Store) Delete(string, string) error { return nil }

func (s *Store) SetMembers(string, string, []Member) (Info, error) { return Info{}, nil }

func (s *Store) Append(string, string, Log, string, []NewEntry) ([]Result, uint64, error) {
	return nil, 0, nil
}

func (s *Store) Read(string, string, Log, uint64) ([]Entry, uint64, bool, error) {
	return nil, 0, false, nil
}

func (s *Store) Reconcile(func(string) bool) error { return nil }

func formatTime(time.Time) string { return "" }
