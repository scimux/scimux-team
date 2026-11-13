// Package registry stores the team server's identity, join codes and seats
// in one append-only journal, registry.jsonl, inside the state directory.
//
// A code is issued, then bound to exactly one installation, then possibly
// revoked. The journal stores the verifier of a join code and the SHA-256
// of a token, never the plaintext. Operator commands and the running server
// share the directory. Replay is strict: a line that does not follow from
// the records before it is a hard error and is never skipped.
package registry

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/scimux/scimux-team/internal/invite"
	"github.com/scimux/scimux-team/internal/journal"
)

const (
	journalName     = "registry.jsonl"
	defaultName     = "scimux team"
	defaultMaxSeats = 200
	dirPerm         = 0o700
)

// Options configures Open. Zero values select the defaults.
type Options struct {
	// Name is the server name written when the journal is empty. Once a
	// server record exists, later opens keep that name.
	Name string

	// MaxSeats is the maximum number of active seats. Zero means 200.
	MaxSeats int

	// Now supplies the clock. Nil means time.Now.
	Now func() time.Time

	// Entropy supplies server ids, join codes and tokens. Nil means crypto/rand.
	Entropy io.Reader
}

// Server is the identity written on the first open of a state directory.
type Server struct {
	ID   string
	Name string
}

// Code is one join code. It never carries the plaintext.
type Code struct {
	ID      string
	State   string // "unused", "redeemed", "revoked", "expired"
	Issued  time.Time
	Expires time.Time
	Seat    string // installation ID once redeemed, else ""
	Name    string // seat name once redeemed, else ""
}

// Seat is one active seat: a code in state redeemed.
type Seat struct {
	ID         string
	Name       string
	Registered time.Time
}

// Status counts codes by their current state.
type Status struct {
	Codes    int
	Unused   int
	Redeemed int
	Revoked  int
	Expired  int
}

var (
	// ErrJoinRefused is an unknown, used, expired or revoked join code.
	ErrJoinRefused = errors.New("join refused")

	// ErrAlreadyRegistered is an installation ID that is already an active seat.
	ErrAlreadyRegistered = errors.New("already registered")

	// ErrFull means the active-seat limit is reached.
	ErrFull = errors.New("seat limit reached")

	// ErrInvalid is a name or an installation ID the protocol rejects.
	ErrInvalid = errors.New("invalid registration")

	// ErrUnknown is a revoke key that names neither a code nor an active seat.
	ErrUnknown = errors.New("unknown")

	errMalformed   = errors.New("registry: malformed record")
	errUnknownType = errors.New("registry: unknown record type")
	errSecond      = errors.New("registry: second server record")
	errDuplicate   = errors.New("registry: duplicate issue")
	errIssueID     = errors.New("registry: issue id does not match verifier")
	errNotUnused   = errors.New("registry: code is not unused")
	errNoCode      = errors.New("registry: unknown code")
	errActive      = errors.New("registry: installation is active")
	errBefore      = errors.New("registry: record before server")
	errCollision   = errors.New("registry: invite collision")
	errClosed      = errors.New("registry: closed")
	errTrailing    = errors.New("registry: trailing data")
	errEmpty       = errors.New("registry: empty line")
	errNegative    = errors.New("registry: negative seat limit")
	errStateDir    = errors.New("registry: empty state directory")
)

// marshalRec is the encoding seam. Tests replace it to force a failure a
// valid record will not produce.
var marshalRec = json.Marshal

// Registry is the server's view of one state directory.
type Registry struct {
	mu       sync.Mutex
	dir      string
	path     string
	j        *journal.File
	now      func() time.Time
	entropy  io.Reader
	maxSeats int

	serverID   string
	serverName string

	order        []*codeRec
	byID         map[string]*codeRec
	byVerifier   map[string]*codeRec
	activeBySeat map[string]*codeRec
	byToken      map[string]*codeRec

	// collect is set while Refresh or Register wants the installation IDs
	// of revokes applied during that call.
	collect bool
	pending []string
}

