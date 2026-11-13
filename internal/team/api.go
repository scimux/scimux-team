package team

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

// Limits are fixed. Stage A has no flags for them.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 16 << 10
	shutdownGrace     = 5 * time.Second
	maxBodyBytes      = 16 << 20

	publicWarning = "warning: plain HTTP is for trusted networks or behind a TLS proxy"
	revokeNote    = "the seat's token stops working and its channels and memberships are removed"
)

// marshalJSON is json.Marshal. Tests replace it to force a response that
// cannot be encoded; a valid response value never makes Marshal fail.
var marshalJSON = json.Marshal

type helloResp struct {
	Protocol int        `json:"protocol"`
	Server   serverJSON `json:"server"`
}

type serverJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type registerReq struct {
	JoinCode       string `json:"join_code"`
	InstallationID string `json:"installation_id"`
	Name           string `json:"name"`
}

type registerResp struct {
	Token  string     `json:"token"`
	Server serverJSON `json:"server"`
}

type seatsResp struct {
	Seats []seatJSON `json:"seats"`
}

type seatJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// server is the HTTP handler. One mutex serializes the registry and the
// channel store. The lock is not held while a body is read or a response
// is written.
type server struct {
	reg         *registry.Registry
	channels    *channel.Store
	mu          sync.Mutex
	mux         *http.ServeMux
	cleanupDue  bool
	cleanupSkip []string
}

// beforeRegister, when set, runs after Refresh and before Register.
// Production leaves it nil. A test uses it to revoke a seat in that window.
var beforeRegister func()

// beforeRename, when set, runs after Refresh and before Rename.
// Production leaves it nil.
var beforeRename func()

// beforeRevokeSeat, when set, runs after Refresh and before RevokeSeat.
// Production leaves it nil.
var beforeRevokeSeat func()

func newHandler(reg *registry.Registry) *server {
	s := &server{reg: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", s.hello)
	mux.HandleFunc("POST /v1/register", s.register)
	mux.HandleFunc("GET /v1/seats", s.seats)
	mux.HandleFunc("PUT /v1/seat", s.putSeat)
	mux.HandleFunc("DELETE /v1/seat", s.deleteSeat)
	mux.HandleFunc("GET /v1/channels", s.listChannels)
	mux.HandleFunc("PUT /v1/channels/{owner}/{chat}", s.putChannel)
	mux.HandleFunc("DELETE /v1/channels/{owner}/{chat}", s.deleteChannel)
	mux.HandleFunc("PUT /v1/channels/{owner}/{chat}/members", s.putMembers)
	mux.HandleFunc("DELETE /v1/channels/{owner}/{chat}/membership", s.deleteMembership)
	mux.HandleFunc("PUT /v1/channels/{owner}/{chat}/blobs/{id}", s.putBlob)
	mux.HandleFunc("GET /v1/channels/{owner}/{chat}/blobs/{id}", s.getBlob)
	mux.HandleFunc("GET /v1/channels/{owner}/{chat}/mirror", s.getMirror)
	mux.HandleFunc("POST /v1/channels/{owner}/{chat}/mirror", s.postMirror)
	mux.HandleFunc("GET /v1/channels/{owner}/{chat}/comments", s.getComments)
	mux.HandleFunc("POST /v1/channels/{owner}/{chat}/comments", s.postComments)
	mux.HandleFunc("/", s.notFound)
	s.mux = mux
	return s
}

func newHTTPServer(h http.Handler) *http.Server {
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(false)
	protos.SetUnencryptedHTTP2(false)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protos,
		// Discard: a nil ErrorLog writes request data to the standard logger.
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// errNotFound, errUnauthorized and errForbidden are handler results. They
// are not store failures: finish writes them as the constant protocol bodies.
var (
	errNotFound     = errors.New("not found")
	errUnauthorized = errors.New("unauthorized")
	errForbidden    = errors.New("forbidden")
)

// ServeHTTP rejects a path that is not already in cleaned form, then
// dispatches. A cleaned path the mux does not know is the constant
// not_found body, including a method this server does not register.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != cleanPath(r.URL.Path) {
		s.notFound(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// withRegistry holds the handler mutex around Refresh and the calls that
// use that refresh. A cleanup that is already due runs first, and a failure
// there returns before the registry is asked. A revoke applied by Refresh
// is reconciled before the request continues. A failed reconcile stays due
// until a later call succeeds. Callers read a body before it and write the
// response after it returns.
func (s *server) withRegistry(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.finishCleanup(); err != nil {
		return err
	}
	revoked, err := s.reg.Refresh()
	if err != nil {
		return err
	}
	if len(revoked) > 0 {
		if err := s.reconcile(nil); err != nil {
			s.noteCleanup(nil)
			return err
		}
	}
	return fn()
}

// finishCleanup repeats a reconcile that an earlier request could not
// finish. The mark stays set until reconcile returns no error.
func (s *server) finishCleanup() error {
	if !s.cleanupDue {
		return nil
	}
	if err := s.reconcile(s.cleanupSkip); err != nil {
		return err
	}
	s.cleanupDue = false
	s.cleanupSkip = nil
	return nil
}

// noteCleanup remembers that reconcile failed. skip holds installation ids
// Register just rebound; they are active and must still be treated as
// inactive. A nil skip means the registry already shows the seats as gone.
func (s *server) noteCleanup(skip []string) {
	s.cleanupDue = true
	s.cleanupSkip = append([]string(nil), skip...)
}

func (s *server) finish(w http.ResponseWriter, err error, status int, v any) {
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "not_found")
		case errors.Is(err, errUnauthorized):
			writeError(w, http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, errForbidden):
			writeError(w, http.StatusForbidden, "forbidden")
		default:
			writeMapped(w, err)
		}
		return
	}
	if status == http.StatusNoContent {
		writeHeaders(w)
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, v)
}

