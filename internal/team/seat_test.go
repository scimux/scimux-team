package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

func TestRenameSeat(t *testing.T) {
	p := newParty(t, channel.Options{})
	id := inst(0x11)
	token := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, token, id, "main", "title-main")
	callOK(t, p.h, http.MethodPost, "/v1/channels/"+id+"/main/mirror", token, `{"entries":[{"key":"k","body":{"n":1}}]}`)

	before := journalOf(t, p.reg)
	status, hdr, body := call(p.h, http.MethodPut, "/v1/seat", token, `{"name":"seat a"}`)
	if status != http.StatusOK || string(body) != `{"id":"`+id+`","name":"seat a"}` {
		t.Fatalf("same name %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	if !bytes.Equal(journalOf(t, p.reg), before) {
		t.Fatal("same name appended a record")
	}

	status, hdr, body = call(p.h, http.MethodPut, "/v1/seat", token, `{"name":"next"}`)
	if status != http.StatusOK || string(body) != `{"id":"`+id+`","name":"next"}` {
		t.Fatalf("rename %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	listed := callOK(t, p.h, http.MethodGet, "/v1/seats", other, "")
	if !strings.Contains(listed, `"id":"`+id+`"`) || !strings.Contains(listed, `"name":"next"`) || strings.Contains(listed, `"name":"seat a"`) {
		t.Fatalf("seats %s", listed)
	}
	page := callOK(t, p.h, http.MethodGet, "/v1/channels/"+id+"/main/mirror", token, "")
	if !strings.Contains(page, `"author":"`+id+`"`) || !strings.Contains(page, `"key":"k"`) {
		t.Fatalf("entry changed %s", page)
	}
}

func TestRenameRejectsABadName(t *testing.T) {
	reg, h := testHandler(t, &seqReader{}, "relay a")
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	id := inst(0x11)
	status, _, body := do(h, http.MethodPost, "/v1/register", "", strings.NewReader(jsonBody(code, id, "seat a")))
	if status != http.StatusCreated {
		t.Fatal(string(body))
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	before := journalOf(t, reg)
	for _, raw := range []string{
		`{"name":""}`,
		`{"name":" next"}`,
		`{"name":"next "}`,
		`{"name":"` + strings.Repeat("n", 65) + `"}`,
		`{"name":"next\n"}`,
		`{"name":"next","extra":1}`,
		`{`,
		``,
	} {
		status, hdr, got := call(h, http.MethodPut, "/v1/seat", created.Token, raw)
		if status != http.StatusBadRequest || string(got) != `{"error":"bad_request"}` {
			t.Fatalf("body %q %d %s", raw, status, got)
		}
		assertJSONHeaders(t, hdr)
	}
	if !bytes.Equal(journalOf(t, reg), before) {
		t.Fatal("bad name appended a record")
	}
	listed := callOK(t, h, http.MethodGet, "/v1/seats", created.Token, "")
	if !strings.Contains(listed, `"name":"seat a"`) || strings.Contains(listed, `"name":"next"`) {
		t.Fatalf("name changed %s", listed)
	}
	expectWithoutRead(t, h, http.MethodPut, "/v1/seat", "", http.StatusUnauthorized, `{"error":"unauthorized"}`)
}

func TestRenameWhileTheBodyIsRevoked(t *testing.T) {
	p := newParty(t, channel.Options{})
	id := inst(0x11)
	token := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 90}})
	status, _, body := callReader(p.h, http.MethodPut, "/v1/seat", token, &readHook{
		r: strings.NewReader(`{"name":"next"}`),
		hook: func() {
			if _, err := second.Refresh(); err != nil {
				t.Errorf("refresh %v", err)
			}
			if _, err := second.Revoke(id); err != nil {
				t.Errorf("revoke %v", err)
			}
		},
	})
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("revoked during rename %d %s", status, body)
	}
	listed := callOK(t, p.h, http.MethodGet, "/v1/seats", other, "")
	if strings.Contains(listed, `"name":"next"`) || strings.Contains(listed, id) {
		t.Fatalf("renamed after revoke %s", listed)
	}
}