type codeRec struct {
	id         string
	verifier   string
	issued     time.Time
	expires    time.Time
	revoked    bool
	seat       string
	name       string
	registered time.Time
	tokenHash  string
}

type record struct {
	Type     string `json:"type"`
	TS       string `json:"ts"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Verifier string `json:"verifier,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Seat     string `json:"seat,omitempty"`
	TokenSHA string `json:"token_sha256,omitempty"`
}

// Open creates a missing state directory with mode 0700, including parents,
// and opens registry.jsonl. An existing path is used only when Lstat shows
// a directory whose permission bits are already 0700; Open does not change
// it. A non-empty Name that breaks the seat-name rule is ErrInvalid and
// changes nothing. An empty journal receives one server record, under the
// journal's exclusive lock, so two processes that start together agree on
// one ID.
func Open(stateDir string, opts Options) (*Registry, error) {
	if stateDir == "" {
		return nil, errStateDir
	}
	if opts.MaxSeats < 0 {
		return nil, errNegative
	}
	if opts.Name != "" && !validName(opts.Name) {
		return nil, ErrInvalid
	}
	maxSeats := opts.MaxSeats
	if maxSeats == 0 {
		maxSeats = defaultMaxSeats
	}
	name := opts.Name
	if name == "" {
		name = defaultName
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	entropy := opts.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	info, err := ensureStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("state directory: not a directory")
	}
	if perm := info.Mode().Perm(); perm != dirPerm {
		return nil, fmt.Errorf("state directory: mode %04o, want %04o", perm, dirPerm)
	}
	path := filepath.Join(stateDir, journalName)
	j, err := journal.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		dir:          stateDir,
		path:         path,
		j:            j,
		now:          now,
		entropy:      entropy,
		maxSeats:     maxSeats,
		byID:         map[string]*codeRec{},
		byVerifier:   map[string]*codeRec{},
		activeBySeat: map[string]*codeRec{},
		byToken:      map[string]*codeRec{},
	}
	if _, err := r.update(false, func() ([][]byte, error) {
		if r.serverID != "" {
			return nil, nil
		}
		id, err := randomHex(r.entropy, 16)
		if err != nil {
			return nil, err
		}
		return oneRecord(record{
			Type: "server",
			TS:   r.stamp(),
			ID:   id,
			Name: name,
		})
	}); err != nil {
		j.Close()
		return nil, err
	}
	return r, nil
}

// ensureStateDir reports the path as it is, or creates it mode 0700 when
// it is missing. It does not change an existing path.
func ensureStateDir(path string) (os.FileInfo, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		err = os.MkdirAll(path, dirPerm)
	}
	if err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

// Close releases the journal. A second call is a no-op. Reads of state
// already loaded still work; Refresh and every mutating call fail.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.j == nil {
		return nil
	}
	err := r.j.Close()
	r.j = nil
	return err
}

// Path is the journal file.
func (r *Registry) Path() string { return r.path }

// Refresh applies journal lines written since the last read. The returned
// IDs are the installations whose revoke record was applied during this
// call. The slice is a copy.
func (r *Registry) Refresh() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.closed(); err != nil {
		return nil, err
	}
	r.collect = true
	r.pending = nil
	err := r.j.Refresh(r.apply)
	revoked := append([]string(nil), r.pending...)
	r.collect = false
	r.pending = nil
	return revoked, err
}

// Server returns the identity stored in the journal.
func (r *Registry) Server() Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Server{ID: r.serverID, Name: r.serverName}
}

// Issue draws a join code, retries on an identifier collision, and stores
// only the verifier. The plaintext is returned once, grouped for display.
func (r *Registry) Issue() (string, Code, error) {
	var plain string
	var issued Code
	_, err := r.update(false, func() ([][]byte, error) {
		for range invite.CollisionRetries {
			norm, err := invite.Generate(r.entropy)
			if err != nil {
				return nil, err
			}
			sum := invite.Verifier(norm)
			id := invite.CodeID(sum)
			if _, ok := r.byID[id]; ok {
				continue
			}
			now := r.now().UTC()
			exp := now.Add(invite.UnusedTTL)
			line, err := oneRecord(record{
				Type:     "issue",
				TS:       now.Format(time.RFC3339Nano),
				ID:       id,
				Verifier: hex.EncodeToString(sum),
				Expires:  exp.Format(time.RFC3339Nano),
			})
			if err != nil {
				return nil, err
			}
			plain = invite.Format(norm)
			issued = Code{ID: id, State: "unused", Issued: now, Expires: exp}
			return line, nil
		}
		return nil, errCollision
	})
	if err != nil {
		return "", Code{}, err
	}
	return plain, issued, nil
}

