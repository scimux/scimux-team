package team

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/scimux/scimux-team/internal/channel"
)

func TestScenarioTeamWalk(t *testing.T) {
	p := newParty(t, channel.Options{})
	ids := []string{inst(0x51), inst(0x52), inst(0x53)}
	names := []string{"Oak", "Pine", "Elm"}
	codes := make([]string, 3)
	tokens := make([]string, 3)
	for i := range codes {
		code, _, err := p.reg.Issue()
		if err != nil {
			t.Fatal("issue failed")
		}
		codes[i] = code
	}
	request := func(method, path, token, body string, want int) map[string]any {
		t.Helper()
		status, _, raw := call(p.h, method, path, token, body)
		if status != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, status, want)
		}
		if len(raw) == 0 {
			return nil
		}
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			t.Fatalf("%s %s: invalid JSON", method, path)
		}
		return out
	}
	for i := range ids {
		body := request(http.MethodPost, "/v1/register", "", jsonBody(codes[i], ids[i], names[i]), http.StatusCreated)
		token, ok := body["token"].(string)
		if !ok || len(token) != 64 {
			t.Fatal("registration token missing")
		}
		tokens[i] = token
	}
	roster := request(http.MethodGet, "/v1/seats", tokens[0], "", http.StatusOK)
	seats, ok := roster["seats"].([]any)
	if !ok || len(seats) != 3 {
		t.Fatal("roster count")
	}
	for i, raw := range seats {
		seat, ok := raw.(map[string]any)
		if !ok || seat["id"] != ids[i] || seat["name"] != names[i] {
			t.Fatal("roster entry")
		}
	}
	path := "/v1/channels/" + ids[0] + "/study"
	created := request(http.MethodPut, path, tokens[0], `{"title":"Synthetic study"}`, http.StatusCreated)
	gen, ok := created["generation"].(string)
	if !ok || len(gen) != 16 || created["owner"] != ids[0] || created["chat"] != "study" || created["access"] != "owner" {
		t.Fatal("channel creation fields")
	}
	members := fmt.Sprintf(`{"members":[{"seat":%q,"access":"write"},{"seat":%q,"access":"read"}]}`, ids[1], ids[2])
	updated := request(http.MethodPut, path+"/members", tokens[0], members, http.StatusOK)
	if list, ok := updated["members"].([]any); !ok || len(list) != 2 {
		t.Fatal("members missing")
	}
	entry := fmt.Sprintf(`{"generation":%q,"entries":[{"key":"item-1","body":{"text":"synthetic"}}]}`, gen)
	appended := request(http.MethodPost, path+"/mirror", tokens[0], entry, http.StatusOK)
	if appended["generation"] != gen || appended["last"] != float64(1) {
		t.Fatal("mirror append fields")
	}
	stale := "0000000000000000"
	if stale == gen {
		stale = "1111111111111111"
	}
	staleBody := strings.Replace(entry, gen, stale, 1)
	if out := request(http.MethodPost, path+"/mirror", tokens[0], staleBody, http.StatusConflict); out["error"] != "stale_generation" {
		t.Fatal("stale generation error")
	}
	for i := 1; i < 3; i++ {
		list := request(http.MethodGet, "/v1/channels", tokens[i], "", http.StatusOK)
		rows, ok := list["channels"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatal("member list")
		}
		row, ok := rows[0].(map[string]any)
		access := []string{"", "write", "read"}[i]
		if !ok || row["owner"] != ids[0] || row["generation"] != gen || row["access"] != access || row["mirror_last"] != float64(1) {
			t.Fatal("member channel fields")
		}
		mirror := request(http.MethodGet, path+"/mirror", tokens[i], "", http.StatusOK)
		entries, ok := mirror["entries"].([]any)
		if !ok || len(entries) != 1 || mirror["generation"] != gen || mirror["last"] != float64(1) {
			t.Fatal("member mirror")
		}
	}
	comment := fmt.Sprintf(`{"generation":%q,"entries":[{"key":"note-1","body":{"text":"synthetic"}}]}`, gen)
	out := request(http.MethodPost, path+"/comments", tokens[1], comment, http.StatusOK)
	if out["last"] != float64(1) || out["generation"] != gen {
		t.Fatal("comment append")
	}
	if out := request(http.MethodPost, path+"/comments", tokens[2], comment, http.StatusForbidden); out["error"] != "forbidden" {
		t.Fatal("read-only comment")
	}
	comments := request(http.MethodGet, path+"/comments", tokens[0], "", http.StatusOK)
	entries, ok := comments["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatal("owner comments")
	}
	record, ok := entries[0].(map[string]any)
	if !ok || record["author"] != ids[1] || record["seq"] != float64(1) {
		t.Fatal("comment author")
	}
	renamed := request(http.MethodPut, "/v1/seat", tokens[1], `{"name":"Birch"}`, http.StatusOK)
	if renamed["id"] != ids[1] || renamed["name"] != "Birch" {
		t.Fatal("rename answer")
	}
	roster = request(http.MethodGet, "/v1/seats", tokens[0], "", http.StatusOK)
	seats = roster["seats"].([]any)
	if seats[1].(map[string]any)["name"] != "Birch" {
		t.Fatal("roster rename")
	}
	request(http.MethodDelete, path+"/membership", tokens[2], "", http.StatusNoContent)
	for _, check := range []struct {
		query, code string
		status      int
	}{
		{"?generation=" + stale, "stale_generation", http.StatusConflict},
		{"", "bad_request", http.StatusBadRequest},
		{"?generation=bad", "bad_request", http.StatusBadRequest},
	} {
		t.Run(check.code+check.query, func(t *testing.T) {
			probe := path + "-probe"
			request(http.MethodPut, probe, tokens[0], `{"title":"Probe"}`, http.StatusCreated)
			status, _, raw := call(p.h, http.MethodDelete, probe+check.query, tokens[0], "")
			if status != check.status || string(raw) != `{"error":"`+check.code+`"}` {
				t.Errorf("delete: %d %s", status, raw)
			}
			// The next probe starts with a fresh channel on either server version.
			if info, err := p.st.Meta(ids[0], "study-probe"); err == nil {
				call(p.h, http.MethodDelete, probe+"?generation="+info.Generation, tokens[0], "")
			}
		})
	}

	request(http.MethodDelete, path+"?generation="+gen, tokens[0], "", http.StatusNoContent)
	if out := request(http.MethodGet, path+"/mirror", tokens[1], "", http.StatusNotFound); out["error"] != "not_found" {
		t.Fatal("deleted channel")
	}
	request(http.MethodDelete, "/v1/seat", tokens[1], "", http.StatusNoContent)
	if out := request(http.MethodGet, "/v1/seats", tokens[1], "", http.StatusUnauthorized); out["error"] != "unauthorized" {
		t.Fatal("left token accepted")
	}
	if _, err := p.reg.RevokeSeat(ids[2]); err != nil {
		t.Fatal("operator revoke")
	}
	if out := request(http.MethodGet, "/v1/seats", tokens[2], "", http.StatusUnauthorized); out["error"] != "unauthorized" {
		t.Fatal("revoked token accepted")
	}
}