func TestRenameBodyDoesNotHoldTheLock(t *testing.T) {
	p := newParty(t, channel.Options{})
	id := inst(0x11)
	token := p.seat(t, 0x11, "seat a")
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	entered := make(chan struct{})
	done := make(chan struct {
		status int
		body   string
	}, 1)
	go func() {
		status, _, body := do(p.h, http.MethodPut, "/v1/seat", token, &signalReader{r: pr, entered: entered})
		done <- struct {
			status int
			body   string
		}{status, string(body)}
	}()
	select {
	case <-entered:
	case got := <-done:
		t.Fatalf("answered %d before the body %s", got.status, got.body)
	case <-time.After(time.Second):
		t.Fatal("rename did not read")
	}
	helloDone := make(chan int, 1)
	go func() {
		status, _, _ := do(p.h, http.MethodGet, "/v1/hello", "", nil)
		helloDone <- status
	}()
	select {
	case status := <-helloDone:
		if status != http.StatusOK {
			t.Fatalf("hello %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("hello blocked behind the rename body")
	}
	if _, err := pw.Write([]byte(`{"name":"next"}`)); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.status != http.StatusOK || got.body != `{"id":"`+id+`","name":"next"}` {
			t.Fatalf("rename %d %s", got.status, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("rename did not finish")
	}
}

func TestLeaveSeat(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, otherID, memberID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	member := p.seat(t, 0x33, "seat c")
	p.create(t, owner, ownerID, "alpha", "title-alpha")
	p.create(t, other, otherID, "beta", "title-beta")
	p.members(t, other, otherID, "beta", memberID, ownerID)
	callOK(t, p.h, http.MethodPost, "/v1/channels/"+otherID+"/beta/comments", member, `{"entries":[{"key":"note","body":{"kept":true}}]}`)
	data := []byte("png-bytes")
	id := blobID(data)
	if created, err := p.st.PutBlob(otherID, "beta", id, data); err != nil || !created {
		t.Fatal(err, created)
	}

	status, hdr, body := call(p.h, http.MethodDelete, "/v1/seat", owner, "")
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("leave %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	status, _, body = call(p.h, http.MethodGet, "/v1/seats", owner, "")
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("old token %d %s", status, body)
	}
	listed := callOK(t, p.h, http.MethodGet, "/v1/seats", other, "")
	if strings.Contains(listed, ownerID) || strings.Contains(listed, `"name":"seat a"`) || !strings.Contains(listed, memberID) {
		t.Fatalf("seats %s", listed)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("owned channel %v", err)
	}
	channels := callOK(t, p.h, http.MethodGet, "/v1/channels", other, "")
	if strings.Contains(channels, ownerID) || strings.Contains(channels, `"chat":"alpha"`) || !strings.Contains(channels, `"chat":"beta"`) || !strings.Contains(channels, memberID) {
		t.Fatalf("channels %s", channels)
	}
	comments := callOK(t, p.h, http.MethodGet, "/v1/channels/"+otherID+"/beta/comments", other, "")
	if !strings.Contains(comments, `"key":"note"`) {
		t.Fatalf("comment removed %s", comments)
	}
	status, hdr, body = call(p.h, http.MethodGet, "/v1/channels/"+otherID+"/beta/blobs/"+id, other, "")
	if status != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("blob after leave %d %s", status, body)
	}
	stdout, stderr, code, err := runWithin([]string{"-state-dir", p.dir, "list"})
	if err != nil || code != 0 {
		t.Fatalf("list %d %v %s %s", code, err, stdout, stderr)
	}
	revoked := false
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, " revoked ") && strings.Contains(line, ownerID) && strings.Contains(line, "seat a") {
			revoked = true
		}
	}
	if !revoked {
		t.Fatalf("operator list %s", stdout)
	}
	codeJoin, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	status, _, body = call(p.h, http.MethodPost, "/v1/register", "", jsonBody(codeJoin, ownerID, "seat a2"))
	if status != http.StatusCreated {
		t.Fatalf("register again %d %s", status, body)
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", created.Token, ""); got != `{"channels":[]}` {
		t.Fatalf("inherited %s", got)
	}
	if strings.Contains(callOK(t, p.h, http.MethodGet, "/v1/channels", other, ""), ownerID) {
		t.Fatal("new seat was put back on beta")
	}
}

