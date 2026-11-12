// Package registry stores the server identity, join codes and seats.
package registry

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

const (
	journalName     = "registry.jsonl"
	defaultName     = "scimux team"
	defaultMaxSeats = 200
)

// Options configures Open. Zero values select the defaults.
type Options struct {
	Name     string
	MaxSeats int
	Now      func() time.Time
	Entropy  io.Reader
}

// Server is the identity written on the first open of a state directory.
type Server struct {
	ID   string
	Name string
}

// Code is one join code. It never carries the plaintext.
type Code struct {
	ID      string
	State   string
	Issued  time.Time
	Expires time.Time
	Seat    string
	Name    string
}

// Seat is one active seat.
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
	ErrJoinRefused       = errors.New("join refused")
	ErrAlreadyRegistered = errors.New("already registered")
	ErrFull              = errors.New("seat limit reached")
	ErrInvalid           = errors.New("invalid registration")
	ErrUnknown           = errors.New("unknown")

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

var chmodPath = os.Chmod
var marshalRec = json.Marshal

// Registry is the server's view of one state directory.
type Registry struct {
	dir      string
	entropy  io.Reader
	maxSeats int
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

var errStub = errors.New("stub")

// Open creates the state directory and opens the journal.
func Open(stateDir string, opts Options) (*Registry, error) {
	return nil, errStub
}

// Close closes the journal.
func (r *Registry) Close() error { return nil }

// Path is the journal path.
func (r *Registry) Path() string { return "" }

// Refresh applies records appended by other processes.
func (r *Registry) Refresh() ([]string, error) { return nil, errStub }

// Server returns the loaded identity.
func (r *Registry) Server() Server { return Server{} }

// Issue draws one join code.
func (r *Registry) Issue() (string, Code, error) { return "", Code{}, errStub }

// List returns a copy of every code.
func (r *Registry) List() []Code { return nil }

// Status counts codes by state.
func (r *Registry) Status() Status { return Status{} }

// Revoke revokes a code or an active seat.
func (r *Registry) Revoke(key string) (Code, error) { return Code{}, errStub }

// Register binds a join code to one installation.
func (r *Registry) Register(code, installationID, name string) (string, []string, error) {
	return "", nil, errStub
}

// Seats returns a copy of the active seats.
func (r *Registry) Seats() []Seat { return nil }

// Seat returns one active seat.
func (r *Registry) Seat(id string) (Seat, bool) { return Seat{}, false }

// Authenticate returns the active seat for a token.
func (r *Registry) Authenticate(token string) (Seat, bool) { return Seat{}, false }

func goodHex(s string, nBytes int) bool { return false }

func validName(name string) bool { return false }
