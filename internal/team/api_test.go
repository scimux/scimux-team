package team

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/invite"
	"github.com/scimux/scimux-team/internal/registry"
)

func TestHelloAndSeatsShapes(t *testing.T) {
	raw := append(bytes.Repeat([]byte{0x01}, 16), bytes.Repeat([]byte{0x21}, invite.EntropyBytes)...)
	raw = append(raw, bytes.Repeat([]byte{0x31}, 32)...)
	reg, h := testHandler(t, bytes.NewReader(raw), "relay a")
	status, hdr, body := do(h, http.MethodGet, "/v1/hello", "", nil)
	want := `{"protocol":1,"server":{"id":"` + hex.EncodeToString(bytes.Repeat([]byte{0x01}, 16)) + `","name":"relay a"}}`
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("hello %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	if hdr.Get("Location") != "" {
		t.Fatal("redirect")
	}

	// No seats yet.
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := reg.Register(code, inst(0x22), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	// The handler has not refreshed. Seats still requires a token, and the
	// registration is visible because this registry is the handler's.
	status, hdr, body = do(h, http.MethodGet, "/v1/seats", token, strings.NewReader("{"))
	if status != http.StatusOK || string(body) != `{"seats":[{"id":"`+inst(0x22)+`","name":"seat a"}]}` {
		t.Fatalf("seats %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)

	status, _, body = do(h, http.MethodGet, "/v1/seats", "", strings.NewReader("{"))
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("unauthenticated bad body %d %s", status, body)
	}
	status, _, body = do(h, http.MethodGet, "/v1/seats", "", nil)
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("missing token %d %s", status, body)
	}
	status, _, body = do(h, http.MethodGet, "/v1/seats", "deadbeef", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong token %d %s", status, body)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.example/v1/seats", nil)
	req.Header.Set("Authorization", "Basic "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("scheme %d", rec.Code)
	}
}

func TestRegisterSuccessAndRefusals(t *testing.T) {
	entropy := append(append(bytes.Repeat([]byte{0x01}, 16), bytes.Repeat([]byte{0x11}, invite.EntropyBytes)...), bytes.Repeat([]byte{0x44}, 32)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x12}, invite.EntropyBytes)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x45}, 32)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x13}, invite.EntropyBytes)...)
	reg, h := testHandler(t, bytes.NewReader(entropy), "relay a")
	id := hex.EncodeToString(bytes.Repeat([]byte{0x01}, 16))
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	loose := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	status, hdr, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+loose+`","installation_id":"`+inst(0x11)+`","name":"seat a"}`))
	wantToken := hex.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	want := `{"token":"` + wantToken + `","server":{"id":"` + id + `","name":"relay a"}}`
	if status != http.StatusCreated || string(body) != want {
		t.Fatalf("register %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	if seat, ok := reg.Authenticate(wantToken); !ok || seat.Name != "seat a" {
		t.Fatal("token did not authenticate")
	}

	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+code+`","installation_id":"`+inst(0x12)+`","name":"seat b"}`))
	if status != http.StatusForbidden || string(body) != `{"error":"join_refused"}` {
		t.Fatalf("second use %d %s", status, body)
	}

	other, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	before := journalOf(t, reg)
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(`{"join_code":"!!!","installation_id":"nope","name":""}`))
	if status != http.StatusForbidden || string(body) != `{"error":"join_refused"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("bad code %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+other+`","installation_id":"NOPE","name":"seat b"}`))
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("bad id %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+other+`","installation_id":"`+inst(0x12)+`","name":" seat"}`))
	if status != http.StatusBadRequest || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("bad name %d %s", status, body)
	}
	// The refused attempts left the code usable.
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+other+`","installation_id":"`+inst(0x12)+`","name":"seat b"}`))
	if status != http.StatusCreated {
		t.Fatalf("retry %d %s", status, body)
	}

	third, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	before = journalOf(t, reg)
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(
		`{"join_code":"`+third+`","installation_id":"`+inst(0x11)+`","name":"again"}`))
	if status != http.StatusConflict || string(body) != `{"error":"already_registered"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("already %d %s", status, body)
	}
}

func TestRegisterQuotaAndInternal(t *testing.T) {
	reg, h := testHandlerOpts(t, registry.Options{
		Name:     "relay a",
		MaxSeats: 1,
		Now:      fixedNow,
		Entropy:  &seqReader{},
	})
	first, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(first, inst(0x11), "seat a"))); status != http.StatusCreated {
		t.Fatal(string(body))
	}
	before := journalOf(t, reg)
	status, _, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(second, inst(0x22), "seat b")))
	if status != http.StatusInsufficientStorage || string(body) != `{"error":"quota"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("quota %d %s", status, body)
	}

	// A registry whose entropy ends after the code. Registration fails closed.
	raw := append(bytes.Repeat([]byte{0x02}, 16), bytes.Repeat([]byte{0x21}, invite.EntropyBytes)...)
	reg, h = testHandler(t, bytes.NewReader(raw), "relay a")
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x33), "seat c")))
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || strings.Contains(string(body), "EOF") {
		t.Fatalf("internal %d %s", status, body)
	}
}