func TestLeaveDoesNotReadTheBody(t *testing.T) {
	p := newParty(t, channel.Options{})
	token := p.seat(t, 0x11, "seat a")
	expectWithoutRead(t, p.h, http.MethodDelete, "/v1/seat", "", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	expectWithoutRead(t, p.h, http.MethodDelete, "/v1/seat", token, http.StatusNoContent, "")
	status, _, body := call(p.h, http.MethodGet, "/v1/seats", token, "")
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("token after leave %d %s", status, body)
	}
}

func TestLeaveCleanupFailureIsRetried(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	otherID := inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "alpha", "title-alpha")
	p.members(t, owner, ownerID, "alpha", otherID, "")
	saveTeamSeams(t)
	left := 2
	reconcileStore = func(st *channel.Store, active func(string) bool) error {
		if left > 0 {
			left--
			return errors.New("reconcile blocked")
		}
		return st.Reconcile(active)
	}
	internal := func(status int, body []byte) {
		t.Helper()
		if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("title-")) {
			t.Fatalf("cleanup %d %s", status, body)
		}
	}
	dir := filepath.Join(p.dir, "channels", ownerID, "alpha")
	status, _, body := call(p.h, http.MethodDelete, "/v1/seat", owner, "")
	internal(status, body)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	// A second registry sees the seat is gone without serving a request,
	// so the failed clean-up stays due.
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 140}})
	if _, err := second.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Seat(ownerID); ok {
		t.Fatal("seat survived the failed clean-up")
	}
	status, _, body = call(p.h, http.MethodDelete, "/v1/seat", owner, "")
	internal(status, body)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	status, _, body = call(p.h, http.MethodDelete, "/v1/seat", owner, "")
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("retry %d %s", status, body)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("channel remains %v", err)
	}
}

func TestLeaveReportsAFailedRevoke(t *testing.T) {
	p := newParty(t, channel.Options{})
	id := inst(0x11)
	token := p.seat(t, 0x11, "seat a")
	p.create(t, token, id, "alpha", "title-alpha")
	prev := revokeSeatFn
	t.Cleanup(func() { revokeSeatFn = prev })
	revokeSeatFn = func(*registry.Registry, string) (registry.Code, error) {
		return registry.Code{}, errors.New("revoke blocked")
	}
	status, _, body := call(p.h, http.MethodDelete, "/v1/seat", token, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("revoke blocked")) {
		t.Fatalf("revoke %d %s", status, body)
	}
	listed := callOK(t, p.h, http.MethodGet, "/v1/seats", token, "")
	if !strings.Contains(listed, `"name":"seat a"`) {
		t.Fatalf("seat gone %s", listed)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", id, "alpha")); err != nil {
		t.Fatal(err)
	}
}