// List returns every code, ordered by issue time and then by ID. The slice
// is a copy. State is evaluated at the registry clock.
func (r *Registry) List() []Code {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Code, len(r.order))
	for i, c := range r.order {
		out[i] = r.row(c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Issued.Equal(out[j].Issued) {
			return out[i].ID < out[j].ID
		}
		return out[i].Issued.Before(out[j].Issued)
	})
	return out
}

// Status counts codes by current state.
func (r *Registry) Status() Status {
	var s Status
	for _, row := range r.List() {
		s.Codes++
		switch row.State {
		case "unused":
			s.Unused++
		case "redeemed":
			s.Redeemed++
		case "revoked":
			s.Revoked++
		case "expired":
			s.Expired++
		}
	}
	return s
}

// Revoke ends a code. key is a code ID or the installation ID of an active
// seat. A code ID wins when a key is both. Revoking an unused code makes it
// unusable. Revoking a redeemed code ends the seat. Revoking an
// already-revoked code succeeds and appends nothing.
func (r *Registry) Revoke(key string) (Code, error) {
	var row Code
	_, err := r.update(false, func() ([][]byte, error) {
		c := r.byID[key]
		if c == nil {
			c = r.activeBySeat[key]
		}
		if c == nil {
			return nil, ErrUnknown
		}
		row = r.row(c)
		row.State = "revoked"
		if c.revoked {
			return nil, nil
		}
		return oneRecord(record{Type: "revoke", TS: r.stamp(), ID: c.id})
	})
	if err != nil {
		return Code{}, err
	}
	return row, nil
}

// RevokeSeat ends the active seat with that installation ID. Unlike
// Revoke it never treats id as a code ID.
func (r *Registry) RevokeSeat(id string) (Code, error) {
	var row Code
	_, err := r.update(false, func() ([][]byte, error) {
		c := r.activeBySeat[id]
		if c == nil {
			return nil, ErrUnknown
		}
		row = r.row(c)
		row.State = "revoked"
		return oneRecord(record{Type: "revoke", TS: r.stamp(), ID: c.id})
	})
	if err != nil {
		return Code{}, err
	}
	return row, nil
}

// Rename changes the name of the active seat with that installation ID.
// A name the protocol rejects is ErrInvalid. An ID that is not an active
// seat is ErrUnknown. Both append nothing. Renaming to the current name
// succeeds and appends nothing.
func (r *Registry) Rename(id, name string) (Seat, error) {
	var seat Seat
	_, err := r.update(false, func() ([][]byte, error) {
		if !validName(name) {
			return nil, ErrInvalid
		}
		c := r.activeBySeat[id]
		if c == nil {
			return nil, ErrUnknown
		}
		seat = seatOf(c)
		if c.name == name {
			return nil, nil
		}
		seat.Name = name
		return oneRecord(record{Type: "name", TS: r.stamp(), ID: c.id, Name: name})
	})
	if err != nil {
		return Seat{}, err
	}
	return seat, nil
}

