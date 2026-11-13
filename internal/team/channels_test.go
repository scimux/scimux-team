package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

const (
	gen1  = "0001020304050607"
	gen2  = "08090a0b0c0d0e0f"
	stamp = "2026-11-12T09:30:00.000Z"
)

func TestChannelAccess(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	p.create(t, owner, ownerID, "gone", "title-gone")
	p.members(t, owner, ownerID, "gone", writeID, readID)

	main := "/v1/channels/" + ownerID + "/main"
	gone := "/v1/channels/" + ownerID + "/gone"
	one := `{"entries":[{"key":"k","body":{"n":1}}]}`
	title := `{"title":"title-main"}`
	memberBody := `{"members":[{"seat":"` + writeID + `","access":"write"},{"seat":"` + readID + `","access":"read"}]}`

	cases := []struct {
		method, path, token, body string
		want                      int
		errCode                   string
	}{
		{http.MethodGet, "/v1/channels", owner, "", http.StatusOK, ""},
		{http.MethodGet, "/v1/channels", write, "", http.StatusOK, ""},
		{http.MethodGet, "/v1/channels", read, "", http.StatusOK, ""},
		{http.MethodGet, "/v1/channels", stranger, "", http.StatusOK, ""},
		{http.MethodGet, "/v1/channels", "", "", http.StatusUnauthorized, "unauthorized"},

		{http.MethodPut, main, owner, title, http.StatusOK, ""},
		{http.MethodPut, main, write, title, http.StatusForbidden, "forbidden"},
		{http.MethodPut, main, read, title, http.StatusForbidden, "forbidden"},
		{http.MethodPut, main, stranger, title, http.StatusNotFound, "not_found"},
		{http.MethodPut, main, "", title, http.StatusUnauthorized, "unauthorized"},

		{http.MethodPut, main + "/members", owner, memberBody, http.StatusOK, ""},
		{http.MethodPut, main + "/members", write, memberBody, http.StatusForbidden, "forbidden"},
		{http.MethodPut, main + "/members", read, memberBody, http.StatusForbidden, "forbidden"},
		{http.MethodPut, main + "/members", stranger, memberBody, http.StatusNotFound, "not_found"},
		{http.MethodPut, main + "/members", "", memberBody, http.StatusUnauthorized, "unauthorized"},

		{http.MethodGet, main + "/mirror", owner, "", http.StatusOK, ""},
		{http.MethodGet, main + "/mirror", write, "", http.StatusOK, ""},
		{http.MethodGet, main + "/mirror", read, "", http.StatusOK, ""},
		{http.MethodGet, main + "/mirror", stranger, "", http.StatusNotFound, "not_found"},
		{http.MethodGet, main + "/mirror", "", "", http.StatusUnauthorized, "unauthorized"},

		{http.MethodPost, main + "/mirror", owner, one, http.StatusOK, ""},
		{http.MethodPost, main + "/mirror", write, one, http.StatusForbidden, "forbidden"},
		{http.MethodPost, main + "/mirror", read, one, http.StatusForbidden, "forbidden"},
		{http.MethodPost, main + "/mirror", stranger, one, http.StatusNotFound, "not_found"},
		{http.MethodPost, main + "/mirror", "", one, http.StatusUnauthorized, "unauthorized"},

		{http.MethodGet, main + "/comments", owner, "", http.StatusOK, ""},
		{http.MethodGet, main + "/comments", write, "", http.StatusOK, ""},
		{http.MethodGet, main + "/comments", read, "", http.StatusOK, ""},
		{http.MethodGet, main + "/comments", stranger, "", http.StatusNotFound, "not_found"},
		{http.MethodGet, main + "/comments", "", "", http.StatusUnauthorized, "unauthorized"},

		{http.MethodPost, main + "/comments", owner, one, http.StatusOK, ""},
		{http.MethodPost, main + "/comments", write, `{"entries":[{"key":"w","body":{"n":2}}]}`, http.StatusOK, ""},
		{http.MethodPost, main + "/comments", read, one, http.StatusForbidden, "forbidden"},
		{http.MethodPost, main + "/comments", stranger, one, http.StatusNotFound, "not_found"},
		{http.MethodPost, main + "/comments", "", one, http.StatusUnauthorized, "unauthorized"},

		{http.MethodDelete, gone, write, "", http.StatusForbidden, "forbidden"},
		{http.MethodDelete, gone, read, "", http.StatusForbidden, "forbidden"},
		{http.MethodDelete, gone, stranger, "", http.StatusNotFound, "not_found"},
		{http.MethodDelete, gone, "", "", http.StatusUnauthorized, "unauthorized"},
		{http.MethodDelete, gone, owner, "", http.StatusNoContent, ""},
	}
	for _, tc := range cases {
		status, hdr, body := call(p.h, tc.method, tc.path, tc.token, tc.body)
		if status != tc.want || (tc.errCode != "" && string(body) != `{"error":"`+tc.errCode+`"}`) || (tc.want == http.StatusNoContent && len(body) != 0) || hdr.Get("Location") != "" {
			t.Fatalf("%s %s token %t: %d %s", tc.method, tc.path, tc.token != "", status, body)
		}
		assertJSONHeaders(t, hdr)
	}

	// The list carries members only for the owner, and a stranger sees nothing.
	ownerList := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, "")
	if !strings.Contains(ownerList, `"access":"owner"`) || !strings.Contains(ownerList, `"members":[`) || !strings.Contains(ownerList, `"chat":"main"`) || strings.Contains(ownerList, `"chat":"gone"`) {
		t.Fatalf("owner list %s", ownerList)
	}
	writeList := callOK(t, p.h, http.MethodGet, "/v1/channels", write, "")
	if !strings.Contains(writeList, `"access":"write"`) || strings.Contains(writeList, `"members"`) || !strings.Contains(writeList, `"chat":"main"`) {
		t.Fatalf("write list %s", writeList)
	}
	readList := callOK(t, p.h, http.MethodGet, "/v1/channels", read, "")
	if !strings.Contains(readList, `"access":"read"`) || strings.Contains(readList, `"members"`) {
		t.Fatalf("read list %s", readList)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", stranger, ""); got != `{"channels":[]}` {
		t.Fatalf("stranger list %s", got)
	}

	// A method or path this stage does not register stays the constant 404.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/channels"},
		{http.MethodHead, "/v1/channels"},
		{http.MethodHead, main + "/mirror"},
		{http.MethodHead, main + "/comments"},
		{http.MethodGet, main},
		{http.MethodDelete, main + "/comments"},
		{http.MethodOptions, main + "/mirror"},
	} {
		status, hdr, body := call(p.h, tc.method, tc.path, owner, `{"title":"x"}`)
		if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` || hdr.Get("Location") != "" {
			t.Fatalf("unlisted %s %s %d %s location %q", tc.method, tc.path, status, body, hdr.Get("Location"))
		}
	}
}

func TestStrangerMatchesMissingChannel(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")

	real := "/v1/channels/" + ownerID + "/main"
	missing := "/v1/channels/" + ownerID + "/missing"
	bad := `{"title":`
	routes := []struct{ method, suffix, body string }{
		{http.MethodPut, "", bad},
		{http.MethodDelete, "", ""},
		{http.MethodPut, "/members", bad},
		{http.MethodGet, "/mirror", ""},
		{http.MethodPost, "/mirror", bad},
		{http.MethodGet, "/comments", ""},
		{http.MethodPost, "/comments", bad},
	}
	for _, tc := range routes {
		s1, h1, b1 := call(p.h, tc.method, real+tc.suffix, stranger, tc.body)
		s2, h2, b2 := call(p.h, tc.method, missing+tc.suffix, stranger, tc.body)
		if s1 != http.StatusNotFound || s2 != s1 || !bytes.Equal(b1, b2) || string(b1) != `{"error":"not_found"}` {
			t.Fatalf("%s %s real %d %s missing %d %s", tc.method, tc.suffix, s1, b1, s2, b2)
		}
		if h1.Get("Content-Type") != h2.Get("Content-Type") || h1.Get("Cache-Control") != h2.Get("Cache-Control") || h1.Get("X-Content-Type-Options") != h2.Get("X-Content-Type-Options") || h1.Get("Location") != "" || h2.Get("Location") != "" {
			t.Fatalf("headers %v %v", h1, h2)
		}
	}
}

func TestChannelCheckOrder(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	stranger := p.seat(t, 0x44, "seat d")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, readID)
	path := "/v1/channels/" + ownerID + "/main/comments"
	bad := `{"entries":`
	status, _, body := call(p.h, http.MethodPost, path, "", bad)
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("no token %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodPost, path, stranger, bad)
	if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` {
		t.Fatalf("stranger %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodPost, path, read, bad)
	if status != http.StatusForbidden || string(body) != `{"error":"forbidden"}` {
		t.Fatalf("read member %d %s", status, body)
	}
	// A bad path is content, so it loses to authentication and beats visibility.
	status, _, body = call(p.h, http.MethodPut, "/v1/channels/abc/a.b", "", `{"title":"x"}`)
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("bad path anonymous %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/mirror?after=nope", stranger, "")
	if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` {
		t.Fatalf("bad query stranger %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/a.b/mirror", stranger, "")
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("bad path stranger %d %s", status, body)
	}
	mirror := "/v1/channels/" + ownerID + "/main/mirror?after=nope"
	status, _, body = call(p.h, http.MethodGet, mirror, read, "")
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("bad query member %d %s", status, body)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/mirror", read, ""); !strings.Contains(got, `"entries":[]`) {
		t.Fatalf("log changed %s", got)
	}
}

func TestChannelJSONShapes(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID, readID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	read := p.seat(t, 0x33, "seat c")
	created := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/a-chat", owner, `{"title":"title-a"}`)
	want := channelJSON(ownerID, "a-chat", "title-a", gen1, "owner", 0, 0, "[]")
	if created != want {
		t.Fatalf("create %s\nwant %s", created, want)
	}
	updated := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/a-chat", owner, `{"title":"title-b"}`)
	if updated != channelJSON(ownerID, "a-chat", "title-b", gen1, "owner", 0, 0, "[]") {
		t.Fatalf("update %s", updated)
	}
	empty := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/b-chat", owner, `{"title":""}`)
	if empty != channelJSON(ownerID, "b-chat", "", gen2, "owner", 0, 0, "[]") {
		t.Fatalf("empty title %s", empty)
	}
	members := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/a-chat/members", owner,
		`{"members":[{"seat":"`+readID+`","access":"read"},{"seat":"`+writeID+`","access":"write"}]}`)
	listed := `[{"seat":"` + writeID + `","access":"write"},{"seat":"` + readID + `","access":"read"}]`
	if members != channelJSON(ownerID, "a-chat", "title-b", gen1, "owner", 0, 0, listed) {
		t.Fatalf("members %s", members)
	}
	// b-chat then a-chat would be wrong: owner order, then chat order.
	list := callOK(t, p.h, http.MethodGet, "/v1/channels", owner, "")
	wantList := `{"channels":[` + channelJSON(ownerID, "a-chat", "title-b", gen1, "owner", 0, 0, listed) + `,` + channelJSON(ownerID, "b-chat", "", gen2, "owner", 0, 0, "[]") + `]}`
	if list != wantList {
		t.Fatalf("list %s\nwant %s", list, wantList)
	}
	asWrite := callOK(t, p.h, http.MethodGet, "/v1/channels", write, "")
	if asWrite != `{"channels":[`+channelJSON(ownerID, "a-chat", "title-b", gen1, "write", 0, 0, "")+`]}` {
		t.Fatalf("write view %s", asWrite)
	}

	posted := callOK(t, p.h, http.MethodPost, "/v1/channels/"+ownerID+"/a-chat/mirror", owner, `{"entries":[{"key":"k/1","body":{"a":"<b>"}}]}`)
	if posted != `{"generation":"`+gen1+`","results":[{"key":"k/1","seq":1,"duplicate":false}],"last":1}` {
		t.Fatalf("append %s", posted)
	}
	stored, err := os.ReadFile(filepath.Join(p.dir, "channels", ownerID, "a-chat", "mirror.jsonl"))
	if err != nil || !bytes.Contains(stored, []byte(`{"a":"<b>"}`)) || bytes.Contains(stored, []byte(`\u003c`)) {
		t.Fatalf("stored %s %v", stored, err)
	}
	got := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/a-chat/mirror", read, "")
	// encoding/json escapes <, > and & in the response. The value is unchanged.
	entry := `{"seq":1,"key":"k/1","author":"` + ownerID + `","at":"` + stamp + `","body":{"a":"\u003cb\u003e"}}`
	if got != `{"generation":"`+gen1+`","entries":[`+entry+`],"last":1,"more":false}` {
		t.Fatalf("read %s", got)
	}
	again := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/a-chat/mirror?after=0", write, "")
	if again != got {
		t.Fatalf("after 0 %s", again)
	}
	tail := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/a-chat/mirror?after=1", owner, "")
	if tail != `{"generation":"`+gen1+`","entries":[],"last":1,"more":false}` {
		t.Fatalf("tail %s", tail)
	}
}

func TestChannelValidationAndLimits(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "main", "title-main")
	base := "/v1/channels/" + ownerID + "/main"

	badPut := []string{
		"/v1/channels/abc/main",
		"/v1/channels/" + strings.Repeat("g", 32) + "/main",
		"/v1/channels/" + strings.ToUpper(inst(0xab)) + "/main",
		"/v1/channels/" + ownerID + "/a.b",
		"/v1/channels/" + ownerID + "/" + strings.Repeat("a", 65),
	}
	for _, path := range badPut {
		status, _, body := call(p.h, http.MethodPut, path, owner, `{"title":"x"}`)
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("path %s %d %s", path, status, body)
		}
	}
	longTitle := strings.Repeat("a", 200)
	if got := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/"+strings.Repeat("b", 64), owner, `{"title":"`+longTitle+`"}`); !strings.Contains(got, `"title":"`+longTitle+`"`) {
		t.Fatalf("boundary %s", got)
	}
	for _, body := range []string{
		`{"title":"` + strings.Repeat("a", 201) + `"}`,
		`{"title":"bad` + "\n" + `"}`,
		`{"title":"x","extra":1}`,
		`{"title":"x"}{"title":"y"}`,
		`{"title":`,
		``,
	} {
		status, _, got := call(p.h, http.MethodPut, base, owner, body)
		if status != http.StatusBadRequest || string(got) != `{"error":"bad_request"}` {
			t.Fatalf("title %q %d %s", body, status, got)
		}
	}

	status, _, body := call(p.h, http.MethodPost, base+"/mirror", owner, `{"entries":[{"key":"`+strings.Repeat("a", 200)+`","body":null}]}`)
	if status != http.StatusOK {
		t.Fatalf("long key %d %s", status, body)
	}
	for _, entries := range []string{
		`{"entries":[]}`,
		`{"entries":[{"key":"","body":1}]}`,
		`{"entries":[{"key":"a b","body":1}]}`,
		`{"entries":[{"key":"` + strings.Repeat("a", 201) + `","body":1}]}`,
		`{"entries":[{"key":"k","body":1},{"key":"bad key","body":2}]}`,
		`{"entries":[{"key":"k","body":1,"extra":true}]}`,
		`{"entries":[{"key":"k","body":1}],"extra":1}`,
		`{"entries":[{"key":"k","body":1}]}{"more":true}`,
		`{`,
	} {
		before := callOK(t, p.h, http.MethodGet, base+"/mirror", owner, "")
		status, _, got := call(p.h, http.MethodPost, base+"/mirror", owner, entries)
		if status != http.StatusBadRequest || string(got) != `{"error":"bad_request"}` {
			t.Fatalf("entries %s %d %s", entries, status, got)
		}
		if after := callOK(t, p.h, http.MethodGet, base+"/mirror", owner, ""); after != before {
			t.Fatalf("stored %s on %s", after, entries)
		}
	}
	wide := make([]string, 257)
	for i := range wide {
		wide[i] = fmt.Sprintf(`{"key":"k%03d","body":1}`, i)
	}
	status, _, body = call(p.h, http.MethodPost, base+"/mirror", owner, `{"entries":[`+strings.Join(wide, ",")+`]}`)
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("batch %d %s", status, body)
	}

	over := jsonText(4<<20 + 1)
	status, _, body = call(p.h, http.MethodPost, base+"/comments", owner, `{"entries":[{"key":"ok","body":1},{"key":"big","body":`+over+`}]}`)
	if status != http.StatusRequestEntityTooLarge || string(body) != `{"error":"too_large"}` {
		t.Fatalf("entry %d %s", status, body)
	}
	if got := callOK(t, p.h, http.MethodGet, base+"/comments", owner, ""); !strings.Contains(got, `"entries":[]`) || !strings.Contains(got, `"last":0`) {
		t.Fatalf("oversized stored %s", got)
	}
	status, _, body = call(p.h, http.MethodPost, base+"/mirror", owner, string(bytes.Repeat([]byte{' '}, maxBodyBytes+1)))
	if status != http.StatusRequestEntityTooLarge || string(body) != `{"error":"too_large"}` {
		t.Fatalf("request %d %s", status, body)
	}

	// unknown_member only after the list is structurally valid.
	status, _, body = call(p.h, http.MethodPut, base+"/members", owner, `{"members":[{"seat":"nope","access":"nope"}]}`)
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("structural %d %s", status, body)
	}
	for _, members := range []string{
		`{"members":[{"seat":"` + ownerID + `","access":"read"}]}`,
		`{"members":[{"seat":"` + inst(0x22) + `","access":"admin"}]}`,
		`{"members":[{"seat":"` + inst(0x22) + `","access":"read"},{"seat":"` + inst(0x22) + `","access":"write"}]}`,
		`{}`,
		`{"members":null}`,
	} {
		status, _, body = call(p.h, http.MethodPut, base+"/members", owner, members)
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("members %s %d %s", members, status, body)
		}
	}
	var many []string
	for i := 1; i <= 201; i++ {
		many = append(many, fmt.Sprintf(`{"seat":"%032x","access":"read"}`, i))
	}
	status, _, body = call(p.h, http.MethodPut, base+"/members", owner, `{"members":[`+strings.Join(many, ",")+`]}`)
	if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
		t.Fatalf("too many %d %s", status, body)
	}
	for _, members := range []string{`{`, `{"members":[],"extra":1}`, `{"members":[]}{"x":1}`} {
		status, _, body = call(p.h, http.MethodPut, base+"/members", owner, members)
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("members json %s %d %s", members, status, body)
		}
	}
	status, _, body = call(p.h, http.MethodPut, base+"/members", owner, `{"members":[{"seat":"`+inst(0x99)+`","access":"read"}]}`)
	if status != http.StatusUnprocessableEntity || string(body) != `{"error":"unknown_member"}` {
		t.Fatalf("unknown %d %s", status, body)
	}
	// The other seat is registered, so this one is accepted and sorted.
	got := callOK(t, p.h, http.MethodPut, base+"/members", owner, `{"members":[{"seat":"`+inst(0x22)+`","access":"read"}]}`)
	if !strings.Contains(got, `"members":[{"seat":"`+inst(0x22)+`","access":"read"}]`) {
		t.Fatalf("accepted %s", got)
	}
	cleared := callOK(t, p.h, http.MethodPut, base+"/members", owner, `{"members":[]}`)
	if !strings.Contains(cleared, `"members":[]`) {
		t.Fatalf("cleared %s", cleared)
	}
	_ = other
}

func TestAppendIsAtomicAndIdempotent(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, writeID := inst(0x11), inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	write := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "main", "title-main")
	p.members(t, owner, ownerID, "main", writeID, "")
	mirror := "/v1/channels/" + ownerID + "/main/mirror"
	comments := "/v1/channels/" + ownerID + "/main/comments"

	first := callOK(t, p.h, http.MethodPost, mirror, owner, `{"entries":[{"key":"same","body":{"n":1}},{"key":"same","body":{"n":2}}]}`)
	if first != `{"generation":"`+gen1+`","results":[{"key":"same","seq":1,"duplicate":false},{"key":"same","seq":1,"duplicate":true}],"last":1}` {
		t.Fatalf("batch dup %s", first)
	}
	again := callOK(t, p.h, http.MethodPost, mirror, owner, `{"entries":[{"key":"same","body":{"n":9}}]}`)
	if again != `{"generation":"`+gen1+`","results":[{"key":"same","seq":1,"duplicate":true}],"last":1}` {
		t.Fatalf("retry %s", again)
	}
	got := callOK(t, p.h, http.MethodGet, mirror, owner, "")
	if !strings.Contains(got, `"body":{"n":1}`) || strings.Contains(got, `"n":2`) || strings.Contains(got, `"n":9`) {
		t.Fatalf("first body lost %s", got)
	}

	// Comment keys are per author: the same key from two seats is two entries.
	ownerComment := callOK(t, p.h, http.MethodPost, comments, owner, `{"entries":[{"key":"same","body":1}]}`)
	writeComment := callOK(t, p.h, http.MethodPost, comments, write, `{"entries":[{"key":"same","body":2}]}`)
	if !strings.Contains(ownerComment, `"duplicate":false`) || !strings.Contains(writeComment, `"seq":2,"duplicate":false`) {
		t.Fatalf("per author %s %s", ownerComment, writeComment)
	}

	// Quota: one entry fills the log, a pair stores nothing, a retry still resolves.
	line := oneLine(t, ownerID)
	q := newParty(t, channel.Options{MaxLogBytes: line})
	qOwner := q.seat(t, 0x11, "seat a")
	q.create(t, qOwner, ownerID, "main", "title-main")
	qMirror := "/v1/channels/" + ownerID + "/main/mirror"
	if status, _, body := call(q.h, http.MethodPost, qMirror, qOwner, `{"entries":[{"key":"a","body":1}]}`); status != http.StatusOK {
		t.Fatalf("fill %d %s", status, body)
	}
	status, _, body := call(q.h, http.MethodPost, qMirror, qOwner, `{"entries":[{"key":"b","body":1},{"key":"c","body":1}]}`)
	if status != http.StatusInsufficientStorage || string(body) != `{"error":"quota"}` {
		t.Fatalf("quota %d %s", status, body)
	}
	got = callOK(t, q.h, http.MethodGet, qMirror, qOwner, "")
	if strings.Contains(got, `"key":"b"`) || strings.Contains(got, `"key":"c"`) || !strings.Contains(got, `"last":1`) {
		t.Fatalf("quota wrote %s", got)
	}
	dup := callOK(t, q.h, http.MethodPost, qMirror, qOwner, `{"entries":[{"key":"a","body":9}]}`)
	if !strings.Contains(dup, `"seq":1,"duplicate":true`) {
		t.Fatalf("dup at limit %s", dup)
	}

	limited := newParty(t, channel.Options{MaxChannelsPerOwner: 1})
	lim := limited.seat(t, 0x11, "seat a")
	limited.create(t, lim, ownerID, "one", "title-one")
	status, _, body = call(limited.h, http.MethodPut, "/v1/channels/"+ownerID+"/two", lim, `{"title":"title-two"}`)
	if status != http.StatusInsufficientStorage || string(body) != `{"error":"quota"}` {
		t.Fatalf("channel quota %d %s", status, body)
	}
	if got := callOK(t, limited.h, http.MethodGet, "/v1/channels", lim, ""); strings.Contains(got, `"chat":"two"`) || !strings.Contains(got, `"chat":"one"`) {
		t.Fatalf("quota created %s", got)
	}
}

func TestReadAfterAndMore(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	mirror := "/v1/channels/" + ownerID + "/main/mirror"
	var batch []string
	for i := 0; i < 256; i++ {
		batch = append(batch, fmt.Sprintf(`{"key":"k%03d","body":%d}`, i, i))
	}
	if status, _, body := call(p.h, http.MethodPost, mirror, owner, `{"entries":[`+strings.Join(batch, ",")+`]}`); status != http.StatusOK {
		t.Fatalf("batch %d %s", status, body)
	}
	if status, _, body := call(p.h, http.MethodPost, mirror, owner, `{"entries":[{"key":"tail","body":true}]}`); status != http.StatusOK {
		t.Fatalf("tail %d %s", status, body)
	}
	var page readResp
	if err := json.Unmarshal([]byte(callOK(t, p.h, http.MethodGet, mirror, owner, "")), &page); err != nil {
		t.Fatal(err)
	}
	if !page.More || page.Last != 257 || len(page.Entries) != 256 || page.Entries[0].Seq != 1 || page.Entries[255].Seq != 256 || page.Generation != gen1 {
		t.Fatalf("page %+v", page)
	}
	var rest readResp
	if err := json.Unmarshal([]byte(callOK(t, p.h, http.MethodGet, mirror+"?after=256", owner, "")), &rest); err != nil {
		t.Fatal(err)
	}
	if rest.More || rest.Last != 257 || len(rest.Entries) != 1 || rest.Entries[0].Seq != 257 || rest.Entries[0].Key != "tail" {
		t.Fatalf("rest %+v", rest)
	}
	for _, q := range []string{"?after=", "?after=1&after=2", "?after=-1", "?after=18446744073709551616"} {
		status, _, body := call(p.h, http.MethodGet, mirror+q, owner, "")
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("query %s %d %s", q, status, body)
		}
	}
}

func TestGenerationRestartsAfterDelete(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	first := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main", owner, `{"title":"title-main"}`)
	if !strings.Contains(first, `"generation":"`+gen1+`"`) {
		t.Fatalf("first %s", first)
	}
	callOK(t, p.h, http.MethodPost, "/v1/channels/"+ownerID+"/main/mirror", owner, `{"entries":[{"key":"k","body":1}]}`)
	status, hdr, body := call(p.h, http.MethodDelete, "/v1/channels/"+ownerID+"/main", owner, "")
	if status != http.StatusNoContent || len(body) != 0 || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("delete %d %s", status, body)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/mirror", owner, "")
	if status != http.StatusNotFound || string(body) != `{"error":"not_found"}` {
		t.Fatalf("still there %d %s", status, body)
	}
	second := callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/main", owner, `{"title":"title-new"}`)
	if second != channelJSON(ownerID, "main", "title-new", gen2, "owner", 0, 0, "[]") {
		t.Fatalf("second %s", second)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "main", "mirror.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestRevocationRemovesChannelsAndMemberships(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, otherID, memberID := inst(0x11), inst(0x22), inst(0x33)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.seat(t, 0x33, "seat c")
	p.create(t, owner, ownerID, "alpha", "title-alpha")
	p.create(t, other, otherID, "beta", "title-beta")
	callOK(t, p.h, http.MethodPut, "/v1/channels/"+ownerID+"/alpha/members", owner, `{"members":[{"seat":"`+otherID+`","access":"read"},{"seat":"`+memberID+`","access":"write"}]}`)
	callOK(t, p.h, http.MethodPut, "/v1/channels/"+otherID+"/beta/members", other, `{"members":[{"seat":"`+ownerID+`","access":"read"},{"seat":"`+memberID+`","access":"write"}]}`)

	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 80}})
	if _, err := second.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Revoke(ownerID); err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodGet, "/v1/channels", owner, "")
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("revoked token %d %s", status, body)
	}
	list := callOK(t, p.h, http.MethodGet, "/v1/channels", other, "")
	if strings.Contains(list, `"chat":"alpha"`) || strings.Contains(list, ownerID) || !strings.Contains(list, `"chat":"beta"`) || !strings.Contains(list, memberID) {
		t.Fatalf("after revoke %s", list)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha remains %v", err)
	}
}

func TestReregisteredSeatInheritsNothing(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, otherID := inst(0x11), inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "alpha", "title-alpha")
	p.create(t, other, otherID, "beta", "title-beta")
	callOK(t, p.h, http.MethodPut, "/v1/channels/"+otherID+"/beta/members", other, `{"members":[{"seat":"`+ownerID+`","access":"write"}]}`)
	code, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 90}})
	saveTeamSeams(t)
	beforeRegister = func() {
		if _, err := second.Refresh(); err != nil {
			t.Errorf("refresh %v", err)
		}
		if _, err := second.Revoke(ownerID); err != nil {
			t.Errorf("revoke %v", err)
		}
	}
	status, _, body := call(p.h, http.MethodPost, "/v1/register", "", `{"join_code":"`+code+`","installation_id":"`+ownerID+`","name":"seat a2"}`)
	if status != http.StatusCreated {
		t.Fatalf("register %d %s", status, body)
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.Token == owner {
		t.Fatalf("token %q", created.Token)
	}
	status, _, body = call(p.h, http.MethodGet, "/v1/channels", owner, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("old token %d %s", status, body)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", created.Token, ""); got != `{"channels":[]}` {
		t.Fatalf("inherited %s", got)
	}
	list := callOK(t, p.h, http.MethodGet, "/v1/channels", other, "")
	if strings.Contains(list, ownerID) || !strings.Contains(list, `"chat":"beta"`) || !strings.Contains(list, `"members":[]`) {
		t.Fatalf("other channel %s", list)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha remains %v", err)
	}
}

func TestRegisterReconcileFailureIsInternal(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "alpha", "title-secret")
	code, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 90}})
	saveTeamSeams(t)
	beforeRegister = func() {
		if _, err := second.Refresh(); err != nil {
			t.Errorf("refresh %v", err)
		}
		if _, err := second.Revoke(ownerID); err != nil {
			t.Errorf("revoke %v", err)
		}
		if err := p.st.Close(); err != nil {
			t.Errorf("close %v", err)
		}
	}
	status, _, body := call(p.h, http.MethodPost, "/v1/register", "", `{"join_code":"`+code+`","installation_id":"`+ownerID+`","name":"seat a2"}`)
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || strings.Contains(string(body), "title-secret") {
		t.Fatalf("reconcile %d %s", status, body)
	}
}

func TestRefreshReconcileFailureIsInternal(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "alpha", "title-secret")
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 80}})
	if _, err := second.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Revoke(ownerID); err != nil {
		t.Fatal(err)
	}
	if err := p.st.Close(); err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodGet, "/v1/channels", other, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("title-secret")) {
		t.Fatalf("refresh reconcile %d %s", status, body)
	}
}

func TestFailedCleanupIsRetriedAndRegisterSpendsNoCode(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID, otherID := inst(0x11), inst(0x22)
	owner := p.seat(t, 0x11, "seat a")
	other := p.seat(t, 0x22, "seat b")
	p.create(t, owner, ownerID, "alpha", "title-alpha")
	p.create(t, other, otherID, "beta", "title-beta")
	callOK(t, p.h, http.MethodPut, "/v1/channels/"+otherID+"/beta/members", other, `{"members":[{"seat":"`+ownerID+`","access":"read"}]}`)
	code, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 120}})
	if _, err := second.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Revoke(ownerID); err != nil {
		t.Fatal(err)
	}
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
	status, _, body := call(p.h, http.MethodGet, "/v1/channels", other, "")
	internal(status, body)
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); err != nil {
		t.Fatal(err)
	}
	regBody := `{"join_code":"` + code + `","installation_id":"` + ownerID + `","name":"seat a2"}`
	status, _, body = call(p.h, http.MethodPost, "/v1/register", "", regBody)
	internal(status, body)
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); err != nil {
		t.Fatal(err)
	}
	status, _, body = call(p.h, http.MethodPost, "/v1/register", "", regBody)
	if status != http.StatusCreated {
		t.Fatalf("register %d %s", status, body)
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", created.Token, ""); got != `{"channels":[]}` {
		t.Fatalf("inherited %s", got)
	}
	list := callOK(t, p.h, http.MethodGet, "/v1/channels", other, "")
	if strings.Contains(list, ownerID) || strings.Contains(list, `"chat":"alpha"`) || !strings.Contains(list, `"chat":"beta"`) || !strings.Contains(list, `"members":[]`) {
		t.Fatalf("after retry %s", list)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha remains %v", err)
	}

	// Register itself reports the revoked id. The cleanup has to remember
	// that id, because the new seat is already active.
	again, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	beforeRegister = func() {
		if _, err := second.Refresh(); err != nil {
			t.Errorf("refresh %v", err)
		}
		if _, err := second.Revoke(otherID); err != nil {
			t.Errorf("revoke %v", err)
		}
	}
	left = 1
	status, _, body = call(p.h, http.MethodPost, "/v1/register", "", `{"join_code":"`+again+`","installation_id":"`+otherID+`","name":"seat b2"}`)
	internal(status, body)
	if _, err := os.Stat(filepath.Join(p.dir, "channels", otherID, "beta")); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", created.Token, ""); got != `{"channels":[]}` {
		t.Fatalf("kept %s", got)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", otherID, "beta")); !os.IsNotExist(err) {
		t.Fatalf("beta remains %v", err)
	}
}

func TestStartupReconcileRunsBeforeServe(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	reg := openRegistry(t, dir, registry.Options{Name: "relay a", Now: fixedNow, Entropy: &seqReader{}})
	st, err := channel.Open(dir, channel.Options{Now: fixedNow, Entropy: &seqReader{}})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, otherID, memberID := inst(0x11), inst(0x22), inst(0x33)
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	ownerTok, _, err := reg.Register(code, ownerID, "seat a")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err = reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _, err := reg.Register(code, otherID, "seat b")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err = reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Register(code, memberID, "seat c"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(ownerID, "alpha", "title-alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(otherID, "beta", "title-beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetMembers(otherID, "beta", []channel.Member{{Seat: ownerID, Access: "read"}, {Seat: memberID, Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	second := openRegistry(t, dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 80}})
	if _, err := second.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Revoke(ownerID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x11}, 256))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	client := &http.Client{Timeout: 2 * time.Second}
	status, resp := getAuth(t, client, "http://"+srv.addr+"/v1/channels", ownerTok)
	if status != http.StatusUnauthorized || resp != `{"error":"unauthorized"}` {
		t.Fatalf("old token %d %s", status, resp)
	}
	status, resp = getAuth(t, client, "http://"+srv.addr+"/v1/channels", otherTok)
	if status != http.StatusOK || strings.Contains(resp, "alpha") || strings.Contains(resp, ownerID) || !strings.Contains(resp, "beta") || !strings.Contains(resp, memberID) || !strings.Contains(resp, `"members":[{"seat":"`+memberID+`","access":"write"}]`) {
		t.Fatalf("startup list %d %s", status, resp)
	}
	if strings.Contains(srv.out.String(), "title-alpha") || strings.Contains(srv.errb.String(), "title-alpha") {
		t.Fatalf("stdout %q stderr %q", srv.out.String(), srv.errb.String())
	}
	srv.wait(t)
}

func TestServeStopsWhenTheChannelStoreCannotOpen(t *testing.T) {
	restoreSeams(t)
	saveTeamSeams(t)
	openChannelStore = func(string, channel.Options) (*channel.Store, error) {
		return nil, errors.New("open channels failed")
	}
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x21}, 16))
	stdout, stderr, err := waitServe(t, t.TempDir())
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "open channels failed") || stdout != "" || strings.Contains(stderr, "listening") {
		t.Fatalf("open %v stdout %q stderr %q", err, stdout, stderr)
	}

	openChannelStore = channel.Open
	reconcileStore = func(*channel.Store, func(string) bool) error {
		return errors.New("reconcile failed")
	}
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x22}, 16))
	stdout, stderr, err = waitServe(t, t.TempDir())
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "reconcile failed") || strings.Contains(stdout, "listening") {
		t.Fatalf("reconcile %v stdout %q stderr %q", err, stdout, stderr)
	}
}

func TestServePrintsNoChannelContent(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x31}, 4096))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + srv.addr
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x44}, 64))
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
	status, resp = reqJSON(t, client, http.MethodPut, base+"/v1/channels/"+inst(0x11)+"/main", created.Token, `{"title":"title-alpha"}`)
	if status != http.StatusCreated {
		t.Fatalf("put %d %s", status, resp)
	}
	status, resp = reqJSON(t, client, http.MethodPost, base+"/v1/channels/"+inst(0x11)+"/main/mirror", created.Token, `{"entries":[{"key":"key-alpha","body":{"marked":"body-alpha"}}]}`)
	if status != http.StatusOK {
		t.Fatalf("post %d %s", status, resp)
	}
	leaked := srv.out.String() + srv.errb.String()
	for _, secret := range []string{"title-alpha", "key-alpha", "body-alpha", created.Token, join} {
		if strings.Contains(leaked, secret) {
			t.Fatalf("printed %s in %q", secret, leaked)
		}
	}
	srv.wait(t)
}

func TestMutationStopsWhenTheSeatIsRevokedDuringTheBody(t *testing.T) {
	// Each route reads the body between its two locked sections. A revoke in
	// that window must not apply the body.
	for _, tc := range []struct {
		method, suffix, body, marker string
	}{
		{http.MethodPut, "", `{"title":"changed-title"}`, "changed-title"},
		{http.MethodPut, "/members", `{"members":[]}`, ""},
		{http.MethodPost, "/mirror", `{"entries":[{"key":"late-key","body":1}]}`, "late-key"},
	} {
		p := newParty(t, channel.Options{})
		id := inst(0x11)
		token := p.seat(t, 0x11, "seat a")
		p.create(t, token, id, "main", "title-main")
		second := openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 70}})
		path := "/v1/channels/" + id + "/main" + tc.suffix
		file := filepath.Join(p.dir, "channels", id, "main", "channel.json")
		if tc.method == http.MethodPost {
			file = filepath.Join(p.dir, "channels", id, "main", "mirror.jsonl")
		}
		status, _, body := callReader(p.h, tc.method, path, token, &readHook{
			r: strings.NewReader(tc.body),
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
			t.Fatalf("%s %s %d %s", tc.method, tc.suffix, status, body)
		}
		if tc.marker != "" {
			stored, err := os.ReadFile(file)
			if err == nil && bytes.Contains(stored, []byte(tc.marker)) {
				t.Fatalf("%s stored %s", tc.suffix, stored)
			}
		}
	}
}

func TestSecondSeatUsesItsChannelWhileAnotherIsDamaged(t *testing.T) {
	ownerID, otherID := inst(0x11), inst(0x22)
	damages := []struct {
		name string
		file string
		body string
	}{
		{"meta", "channel.json", `{"title":"PLANTED-SECRET"}`},
		{"mirror", "mirror.jsonl", "{\"secret-marker\":true}\n"},
		{"comments", "comments.jsonl", "{\"secret-marker\":true}\n"},
	}
	for _, damage := range damages {
		t.Run(damage.name, func(t *testing.T) {
			p := newParty(t, channel.Options{})
			owner := p.seat(t, 0x11, "seat a")
			other := p.seat(t, 0x22, "seat b")
			p.create(t, owner, ownerID, "alpha", "title-alpha")
			p.create(t, other, otherID, "beta", "title-beta")
			callOK(t, p.h, http.MethodPost, "/v1/channels/"+otherID+"/beta/mirror", other, `{"entries":[{"key":"keep","body":{"n":1}}]}`)
			callOK(t, p.h, http.MethodPost, "/v1/channels/"+otherID+"/beta/comments", other, `{"entries":[{"key":"note","body":true}]}`)
			path := filepath.Join(p.dir, "channels", ownerID, "alpha", damage.file)
			if err := os.WriteFile(path, []byte(damage.body), 0o600); err != nil {
				t.Fatal(err)
			}
			// The open store cached the healthy record. A restart is what
			// makes the damaged file visible.
			if err := p.st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := channel.Open(p.dir, channel.Options{Now: fixedNow})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			p.h.channels = st
			status, _, body := call(p.h, http.MethodGet, "/v1/channels", other, "")
			if status != http.StatusOK || !bytes.Contains(body, []byte(`"chat":"beta"`)) || bytes.Contains(body, []byte(`"chat":"alpha"`)) || bytes.Contains(body, []byte("PLANTED-SECRET")) || bytes.Contains(body, []byte("secret-marker")) || bytes.Contains(body, []byte("title-alpha")) {
				t.Fatalf("list %d %s", status, body)
			}
			got := callOK(t, p.h, http.MethodGet, "/v1/channels/"+otherID+"/beta/mirror", other, "")
			if !strings.Contains(got, `"key":"keep"`) || !strings.Contains(got, `"last":1`) {
				t.Fatalf("mirror %s", got)
			}
			got = callOK(t, p.h, http.MethodGet, "/v1/channels/"+otherID+"/beta/comments", other, "")
			if !strings.Contains(got, `"key":"note"`) {
				t.Fatalf("comments %s", got)
			}
			status, _, body = call(p.h, http.MethodPost, "/v1/channels/"+otherID+"/beta/mirror", other, `{"entries":[{"key":"more","body":2}]}`)
			if status != http.StatusOK || !bytes.Contains(body, []byte(`"seq":2`)) {
				t.Fatalf("append %d %s", status, body)
			}
			status, _, body = call(p.h, http.MethodPut, "/v1/channels/"+otherID+"/beta", other, `{"title":"title-beta-2"}`)
			if status != http.StatusOK || !bytes.Contains(body, []byte(`"title":"title-beta-2"`)) {
				t.Fatalf("rename %d %s", status, body)
			}
			alpha := "/v1/channels/" + ownerID + "/alpha"
			for _, route := range []struct {
				method, path, payload string
			}{
				{http.MethodPut, alpha, `{"title":"nope"}`},
				{http.MethodDelete, alpha, ""},
				{http.MethodPut, alpha + "/members", `{"members":[]}`},
				{http.MethodGet, alpha + "/mirror", ""},
				{http.MethodPost, alpha + "/mirror", `{"entries":[{"key":"k","body":1}]}`},
				{http.MethodGet, alpha + "/comments", ""},
				{http.MethodPost, alpha + "/comments", `{"entries":[{"key":"k","body":1}]}`},
			} {
				status, _, body = call(p.h, route.method, route.path, owner, route.payload)
				if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("PLANTED-SECRET")) || bytes.Contains(body, []byte("secret-marker")) || bytes.Contains(body, []byte("title-alpha")) {
					t.Fatalf("%s %s %d %s", route.method, route.path, status, body)
				}
			}
		})
	}
}

func TestServeStartsWithADamagedChannel(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	reg := openRegistry(t, dir, registry.Options{Name: "relay a", Now: fixedNow, Entropy: &seqReader{}})
	st, err := channel.Open(dir, channel.Options{Now: fixedNow, Entropy: &seqReader{}})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, otherID := inst(0x11), inst(0x22)
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	ownerTok, _, err := reg.Register(code, ownerID, "seat a")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err = reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _, err := reg.Register(code, otherID, "seat b")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(ownerID, "alpha", "title-alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(otherID, "beta", "title-beta"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Append(otherID, "beta", channel.Mirror, otherID, []channel.NewEntry{{Key: "keep", Body: json.RawMessage(`{"n":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "channels", ownerID, "alpha", "channel.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	entropy = bytes.NewReader(bytes.Repeat([]byte{0x11}, 64))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + srv.addr
	status, resp := getAuth(t, client, base+"/v1/hello", "")
	if status != http.StatusOK || !strings.Contains(resp, `"protocol":1`) {
		t.Fatalf("hello %d %s", status, resp)
	}
	status, resp = getAuth(t, client, base+"/v1/channels", otherTok)
	if status != http.StatusOK || !strings.Contains(resp, `"chat":"beta"`) || strings.Contains(resp, `"chat":"alpha"`) || strings.Contains(resp, "title-alpha") {
		t.Fatalf("list %d %s", status, resp)
	}
	status, resp = getAuth(t, client, base+"/v1/channels/"+otherID+"/beta/mirror", otherTok)
	if status != http.StatusOK || !strings.Contains(resp, `"key":"keep"`) {
		t.Fatalf("beta %d %s", status, resp)
	}
	status, resp = getAuth(t, client, base+"/v1/channels/"+ownerID+"/alpha/mirror", ownerTok)
	if status != http.StatusInternalServerError || resp != `{"error":"internal"}` {
		t.Fatalf("alpha %d %s", status, resp)
	}
	if strings.Contains(srv.out.String(), "title-alpha") || strings.Contains(srv.errb.String(), "title-alpha") || strings.Contains(srv.out.String(), ownerTok) {
		t.Fatalf("stdout %q stderr %q", srv.out.String(), srv.errb.String())
	}
	srv.wait(t)
}

func TestClosedStoreListIsInternal(t *testing.T) {
	p := newParty(t, channel.Options{})
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, inst(0x11), "alpha", "title-secret")
	if err := p.st.Close(); err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodGet, "/v1/channels", owner, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("title-secret")) {
		t.Fatalf("closed %d %s", status, body)
	}
}

func TestHandlerWithoutAStoreIsInternal(t *testing.T) {
	reg, h := testHandler(t, &seqReader{}, "relay a")
	code, _, err := reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := reg.Register(code, inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	status, _, body := do(h, http.MethodGet, "/v1/channels", token, nil)
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("seat a")) {
		t.Fatalf("nil store %d %s", status, body)
	}
	status, _, body = do(h, http.MethodGet, "/v1/channels", "", nil)
	if status != http.StatusUnauthorized || string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("nil store anonymous %d %s", status, body)
	}
}

func TestConcurrentAppendsOverHTTP(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "seat a")
	p.create(t, owner, ownerID, "main", "title-main")
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errc := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"entries":[{"key":"k%d","body":{"n":%d}}]}`, i, i)
			status, _, got := call(p.h, http.MethodPost, "/v1/channels/"+ownerID+"/main/mirror", owner, body)
			if status != http.StatusOK {
				errc <- fmt.Sprintf("%d %s", status, got)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errc)
	for msg := range errc {
		t.Error(msg)
	}
	var page readResp
	raw := callOK(t, p.h, http.MethodGet, "/v1/channels/"+ownerID+"/main/mirror", owner, "")
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if page.More || page.Last != n || len(page.Entries) != n {
		t.Fatalf("log %+v", page)
	}
	seen := map[uint64]bool{}
	for _, e := range page.Entries {
		if seen[e.Seq] || e.Seq < 1 || e.Seq > n {
			t.Fatalf("seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func TestRevokeBetweenRefreshAndTheCallIsCleanedUp(t *testing.T) {
	// Each subtest revokes seat a from a second registry after this
	// request's Refresh and before the registry call. The call itself
	// then fails, except the successful rename.
	t.Run("refused registration", func(t *testing.T) {
		p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
		saveTeamSeams(t)
		beforeRegister = func() { revokeFrom(t, second, ownerID) }
		status, _, body := call(p.h, http.MethodPost, "/v1/register", "", `{"join_code":"not-a-code","installation_id":"`+inst(0x99)+`","name":"seat z"}`)
		if status != http.StatusForbidden || string(body) != `{"error":"join_refused"}` {
			t.Fatalf("refused %d %s", status, body)
		}
		assertRevokedSeatIsGone(t, p, ownerID, otherTok, thirdTok)
	})

	t.Run("successful rename", func(t *testing.T) {
		p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
		saveTeamSeams(t)
		beforeRename = func() { revokeFrom(t, second, ownerID) }
		status, _, body := call(p.h, http.MethodPut, "/v1/seat", otherTok, `{"name":"seat b2"}`)
		if status != http.StatusOK || !strings.Contains(string(body), `"name":"seat b2"`) {
			t.Fatalf("rename %d %s", status, body)
		}
		assertRevokedSeatIsGone(t, p, ownerID, otherTok, thirdTok)
	})

	t.Run("refused rename", func(t *testing.T) {
		p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
		saveTeamSeams(t)
		beforeRename = func() { revokeFrom(t, second, ownerID) }
		status, _, body := call(p.h, http.MethodPut, "/v1/seat", otherTok, `{"name":""}`)
		if status != http.StatusBadRequest || string(body) != `{"error":"bad_request"}` {
			t.Fatalf("bad name %d %s", status, body)
		}
		listed := callOK(t, p.h, http.MethodGet, "/v1/seats", thirdTok, "")
		if !strings.Contains(listed, `"name":"seat b"`) {
			t.Fatalf("name changed %s", listed)
		}
		assertRevokedSeatIsGone(t, p, ownerID, otherTok, thirdTok)
	})

	t.Run("leave of another seat", func(t *testing.T) {
		p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
		otherID := inst(0x22)
		saveTeamSeams(t)
		// The operator revokes the caller as well, so RevokeSeat fails
		// after it has replayed the other seat's revoke.
		beforeRevokeSeat = func() {
			revokeFrom(t, second, ownerID)
			revokeFrom(t, second, otherID)
		}
		status, _, body := call(p.h, http.MethodDelete, "/v1/seat", otherTok, "")
		if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` {
			t.Fatalf("leave %d %s", status, body)
		}
		assertRevokedSeatIsGone(t, p, ownerID, thirdTok, thirdTok)
		if _, err := os.Stat(filepath.Join(p.dir, "channels", otherID, "beta")); !os.IsNotExist(err) {
			t.Fatalf("beta %v", err)
		}
	})

	t.Run("refresh meets a damaged line", func(t *testing.T) {
		p, ownerID, _, otherTok, _, second := revokedSeatFixture(t)
		revokeFrom(t, second, ownerID)
		f, err := os.OpenFile(p.reg.Path(), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("{\"type\":\"zzz\"}\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		status, _, body := call(p.h, http.MethodGet, "/v1/channels", otherTok, "")
		if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("zzz")) {
			t.Fatalf("refresh %d %s", status, body)
		}
		if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
			t.Fatalf("alpha %v", err)
		}
	})
}

func TestRenameCleanupFailureStaysDue(t *testing.T) {
	p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
	saveTeamSeams(t)
	left := 1
	reconcileStore = func(st *channel.Store, active func(string) bool) error {
		if left > 0 {
			left--
			return errors.New("reconcile blocked")
		}
		return st.Reconcile(active)
	}
	beforeRename = func() { revokeFrom(t, second, ownerID) }
	status, _, body := call(p.h, http.MethodPut, "/v1/seat", otherTok, `{"name":"seat b2"}`)
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("reconcile blocked")) {
		t.Fatalf("cleanup %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", thirdTok, ""); strings.Contains(got, ownerID) || strings.Contains(got, `"chat":"alpha"`) {
		t.Fatalf("still there %s", got)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha remains %v", err)
	}
}

func TestLeaveCleanupFailureStaysDue(t *testing.T) {
	p, ownerID, _, otherTok, thirdTok, second := revokedSeatFixture(t)
	otherID := inst(0x22)
	saveTeamSeams(t)
	left := 1
	reconcileStore = func(st *channel.Store, active func(string) bool) error {
		if left > 0 {
			left--
			return errors.New("reconcile blocked")
		}
		return st.Reconcile(active)
	}
	beforeRevokeSeat = func() {
		revokeFrom(t, second, ownerID)
		revokeFrom(t, second, otherID)
	}
	status, _, body := call(p.h, http.MethodDelete, "/v1/seat", otherTok, "")
	if status != http.StatusInternalServerError || string(body) != `{"error":"internal"}` || bytes.Contains(body, []byte("reconcile blocked")) {
		t.Fatalf("cleanup %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", thirdTok, ""); strings.Contains(got, ownerID) || strings.Contains(got, `"chat":"alpha"`) {
		t.Fatalf("still there %s", got)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha remains %v", err)
	}
}

// revokedSeatFixture registers three seats. Seat a owns alpha and is a
// member of beta and gamma. otherTok is seat b. thirdTok is seat c.
func revokedSeatFixture(t *testing.T) (p *party, ownerID, ownerTok, otherTok, thirdTok string, second *registry.Registry) {
	t.Helper()
	p = newParty(t, channel.Options{})
	ownerID = inst(0x11)
	otherID, thirdID := inst(0x22), inst(0x33)
	ownerTok = p.seat(t, 0x11, "seat a")
	otherTok = p.seat(t, 0x22, "seat b")
	thirdTok = p.seat(t, 0x33, "seat c")
	p.create(t, ownerTok, ownerID, "alpha", "title-alpha")
	p.create(t, otherTok, otherID, "beta", "title-beta")
	p.members(t, otherTok, otherID, "beta", ownerID, "")
	p.create(t, thirdTok, thirdID, "gamma", "title-gamma")
	p.members(t, thirdTok, thirdID, "gamma", "", ownerID)
	second = openRegistry(t, p.dir, registry.Options{Now: fixedNow, Entropy: &seqReader{n: 180}})
	return p, ownerID, ownerTok, otherTok, thirdTok, second
}

func revokeFrom(t *testing.T, reg *registry.Registry, id string) {
	t.Helper()
	if _, err := reg.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Revoke(id); err != nil {
		t.Fatal(err)
	}
}

func assertRevokedSeatIsGone(t *testing.T, p *party, ownerID, listTok, memberOwnerTok string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(p.dir, "channels", ownerID, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha %v", err)
	}
	listed := callOK(t, p.h, http.MethodGet, "/v1/channels", memberOwnerTok, "")
	if strings.Contains(listed, ownerID) {
		t.Fatalf("still a member %s", listed)
	}
	code, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	status, _, body := call(p.h, http.MethodPost, "/v1/register", "", jsonBody(code, ownerID, "seat a2"))
	if status != http.StatusCreated {
		t.Fatalf("register %d %s", status, body)
	}
	var created registerResp
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if got := callOK(t, p.h, http.MethodGet, "/v1/channels", created.Token, ""); got != `{"channels":[]}` {
		t.Fatalf("inherited %s", got)
	}
	again := callOK(t, p.h, http.MethodGet, "/v1/channels", listTok, "")
	if strings.Contains(again, ownerID) || strings.Contains(again, `"chat":"alpha"`) {
		t.Fatalf("put back %s", again)
	}
}

type party struct {
	reg *registry.Registry
	h   *server
	st  *channel.Store
	dir string
}

func newParty(t *testing.T, opts channel.Options) *party {
	t.Helper()
	dir := t.TempDir()
	reg := openRegistry(t, dir, registry.Options{Name: "relay a", Now: fixedNow, Entropy: &seqReader{}})
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	if opts.Entropy == nil {
		opts.Entropy = &seqReader{}
	}
	st, err := channel.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := newHandler(reg)
	h.channels = st
	return &party{reg: reg, h: h, st: st, dir: dir}
}

func (p *party) seat(t *testing.T, id byte, name string) string {
	t.Helper()
	code, _, err := p.reg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := p.reg.Register(code, inst(id), name)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (p *party) create(t *testing.T, token, owner, chat, title string) {
	t.Helper()
	status, _, body := call(p.h, http.MethodPut, "/v1/channels/"+owner+"/"+chat, token, `{"title":"`+title+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("create %s %d %s", chat, status, body)
	}
}

func (p *party) members(t *testing.T, token, owner, chat, writeID, readID string) {
	t.Helper()
	var members []string
	if writeID != "" {
		members = append(members, `{"seat":"`+writeID+`","access":"write"}`)
	}
	if readID != "" {
		members = append(members, `{"seat":"`+readID+`","access":"read"}`)
	}
	status, _, body := call(p.h, http.MethodPut, "/v1/channels/"+owner+"/"+chat+"/members", token, `{"members":[`+strings.Join(members, ",")+`]}`)
	if status != http.StatusOK {
		t.Fatalf("members %s %d %s", chat, status, body)
	}
}

func call(h http.Handler, method, path, token, body string) (int, http.Header, []byte) {
	var r io.Reader
	if body != "" || method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete {
		r = strings.NewReader(body)
	}
	return do(h, method, path, token, r)
}

func callReader(h http.Handler, method, path, token string, body io.Reader) (int, http.Header, []byte) {
	return do(h, method, path, token, body)
}

func callOK(t *testing.T, h http.Handler, method, path, token, body string) string {
	t.Helper()
	status, hdr, got := call(h, method, path, token, body)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("%s %s %d %s", method, path, status, got)
	}
	assertJSONHeaders(t, hdr)
	return string(got)
}

func channelJSON(owner, chat, title, gen, access string, mirror, comments uint64, members string) string {
	s := fmt.Sprintf(`{"owner":"%s","chat":"%s","title":"%s","generation":"%s","created_at":"%s","access":"%s","mirror_last":%d,"comments_last":%d`,
		owner, chat, title, gen, stamp, access, mirror, comments)
	if members != "" {
		s += `,"members":` + members
	}
	return s + "}"
}

func jsonText(n int) string {
	b := make([]byte, n)
	b[0] = '"'
	b[n-1] = '"'
	for i := 1; i < n-1; i++ {
		b[i] = 'a'
	}
	return string(b)
}

func oneLine(t *testing.T, owner string) int64 {
	t.Helper()
	dir := t.TempDir()
	st, err := channel.Open(dir, channel.Options{Now: fixedNow, Entropy: bytes.NewReader(bytes.Repeat([]byte{0xab}, 8))})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.Put(owner, "p", "t"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Append(owner, "p", channel.Mirror, owner, []channel.NewEntry{{Key: "a", Body: json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "channels", owner, "p", "mirror.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func saveTeamSeams(t *testing.T) {
	t.Helper()
	open, recon, before := openChannelStore, reconcileStore, beforeRegister
	rename, leave := beforeRename, beforeRevokeSeat
	t.Cleanup(func() {
		openChannelStore, reconcileStore, beforeRegister = open, recon, before
		beforeRename, beforeRevokeSeat = rename, leave
	})
}

func waitServe(t *testing.T, dir string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-state-dir", dir}, "dev", &stdout, &stderr)
	}()
	select {
	case err := <-errc:
		return stdout.String(), stderr.String(), err
	case <-time.After(2 * time.Second):
		cancel()
		<-errc
		t.Fatalf("serve stayed up stdout %q stderr %q", stdout.String(), stderr.String())
	}
	return "", "", nil
}

type readHook struct {
	r    io.Reader
	hook func()
	once sync.Once
}

func (h *readHook) Read(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.r.Read(p)
}

func getAuth(t *testing.T, client *http.Client, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func reqJSON(t *testing.T, client *http.Client, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