func TestMemberCanLeaveAChannel(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	callOK(t, p.h, http.MethodPost, "/v1/channels/"+ownerID+"/main/comments", write, `{"entries":[{"key":"note","body":{"kept":true}}]}`)
	data := []byte("png-bytes")
	id := blobID(data)
	if created, err := p.st.PutBlob(ownerID, "main", id, data); err != nil || !created {
		t.Fatal(err, created)
	}
	path := "/v1/channels/" + ownerID + "/main/membership"
	status, hdr, body := call(p.h, http.MethodDelete, path, owner, "")
	if status != http.StatusForbidden || string(body) != `{"error":"forbidden"}` {
		t.Fatalf("owner %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	members := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, "")
	if !strings.Contains(members, writeID) || !strings.Contains(members, readID) {
		t.Fatalf("owner leave changed members %s", members)
	}

	status, hdr, body = call(p.h, http.MethodDelete, path, write, "")
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("write leave %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	miss, missHdr, missBody := call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/missing/comments", stranger, "")
	got, gotHdr, gotBody := call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/comments", write, "")
	if got != http.StatusNotFound || miss != got || !bytes.Equal(gotBody, missBody) || string(gotBody) != `{"error":"not_found"}` {
		t.Fatalf("former member %d %s missing %d %s", got, gotBody, miss, missBody)
	}
	if gotHdr.Get("Content-Type") != missHdr.Get("Content-Type") || gotHdr.Get("Location") != "" || missHdr.Get("Location") != "" {
		t.Fatalf("headers %v %v", gotHdr, missHdr)
	}
	if strings.Contains(callOK(t, p.h, http.MethodGet, "/v1/channels", write, ""), `"chat":"main"`) {
		t.Fatal("left channel still listed")
	}
	comments := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/comments", owner, "")
	if !strings.Contains(comments, `"key":"note"`) || !strings.Contains(comments, writeID) {
		t.Fatalf("entry removed %s", comments)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/blobs/"+id, owner, "")
	if status != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("blob left with the member %d %s", status, body)
	}
	ownerList := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, "")
	if strings.Contains(ownerList, writeID) || !strings.Contains(ownerList, readID) {
		t.Fatalf("members %s", ownerList)
	}

	status, hdr, body = call(p.h, http.MethodDelete, path, read, "")
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("read leave %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	if strings.Contains(callOK(t, p.h, http.MethodGet, "/v1/channels", read, ""), `"chat":"main"`) {
		t.Fatal("read member still listed")
	}
}

func TestMembershipAccess(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	path := "/v1/channels/" + ownerID + "/main/membership"
	missing := "/v1/channels/" + ownerID + "/missing/membership"
	expectWithoutRead(t, p.h, http.MethodDelete, path, "", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	sameNotFound(t, p.h, http.MethodDelete, path, missing, stranger, "")
	status, hdr, body := call(p.h, http.MethodDelete, path, write, "")
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("write %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	status, _, body = call(p.h, http.MethodDelete, path, read, "")
	if status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("read %d %s", status, body)
	}
}

func TestMembershipWriteFailureStaysInternal(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID := inst(0x11), inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, "")
	dir := filepath.Join(p.dir, "channels", ownerID, "main")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodDelete, "/v1/channels/"+ownerID+"/main/membership", write, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` {
		t.Fatalf("write %d %s", status, body)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listed := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, "")
	if !strings.Contains(listed, writeID) {
		t.Fatalf("member dropped %s", listed)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/comments", write, "")
	if status != http.StatusOK {
		t.Fatalf("member lost access %d %s", status, body)
	}
}

func expectWithoutRead(t *testing.T, h http.Handler, method, path, token string, want int, wantBody string) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
	})
	entered := make(chan struct{})
	done := make(chan struct {
		status int
		hdr    http.Header
		body   string
	}, 1)
	go func() {
		status, hdr, body := do(h, method, path, token, &signalReader{r: pr, entered: entered})
		done <- struct {
			status int
			hdr    http.Header
			body   string
		}{status, hdr, string(body)}
	}()
	select {
	case <-entered:
		t.Fatalf("%s %s read the body", method, path)
	case got := <-done:
		select {
		case <-entered:
			t.Fatalf("%s %s read the body", method, path)
		default:
		}
		if got.status != want || (wantBody != "" && got.body != wantBody) || (want == http.StatusNoContent && got.body != "") {
			t.Fatalf("%s %s %d %s", method, path, got.status, got.body)
		}
		if wantBody != "" || want == http.StatusNoContent {
			assertJSONHeaders(t, got.hdr)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s %s timed out", method, path)
	}
}

func sameNotFound(t *testing.T, h http.Handler, method, real, missing, token, body string) {
	t.Helper()
	s1, h1, b1 := call(h, method, real, token, body)
	s2, h2, b2 := call(h, method, missing, token, body)
	if s1 != http.StatusNotFound || s2 != s1 || !bytes.Equal(b1, b2) || string(b1) != `{"error":"not_found"}` {
		t.Fatalf("%s real %d %s missing %d %s", method, s1, b1, s2, b2)
	}
	if h1.Get("Content-Type") != h2.Get("Content-Type") || h1.Get("Cache-Control") != h2.Get("Cache-Control") || h1.Get("X-Content-Type-Options") != h2.Get("X-Content-Type-Options") || h1.Get("Location") != "" || h2.Get("Location") != "" {
		t.Fatalf("headers %v %v", h1, h2)
	}
}