// Register binds an unused code to one installation and returns a new token.
// Checks run in order: the code parses and is unused, else ErrJoinRefused;
// the installation ID and the name follow the protocol, else ErrInvalid;
// the installation is not already active, else ErrAlreadyRegistered; the
// seat limit is not reached, else ErrFull. A refusal appends nothing.
// revoked lists installations whose revoke was applied while replaying the
// journal for this call.
func (r *Registry) Register(code, installationID, name string) (string, []string, error) {
	var token string
	revoked, err := r.update(true, func() ([][]byte, error) {
		norm, err := invite.Parse(code)
		if err != nil {
			return nil, ErrJoinRefused
		}
		c := r.byVerifier[hex.EncodeToString(invite.Verifier(norm))]
		if c == nil || c.state(r.now()) != "unused" {
			return nil, ErrJoinRefused
		}
		if !goodHex(installationID, 16) || !validName(name) {
			return nil, ErrInvalid
		}
		if _, ok := r.activeBySeat[installationID]; ok {
			return nil, ErrAlreadyRegistered
		}
		if len(r.activeBySeat) >= r.maxSeats {
			return nil, ErrFull
		}
		tok, err := randomHex(r.entropy, 32)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(tok))
		line, err := oneRecord(record{
			Type:     "bind",
			TS:       r.stamp(),
			ID:       c.id,
			Seat:     installationID,
			Name:     name,
			TokenSHA: hex.EncodeToString(sum[:]),
		})
		if err != nil {
			return nil, err
		}
		token = tok
		return line, nil
	})
	if err != nil {
		return "", revoked, err
	}
	return token, revoked, nil
}

