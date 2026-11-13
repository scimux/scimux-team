package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

func TestBlobRoundTripOverHTTP(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	data := []byte("png-bytes")
	id := blobID(data)
	path := "/v1/channels/" + ownerID + "/main/blobs/" + id

	status, hdr, body := call(p.h, http.MethodPut, path, owner, string(data))
	if status != http.StatusCreated || string(body) != blobJSON(id, len(data)) {
		t.Fatalf("create %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	if hdr.Get("Location") != "" {
		t.Fatalf("location %q", hdr.Get("Location"))
	}

	status, hdr, body = call(p.h, http.MethodGet, path, read, "")
	if status != http.StatusOK || !bytes.Equal(body, data) || hdr.Get("Content-Type") != "application/octet-stream" || hdr.Get("Cache-Control") != "no-store" || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("get %d %q headers %v", status, body, hdr)
	}
	if hdr.Get("Content-Type") == "application/json" {
		t.Fatal("download was json")
	}

	status, hdr, body = call(p.h, http.MethodPut, path, write, string(data))
	if status != http.StatusOK || string(body) != blobJSON(id, len(data)) {
		t.Fatalf("duplicate %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	more := []byte("more-bytes")
	morePath := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(more)
	status, _, body = call(p.h, http.MethodPut, morePath, write, string(more))
	if status != http.StatusCreated || string(body) != blobJSON(blobID(more), len(more)) {
		t.Fatalf("write member %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, morePath, owner, "")
	if status != http.StatusOK || !bytes.Equal(body, more) {
		t.Fatalf("owner get %d %q", status, body)
	}

	status, hdr, body = call(p.h, http.MethodHead, path, owner, "")
	if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` {
		t.Fatalf("head %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	absent := blobID([]byte("absent"))
	status, hdr, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/blobs/"+absent, owner, "")
	if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` {
		t.Fatalf("missing %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)

	for _, bad := range []string{strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("ab", 31), id + "0"} {
		status, _, body = call(p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main/blobs/"+bad, owner, string(data))
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("put %q %d %s", bad, status, body)
		}
		status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/blobs/"+bad, owner, "")
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("get %q %d %s", bad, status, body)
		}
	}
	status, _, body = call(p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main/blobs/"+blobID([]byte("yep")), owner, "nope")
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("hash %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "main", "blobs", blobID([]byte("nope")))); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	status, _, body = call(p.h, http.MethodPut, path, owner, "")
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("empty %d %s", status, body)
	}
}

func TestBlobAccess(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	data := []byte("png-bytes")
	path := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(data)
	status, _, body := call(p.h, http.MethodPut, path, owner, string(data))
	if status != http.StatusCreated {
		t.Fatalf("seed %d %s", status, body)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", http.StatusUnauthorized},
		{stranger, http.StatusNotFound},
		{read, http.StatusOK},
		{write, http.StatusOK},
		{owner, http.StatusOK},
	} {
		status, hdr, body := call(p.h, http.MethodGet, path, tc.token, "")
		if status != tc.want {
			t.Fatalf("get token %t %d %s", tc.token != "", status, body)
		}
		if tc.want == http.StatusOK && !bytes.Equal(body, data) {
			t.Fatalf("bytes %q", body)
		}
		if tc.want != http.StatusOK && string(body) != `{"error":"`+httpError(tc.want)+`"}` {
			t.Fatalf("error %d %s", status, body)
		}
		if tc.want == http.StatusOK {
			if hdr.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("type %s", hdr.Get("Content-Type"))
			}
		} else {
			assertJSONHeaders(t, hdr)
		}
	}
	fresh := []byte("zz-other")
	freshPath := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(fresh)
	status, _, body = call(p.h, http.MethodPut, freshPath, read, string(fresh))
	if status != http.StatusForbidden || string(body) != `{"error":"forbidden"}` {
		t.Fatalf("read put %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodPut, freshPath, write, string(fresh))
	if status != http.StatusCreated || string(body) != blobJSON(blobID(fresh), len(fresh)) {
		t.Fatalf("write put %d %s", status, body)
	}
}

func TestBlobStrangerMatchesMissingChannel(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	id := blobID([]byte("png-bytes"))
	real := "/v1/channels/" + ownerID + "/main/blobs/" + id
	missing := "/v1/channels/" + ownerID + "/missing/blobs/" + id
	sameNotFound(t, p.h, http.MethodPut, real, missing, stranger, "png-bytes")
	sameNotFound(t, p.h, http.MethodGet, real, missing, stranger, "")
	sameNotFound(t, p.h, http.MethodDelete, "/v1/channels/"+ownerID+"/main/membership", "/v1/channels/"+ownerID+"/missing/membership", stranger, "")
}

func TestBlobUploadCheckOrder(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	path := "/v1/channels/" + ownerID + "/main/blobs/" + blobID([]byte("png-bytes"))
	expectWithoutRead(t, p.h, http.MethodPut, path, "", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	expectWithoutRead(t, p.h, http.MethodPut, path, stranger, http.StatusNotFound, `{"error":"not_found"}`)
	expectWithoutRead(t, p.h, http.MethodPut, path, read, http.StatusForbidden, `{"error":"forbidden"}`)
	bad := "/v1/channels/" + ownerID + "/main/blobs/" + strings.Repeat("A", 64)
	expectWithoutRead(t, p.h, http.MethodPut, bad, owner, http.StatusBadRequest, `{"error":"bad_request"}`)
}

func TestBlobUploadDoesNotHoldTheLock(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	data := []byte("png-bytes")
	path := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(data)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	entered := make(chan struct{})
	done := make(chan struct {
		status int
		body   string
	}, 1)
	go func() {
		status, _, body := do(p.h, http.MethodPut, path, owner, &signalReader{r: pr, entered: entered})
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
		t.Fatal("upload did not read")
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
		t.Fatal("hello blocked behind the upload body")
	}
	if _, err := pw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.status != http.StatusCreated || got.body != blobJSON(blobID(data), len(data)) {
			t.Fatalf("upload %d %s", got.status, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("upload did not finish")
	}
}

func TestBlobDownloadDoesNotHoldTheLock(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	data := []byte("png-bytes")
	id := blobID(data)
	if created, err := p.st.PutBlob(ownerID, "main", id, data); err != nil || !created {
		t.Fatal(err, created)
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	writer := &stallWriter{entered: entered, release: release, hdr: make(http.Header)}
	finished := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://relay.example/v1/channels/"+ownerID+"/main/blobs/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+owner)
		p.h.ServeHTTP(writer, req)
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("response was not written")
	}
	second := make(chan int, 1)
	go func() {
		status, _, _ := do(p.h, http.MethodGet, "/v1/hello", "", nil)
		second <- status
	}()
	select {
	case status := <-second:
		if status != http.StatusOK {
			t.Fatalf("hello %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("hello blocked behind the download")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("download did not finish")
	}
	if writer.hdr.Get("Content-Type") != "application/octet-stream" || writer.hdr.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("headers %v", writer.hdr)
	}
}

func TestBlobUploadRevokedDuringTheBody(t *testing.T) {
	p := newParty(t, channel.Options{})
	id := inst(0x11)
	token := p.seat(t, 0x11, "seat a")
	p.create(t, token, id, "main", "title-main")
	data := []byte("png-bytes")
	blob := blobID(data)
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 90}})
	status, _, body := callReader(p.h, http.MethodPut, "/v1/channels/"+id+"/main/blobs/"+blob, token, &readHook{
		r: bytes.NewReader(data),
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
		t.Fatalf("revoked %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", id, "main", "blobs", blob)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestBlobQuotaOverHTTP(t *testing.T) {
	p := newParty(t, channel.Options{MaxBlobsPerChannel: 1})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	first := []byte("one")
	path := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(first)
	status, hdr, body := call(p.h, http.MethodPut, path, owner, string(first))
	if status != http.StatusCreated || string(body) != blobJSON(blobID(first), len(first)) {
		t.Fatalf("first %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	second := []byte("two")
	status, hdr, body = call(p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main/blobs/"+blobID(second), owner, string(second))
	if status != http.StatusInsufficientStorage || string(body) != `{"error":"quota"}` {
		t.Fatalf("quota %d %s", status, body)
	}
	assertJSONHeaders(t, hdr)
	status, _, body = call(p.h, http.MethodPut, path, owner, string(first))
	if status != http.StatusOK || string(body) != blobJSON(blobID(first), len(first)) {
		t.Fatalf("duplicate at the limit %d %s", status, body)
	}
}

func TestBlobTooLargeStopsReading(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	entered := make(chan struct{})
	body := &blockAfter{limit: (4 << 20) + 1, entered: entered}
	done := make(chan struct {
		status int
		body   string
	}, 1)
	path := "/v1/channels/" + ownerID + "/main/blobs/" + strings.Repeat("ab", 32)
	go func() {
		status, _, got := callReader(p.h, http.MethodPut, path, owner, body)
		done <- struct {
			status int
			body   string
		}{status, string(got)}
	}()
	select {
	case <-entered:
		t.Fatal("read past 4 MiB plus one")
	case got := <-done:
		if got.status != http.StatusRequestEntityTooLarge || got.body != `{"error":"too_large"}` {
			t.Fatalf("too large %d %s", got.status, got.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not answer")
	}
	if entries, err := os.ReadDir(filepath.Join(p.dir, "channels", ownerID, "main", "blobs")); err == nil {
		for _, e := range entries {
			if e.Name() != ".." && e.Name() != "." {
				t.Fatalf("stored %s", e.Name())
			}
		}
	}
}

func TestBlobUploadReadError(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	id := blobID([]byte("png-bytes"))
	status, _, body := callReader(p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main/blobs/"+id, owner, failReader{})
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || strings.Contains(string(body), "read blocked") {
		t.Fatalf("read %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "main", "blobs", id)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDamagedBlobIsInternal(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, otherID := inst(0x11), inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "main", "title-main")
	p.create(t, other, otherID, "side", "title-side")
	keep := []byte("keep")
	if created, err := p.st.PutBlob(ownerID, "main", blobID(keep), keep); err != nil || !created {
		t.Fatal(err, created)
	}
	if created, err := p.st.PutBlob(otherID, "side", blobID(keep), keep); err != nil || !created {
		t.Fatal(err, created)
	}
	planted := []byte("yep")
	id := blobID(planted)
	path := filepath.Join(p.dir, "channels", ownerID, "main", "blobs", id)
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/blobs/"+id, owner, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("nope")) || bytes.Contains(body, []byte(id)) {
		t.Fatalf("damaged %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/blobs/"+blobID(keep), owner, "")
	if status != http.StatusOK || !bytes.Equal(body, keep) {
		t.Fatalf("sibling %d %q", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+otherID+"/side/blobs/"+blobID(keep), other, "")
	if status != http.StatusOK || !bytes.Equal(body, keep) {
		t.Fatalf("other %d %q", status, body)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, ""); !strings.Contains(got, `"chat":"main"`) {
		t.Fatalf("channel %s", got)
	}
}

func TestBlobSurvivesRestartAndIsNotPrinted(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + srv.addr
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x52}, 64))
	out, errOut, code, err := runWithin([]string{"-state-dir", dir, "issue"})
	if err != nil || code != 0 {
		t.Fatalf("issue %d %v %s %s", code, err, out, errOut)
	}
	join := strings.TrimSpace(out)
	status, resp := reqJSON(t, client, http.MethodPost, base+"/v1/register", "", `{"join_code":"`+join+`","installation_id":"`+inst(0x11)+`","name":"seat a"}`)
	if status != http.StatusCreated {
		t.Fatalf("register %d %s", status, resp)
	}
	var created registerResp
	if err := json.Unmarshal([]byte(resp), &created); err != nil {
		t.Fatal(err)
	}
	status, resp = reqJSON(t, client, http.MethodPut, base+"/v1/channels/"+inst(0x11)+"/main", created.Token, `{"title":"title-main"}`)
	if status != http.StatusCreated {
		t.Fatalf("channel %d %s", status, resp)
	}
	data := []byte("zz-blob-marker")
	id := blobID(data)
	status, hdr, raw := reqRaw(t, client, http.MethodPut, base+"/v1/channels/"+inst(0x11)+"/main/blobs/"+id, created.Token, data)
	if status != http.StatusCreated || string(raw) != blobJSON(id, len(data)) {
		t.Fatalf("put %d %s", status, raw)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("put type %s", hdr.Get("Content-Type"))
	}
	srv.wait(t)
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x53}, 64))
	again := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	status, hdr, raw = reqRaw(t, client, http.MethodGet, "http://"+again.addr+"/v1/channels/"+inst(0x11)+"/main/blobs/"+id, created.Token, nil)
	if status != http.StatusOK || !bytes.Equal(raw, data) {
		t.Fatalf("restart %d %q", status, raw)
	}
	if hdr.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("type %s", hdr.Get("Content-Type"))
	}
	leaked := srv.out.String() + srv.errb.String() + again.out.String() + again.errb.String()
	for _, secret := range []string{string(data), id, created.Token, join, "title-main"} {
		if strings.Contains(leaked, secret) {
			t.Fatalf("printed %s", secret)
		}
	}
	again.wait(t)
}

func blobID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func blobJSON(id string, size int) string {
	return `{"id":"` + id + `","size":` + strconv.Itoa(size) + `}`
}

func httpError(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusForbidden:
		return "forbidden"
	default:
		return ""
	}
}

type blockAfter struct {
	n       int
	limit   int
	entered chan struct{}
	once    sync.Once
}

func (b *blockAfter) Read(p []byte) (int, error) {
	if b.n >= b.limit {
		b.once.Do(func() { close(b.entered) })
		select {}
	}
	want := b.limit - b.n
	if want > len(p) {
		want = len(p)
	}
	for i := 0; i < want; i++ {
		p[i] = 'x'
	}
	b.n += want
	return want, nil
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("read blocked") }

func reqRaw(t *testing.T, client *http.Client, method, url, token string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, got
}