func TestBodyLimitUnknownFieldAndTrailing(t *testing.T) {
	reg, h := testHandler(t, &seqReader{}, "relay a")
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	before := journalOf(t, reg)
	status, _, body := do(h, http.MethodPost, "/v1/register", "", bytes.NewReader(bytes.Repeat([]byte{' '}, maxBodyBytes+1)))
	if status != http.StatusRequestEntityTooLarge || string(body) != `{"error":"too_large"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("limit %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(`{"join_code":"`+code+`","installation_id":"`+inst(0x11)+`","name":"seat a","extra":1}`))
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("extra %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x11), "seat a")+`{"more":true}`))
	if status != http.StatusBadRequest || !bytes.Equal(journalOf(t, reg), before) {
		t.Fatalf("trailing %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(`{`))
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("syntax %d %s", status, body)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(``))
	if status != http.StatusBadRequest {
		t.Fatalf("empty %d %s", status, body)
	}
	// The code is still unused.
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x11), "seat a")))
	if status != http.StatusCreated {
		t.Fatalf("still usable %d %s", status, body)
	}
}

func TestConstantNotFound(t *testing.T) {
	_, h := testHandler(t, bytes.NewReader(bytes.Repeat([]byte{0x01}, 16)), "relay a")
	const want = `{"error":"not_found"}`
	paths := []struct{ method, path string }{
		{http.MethodGet, "/v1/missing"},
		{http.MethodPost, "/v1/hello"},
		{http.MethodGet, "/v1/register"},
		{http.MethodPost, "/v1/seats"},
		{http.MethodHead, "/v1/hello"},
		{http.MethodHead, "/v1/seats"},
		{http.MethodGet, "/v1/channels"},
		{http.MethodGet, "/v1/channels/"},
		{http.MethodGet, "/v1/channels/abc/def"},
		{http.MethodPost, "/v1/channels/abc/def/mirror"},
		{http.MethodPut, "/v1/channels/abc/def"},
		{http.MethodDelete, "/v1/channels/abc/def/comments"},
		{http.MethodGet, "/"},
		{http.MethodOptions, "/v1/hello"},
	}
	var first []byte
	for _, p := range paths {
		status, hdr, body := do(h, p.method, p.path, "ignored", strings.NewReader(`{"title":"x"}`))
		if status != http.StatusNotFound || string(body) != want || hdr.Get("Location") != "" {
			t.Fatalf("%s %s %d %s location %q", p.method, p.path, status, body, hdr.Get("Location"))
		}
		assertJSONHeaders(t, hdr)
		if first == nil {
			first = append([]byte(nil), body...)
		} else if !bytes.Equal(body, first) {
			t.Fatalf("%s %s body differs", p.method, p.path)
		}
	}
	unclean := []string{"/v1/hello/../hello", "//v1/hello", "/v1/./hello", "/v1/hello/", "/v1/hello/../", "/v1/channels/../channels"}
	for _, path := range unclean {
		req := httptest.NewRequest(http.MethodGet, "http://relay.example/", nil)
		req.URL.Path = path
		req.URL.RawPath = path
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || rec.Body.String() != want || rec.Header().Get("Location") != "" {
			t.Fatalf("unclean %s %d %s", path, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), first) {
			t.Fatalf("unclean %s body differs", path)
		}
	}
}

func TestCleanPath(t *testing.T) {
	cases := []struct{ in, out string }{
		{"", "/"},
		{"v1/hello", "/v1/hello"},
		{"/v1/hello", "/v1/hello"},
		{"/", "/"},
		{"/v1/hello/", "/v1/hello/"},
		{"/v1/hello/../", "/v1/"},
		{"/v1/./hello", "/v1/hello"},
	}
	for _, tc := range cases {
		if got := cleanPath(tc.in); got != tc.out {
			t.Fatalf("cleanPath(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestRevokedSeatLosesAccessOnTheNextRequest(t *testing.T) {
	dir := t.TempDir()
	reg := openRegistry(t, dir, registry.Options{Name: "relay a", Now: fixedNow, Entropy: &seqReader{n: 1}})
	other := openRegistry(t, dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 80}})
	h := newHandler(reg)
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	otherCode, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	status, _, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x11), "seat a")))
	if status != http.StatusCreated {
		t.Fatal(string(body))
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(otherCode, inst(0x02), "seat b")))
	if status != http.StatusCreated {
		t.Fatal(string(body))
	}
	if _, err := other.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Revoke(inst(0x11)); err != nil {
		t.Fatal(err)
	}
	status, _, body = do(h, http.MethodGet, "/v1/seats", created.Token, nil)
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("revoked access %d %s", status, body)
	}
	// The second token was not kept. A later seat is the live caller.
	var second registerResp
	third, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(third, inst(0x03), "seat c")))
	if status != http.StatusCreated {
		t.Fatal(string(body))
	}
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatal(err)
	}
	status, _, body = do(h, http.MethodGet, "/v1/seats", second.Token, nil)
	if status != http.StatusOK || strings.Contains(string(body), inst(0x11)) || strings.Contains(string(body), "seat a") {
		t.Fatalf("seats after revoke %d %s", status, body)
	}
	if !strings.Contains(string(body), inst(0x02)) || !strings.Contains(string(body), inst(0x03)) {
		t.Fatalf("missing live seats %s", body)
	}
}

func TestConcurrentRegistrations(t *testing.T) {
	reg, h := testHandler(t, &seqReader{}, "relay a")
	const n = 8
	codes := make([]string, n)
	for i := range codes {
		code, _, err := reg.Issue()
		if err != nil {
			t.Fatal(err)
		}
		codes[i] = code
	}
	var wg sync.WaitGroup
	errc := make(chan error, n)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(codes[i], inst(byte(0x30+i)), "seat")))
			if status != http.StatusCreated {
				errc <- errors.New(string(body))
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	if got := len(reg.Seats()); got != n {
		t.Fatalf("seats %d", got)
	}
}

func TestCorruptJournalIsInternalAndMarshalFailureHidesTheToken(t *testing.T) {
	reg, h := testHandler(t, &seqReader{}, "relay a")
	f, err := os.OpenFile(reg.Path(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"nope\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	status, _, body := do(h, http.MethodGet, "/v1/hello", "", nil)
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || strings.Contains(string(body), "nope") {
		t.Fatalf("corrupt %d %s", status, body)
	}

	reg, h = testHandler(t, &seqReader{n: 40}, "relay a")
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	orig := marshalJSON
	t.Cleanup(func() { marshalJSON = orig })
	marshalJSON = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x11), "seat a")))
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || strings.Contains(string(body), "seat a") {
		t.Fatalf("marshal %d %s", status, body)
	}
	// The seat was stored before the response was encoded, so the code is spent.
	status, _, body = do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, inst(0x12), "seat b")))
	if status != http.StatusForbidden || string(body) != `{"error":"join_refused"}` {
		t.Fatalf("spent code %d %s", status, body)
	}
}

func TestServerLimits(t *testing.T) {
	srv := newHTTPServer(http.NewServeMux())
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.ReadTimeout != readTimeout || srv.WriteTimeout != writeTimeout || srv.IdleTimeout != idleTimeout || srv.MaxHeaderBytes != maxHeaderBytes {
		t.Fatalf("timeouts %+v", srv)
	}
	if srv.ErrorLog == nil || srv.Protocols == nil || !srv.Protocols.HTTP1() || srv.Protocols.HTTP2() || srv.Protocols.UnencryptedHTTP2() {
		t.Fatal("server defaults left open")
	}
}

func testHandler(t *testing.T, entropy io.Reader, name string) (*registry.Registry, http.Handler) {
	t.Helper()
	return testHandlerOpts(t, registry.Options{Name: name, Now: fixedNow, Entropy: entropy})
}

func testHandlerOpts(t *testing.T, opts registry.Options) (*registry.Registry, http.Handler) {
	t.Helper()
	reg := openRegistry(t, t.TempDir(), opts)
	return reg, newHandler(reg)
}

func openRegistry(t *testing.T, dir string, opts registry.Options) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return reg
}

func do(h http.Handler, method, path, token string, body io.Reader) (int, http.Header, []byte) {
	req := httptest.NewRequest(method, "http://relay.example"+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

func assertJSONHeaders(t *testing.T, hdr http.Header) {
	t.Helper()
	if hdr.Get("Content-Type") != "application/json" || hdr.Get("Cache-Control") != "no-store" || hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", hdr)
	}
}

func jsonBody(code, id, name string) string {
	b, err := json.Marshal(registerReq{JoinCode: code, InstallationID: id, Name: name})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func journalOf(t *testing.T, reg *registry.Registry) []byte {
	t.Helper()
	b, err := os.ReadFile(reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func inst(b byte) string {
	return strings.Repeat(hex.EncodeToString([]byte{b}), 16)
}

func fixedNow() time.Time {
	return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
}

type seqReader struct{ n byte }

func (s *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = s.n
		s.n++
	}
	return len(p), nil
}