func (s *server) hello(w http.ResponseWriter, r *http.Request) {
	var srv registry.Server
	err := s.withRegistry(func() error {
		if r.Method != http.MethodGet {
			return errNotFound
		}
		srv = s.reg.Server()
		return nil
	})
	s.finish(w, err, http.StatusOK, helloResp{
		Protocol: 1,
		Server:   serverJSON{ID: srv.ID, Name: srv.Name},
	})
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !readJSON(w, r, &req) {
		return
	}
	var token string
	var srv registry.Server
	err := s.withRegistry(func() error {
		if beforeRegister != nil {
			beforeRegister()
		}
		var revoked []string
		var regErr error
		token, revoked, regErr = s.reg.Register(req.JoinCode, req.InstallationID, req.Name)
		if regErr != nil {
			return regErr
		}
		// The id can be active again and still listed here: Register replays
		// a revoke and then binds the same id. Skip it so the new seat
		// inherits nothing.
		if len(revoked) > 0 {
			if err := s.reconcile(revoked); err != nil {
				s.noteCleanup(revoked)
				return err
			}
		}
		srv = s.reg.Server()
		return nil
	})
	s.finish(w, err, http.StatusCreated, registerResp{
		Token:  token,
		Server: serverJSON{ID: srv.ID, Name: srv.Name},
	})
}

func (s *server) seats(w http.ResponseWriter, r *http.Request) {
	var rows []registry.Seat
	err := s.withRegistry(func() error {
		if r.Method != http.MethodGet {
			return errNotFound
		}
		// Authentication before the body. A bad body on an unauthenticated
		// request is unauthorized, not bad_request. This handler does not
		// read the body.
		if _, ok := authenticate(s.reg, r); !ok {
			return errUnauthorized
		}
		rows = s.reg.Seats()
		return nil
	})
	out := make([]seatJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, seatJSON{ID: row.ID, Name: row.Name})
	}
	s.finish(w, err, http.StatusOK, seatsResp{Seats: out})
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	err := s.withRegistry(func() error { return errNotFound })
	s.finish(w, err, 0, nil)
}

func authenticate(reg *registry.Registry, r *http.Request) (registry.Seat, bool) {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return registry.Seat{}, false
	}
	return reg.Authenticate(value[len(prefix):])
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request")
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

func writeMapped(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrJoinRefused):
		writeError(w, http.StatusForbidden, "join_refused")
	case errors.Is(err, registry.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, registry.ErrAlreadyRegistered):
		writeError(w, http.StatusConflict, "already_registered")
	case errors.Is(err, registry.ErrFull):
		writeError(w, http.StatusInsufficientStorage, "quota")
	case errors.Is(err, channel.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, channel.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
	case errors.Is(err, channel.ErrQuota):
		writeError(w, http.StatusInsufficientStorage, "quota")
	case errors.Is(err, errUnknownMember):
		writeError(w, http.StatusUnprocessableEntity, "unknown_member")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalJSON(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}

func writeHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// cleanPath matches net/http.ServeMux's unexported cleanPath so the wrapper
// intercepts exactly the requests the mux would redirect.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}