// Seats returns the active seats, ordered by installation ID. The slice is a copy.
func (r *Registry) Seats() []Seat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Seat, 0, len(r.activeBySeat))
	for _, c := range r.activeBySeat {
		out = append(out, seatOf(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Seat returns the active seat with that installation ID.
func (r *Registry) Seat(id string) (Seat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.activeBySeat[id]
	if c == nil {
		return Seat{}, false
	}
	return seatOf(c), true
}

// Authenticate returns the active seat for a token. An empty token, an
// unknown token and a revoked seat's token all fail.
func (r *Registry) Authenticate(token string) (Seat, bool) {
	if token == "" {
		return Seat{}, false
	}
	sum := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.byToken[hex.EncodeToString(sum[:])]
	if c == nil {
		return Seat{}, false
	}
	return seatOf(c), true
}

func (r *Registry) update(collect bool, next func() ([][]byte, error)) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.closed(); err != nil {
		return nil, err
	}
	r.collect = collect
	r.pending = nil
	err := r.j.Update(r.apply, next)
	revoked := append([]string(nil), r.pending...)
	r.collect = false
	r.pending = nil
	return revoked, err
}

func (r *Registry) closed() error {
	if r.j == nil {
		return errClosed
	}
	return nil
}

func (r *Registry) stamp() string {
	return r.now().UTC().Format(time.RFC3339Nano)
}

func (r *Registry) apply(_ int64, line []byte) error {
	rec, err := parseRecord(line)
	if err != nil {
		return err
	}
	ts, err := parseTime(rec.TS)
	if err != nil {
		return err
	}
	switch rec.Type {
	case "server":
		return r.applyServer(rec)
	case "issue":
		return r.applyIssue(rec, ts)
	case "bind":
		return r.applyBind(rec, ts)
	case "revoke":
		return r.applyRevoke(rec)
	case "name":
		return r.applyName(rec)
	default:
		return errUnknownType
	}
}

func (r *Registry) applyServer(rec record) error {
	if !goodHex(rec.ID, 16) || !validName(rec.Name) || rec.Verifier != "" || rec.Expires != "" || rec.Seat != "" || rec.TokenSHA != "" {
		return errMalformed
	}
	if r.serverID != "" {
		return errSecond
	}
	r.serverID = rec.ID
	r.serverName = rec.Name
	return nil
}

func (r *Registry) applyIssue(rec record, ts time.Time) error {
	if r.serverID == "" {
		return errBefore
	}
	if !goodHex(rec.ID, 16) || !goodHex(rec.Verifier, sha256.Size) || rec.Name != "" || rec.Seat != "" || rec.TokenSHA != "" {
		return errMalformed
	}
	exp, err := parseTime(rec.Expires)
	if err != nil {
		return err
	}
	// goodHex already accepted the verifier, so DecodeString cannot fail.
	raw, _ := hex.DecodeString(rec.Verifier)
	if invite.CodeID(raw) != rec.ID {
		return errIssueID
	}
	if _, ok := r.byID[rec.ID]; ok {
		return errDuplicate
	}
	c := &codeRec{
		id:       rec.ID,
		verifier: rec.Verifier,
		issued:   ts,
		expires:  exp,
	}
	r.byID[c.id] = c
	r.byVerifier[c.verifier] = c
	r.order = append(r.order, c)
	return nil
}

func (r *Registry) applyBind(rec record, ts time.Time) error {
	if r.serverID == "" {
		return errBefore
	}
	if !goodHex(rec.ID, 16) {
		return errMalformed
	}
	c := r.byID[rec.ID]
	if c == nil {
		return errNoCode
	}
	if c.state(ts) != "unused" {
		return errNotUnused
	}
	if !goodHex(rec.Seat, 16) || !validName(rec.Name) || !goodHex(rec.TokenSHA, sha256.Size) || rec.Verifier != "" || rec.Expires != "" {
		return errMalformed
	}
	if _, ok := r.activeBySeat[rec.Seat]; ok {
		return errActive
	}
	c.seat = rec.Seat
	c.name = rec.Name
	c.registered = ts
	c.tokenHash = rec.TokenSHA
	r.activeBySeat[c.seat] = c
	r.byToken[c.tokenHash] = c
	return nil
}

func (r *Registry) applyRevoke(rec record) error {
	if r.serverID == "" {
		return errBefore
	}
	if !goodHex(rec.ID, 16) || rec.Name != "" || rec.Verifier != "" || rec.Expires != "" || rec.Seat != "" || rec.TokenSHA != "" {
		return errMalformed
	}
	c := r.byID[rec.ID]
	if c == nil {
		return errNoCode
	}
	if c.revoked {
		return nil
	}
	if c.seat != "" {
		delete(r.activeBySeat, c.seat)
		delete(r.byToken, c.tokenHash)
		if r.collect {
			r.pending = append(r.pending, c.seat)
		}
	}
	c.revoked = true
	return nil
}

// applyName requires a redeemed code that is not revoked, a valid name and
// no other field. Anything else damages the journal.
func (r *Registry) applyName(rec record) error {
	if r.serverID == "" {
		return errBefore
	}
	if !goodHex(rec.ID, 16) || !validName(rec.Name) || rec.Verifier != "" || rec.Expires != "" || rec.Seat != "" || rec.TokenSHA != "" {
		return errMalformed
	}
	c := r.byID[rec.ID]
	if c == nil {
		return errNoCode
	}
	if c.revoked || c.seat == "" {
		return errMalformed
	}
	c.name = rec.Name
	return nil
}

func (c *codeRec) state(now time.Time) string {
	if c.revoked {
		return "revoked"
	}
	if c.seat != "" {
		return "redeemed"
	}
	if !now.Before(c.expires) {
		return "expired"
	}
	return "unused"
}

func (r *Registry) row(c *codeRec) Code {
	return Code{
		ID:      c.id,
		State:   c.state(r.now()),
		Issued:  c.issued,
		Expires: c.expires,
		Seat:    c.seat,
		Name:    c.name,
	}
}

func seatOf(c *codeRec) Seat {
	return Seat{ID: c.seat, Name: c.name, Registered: c.registered}
}

func parseRecord(line []byte) (record, error) {
	if len(line) == 0 {
		return record{}, errEmpty
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var rec record
	if err := dec.Decode(&rec); err != nil {
		return record{}, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return record{}, errTrailing
	}
	if rec.Type == "" {
		return record{}, errMalformed
	}
	return rec, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("registry: missing time")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("registry: bad time")
	}
	return t.UTC(), nil
}

func goodHex(s string, nBytes int) bool {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != nBytes || hex.EncodeToString(b) != s {
		return false
	}
	return true
}

func validName(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	if name[0] == ' ' || name[len(name)-1] == ' ' {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func randomHex(r io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func oneRecord(rec record) ([][]byte, error) {
	b, err := marshalRec(rec)
	if err != nil {
		return nil, err
	}
	return [][]byte{b}, nil
}
