package team

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

type replayRecord struct {
	N      int             `json:"n"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Auth   string          `json:"auth"`
	Req    json.RawMessage `json:"req"`
	Status int             `json:"status"`
	Res    json.RawMessage `json:"res"`
	Error  string          `json:"error"`
}

type replayOperator struct {
	Before int    `json:"before_n"`
	Cmd    string `json:"cmd"`
	Count  int    `json:"count"`
	Seat   string `json:"seat"`
}

func replayOperators(manifest []byte) ([]replayOperator, error) {
	var data struct {
		Operator []replayOperator `json:"operator"`
	}
	if err := json.Unmarshal(manifest, &data); err != nil {
		return nil, fmt.Errorf("invalid manifest")
	}
	if len(data.Operator) == 0 {
		return nil, fmt.Errorf("manifest operator list missing")
	}
	for _, op := range data.Operator {
		switch op.Cmd {
		case "issue":
			if op.Count <= 0 {
				return nil, fmt.Errorf("invalid issue count")
			}
		case "revoke":
			if op.Seat == "" {
				return nil, fmt.Errorf("missing revoke seat")
			}
		default:
			return nil, fmt.Errorf("unknown operator command")
		}
		if op.Before <= 0 {
			return nil, fmt.Errorf("invalid operator position")
		}
	}
	sort.SliceStable(data.Operator, func(i, j int) bool { return data.Operator[i].Before < data.Operator[j].Before })
	return data.Operator, nil
}

func TestReplayOperators(t *testing.T) {
	for _, tc := range []struct{ data, problem string }{
		{`{}`, "manifest operator list missing"},
		{`{"operator":[{"before_n":1,"cmd":"other"}]}`, "unknown operator command"},
		{`{"operator":[{"before_n":1,"cmd":"issue","count":0}]}`, "invalid issue count"},
		{`{"operator":[{"before_n":1,"cmd":"revoke"}]}`, "missing revoke seat"},
		{`{"operator":[{"before_n":0,"cmd":"issue","count":1}]}`, "invalid operator position"},
		{`{`, "invalid manifest"},
	} {
		if _, err := replayOperators([]byte(tc.data)); err == nil || err.Error() != tc.problem {
			t.Fatalf("manifest validation: want %s", tc.problem)
		}
	}
	ops, err := replayOperators([]byte(`{"operator":[{"before_n":9,"cmd":"revoke","seat":"token-1"},{"before_n":3,"cmd":"issue","count":1}]}`))
	if err != nil || len(ops) != 2 || ops[0].Cmd != "issue" || ops[1].Cmd != "revoke" {
		t.Fatal("manifest operator order")
	}
}

// replayExceptions names any comparisons that depart from the plan. Times are
// handled by the plan's format-and-order rule, not an exception.
var replayExceptions = []string{
	"Roster and member order: anonymised IDs do not preserve the live ID sort order.",
}

type replayMap struct{ forward, reverse map[string]string }

func (m *replayMap) add(a, b string) bool {
	if old, ok := m.forward[a]; ok && old != b {
		return false
	}
	if old, ok := m.reverse[b]; ok && old != a {
		return false
	}
	m.forward[a], m.reverse[b] = b, a
	return true
}
func (m *replayMap) rewrite(v any) any {
	switch x := v.(type) {
	case string:
		if mapped, ok := m.forward[x]; ok {
			return mapped
		}
		return x
	case []any:
		for i := range x {
			x[i] = m.rewrite(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = m.rewrite(x[k])
		}
	}
	return v
}
func replayCompare(want, got any, path string, m *replayMap) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(w) != len(g) {
			return path
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			part := k
			if strings.Contains(path, ".body") {
				part = "<field>"
			}
			v, ok := g[k]
			if !ok {
				return path + "." + part
			}
			if diff := replayCompare(w[k], v, path+"."+part, m); diff != "" {
				return diff
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return path
		}
		if path == "$.seats" || strings.HasSuffix(path, ".members") {
			w = append([]any(nil), w...)
			g = append([]any(nil), g...)
			key := func(v any) string {
				row, _ := v.(map[string]any)
				id, _ := row["id"].(string)
				if path != "$.seats" {
					id, _ = row["seat"].(string)
				}
				if live, ok := m.forward[id]; ok {
					return live
				}
				return id
			}
			sort.Slice(w, func(i, j int) bool { return key(w[i]) < key(w[j]) })
			sort.Slice(g, func(i, j int) bool { return key(g[i]) < key(g[j]) })
		}
		var previous time.Time
		for i := range w {
			if row, ok := g[i].(map[string]any); ok {
				if raw, ok := row["at"].(string); ok {
					at, err := time.Parse(time.RFC3339Nano, raw)
					if err != nil || (!previous.IsZero() && at.Before(previous)) {
						return fmt.Sprintf("%s[%d].at", path, i)
					}
					previous = at
				}
			}
			if diff := replayCompare(w[i], g[i], fmt.Sprintf("%s[%d]", path, i), m); diff != "" {
				return diff
			}
		}
		return ""
	case string:
		g, ok := got.(string)
		if !ok {
			return path
		}
		if strings.HasSuffix(path, ".at") || strings.HasSuffix(path, ".created_at") {
			if _, e := time.Parse(time.RFC3339Nano, w); e != nil {
				return path
			}
			if _, e := time.Parse(time.RFC3339Nano, g); e != nil {
				return path
			}
			return ""
		}
		if mapped, ok := m.forward[w]; ok {
			if mapped != g {
				return path
			}
			return ""
		}
		if strings.HasSuffix(path, ".generation") || strings.HasSuffix(path, ".server.id") || path == "$.token" {
			if !m.add(w, g) {
				return path
			}
			return ""
		}
		if w != g {
			return path
		}
		return ""
	default:
		if want != got {
			return path
		}
		return ""
	}
}

func replayLogTimes(body any, path string, times map[string]map[float64]time.Time) bool {
	row, ok := body.(map[string]any)
	if !ok {
		return true
	}
	entries, ok := row["entries"].([]any)
	if !ok {
		return true
	}
	if times[path] == nil {
		times[path] = make(map[float64]time.Time)
	}
	for _, entry := range entries {
		record, ok := entry.(map[string]any)
		if !ok {
			return false
		}
		seq, ok := record["seq"].(float64)
		if !ok {
			return false
		}
		stamp, ok := record["at"].(string)
		if !ok {
			return false
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return false
		}
		times[path][seq] = at
	}
	seqs := make([]float64, 0, len(times[path]))
	for seq := range times[path] {
		seqs = append(seqs, seq)
	}
	sort.Float64s(seqs)
	for i := 1; i < len(seqs); i++ {
		if times[path][seqs[i]].Before(times[path][seqs[i-1]]) {
			return false
		}
	}
	return true
}

func TestRealTeamE2EReplay(t *testing.T) {
	dir := filepath.Join("testdata")
	wire, err := os.Open(filepath.Join(dir, "real-team-e2e-wire.jsonl"))
	if os.IsNotExist(err) {
		t.Skip("local team acceptance recording absent")
	}
	if err != nil {
		t.Fatal("cannot open local recording")
	}
	defer wire.Close()
	manifest, err := os.ReadFile(filepath.Join(dir, "real-team-e2e-manifest.json"))
	if os.IsNotExist(err) {
		t.Skip("local team acceptance manifest absent")
	}
	if err != nil {
		t.Fatal("cannot read local manifest")
	}
	operators, err := replayOperators(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var records []replayRecord
	scanner := bufio.NewScanner(wire)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var r replayRecord
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			t.Fatal("invalid recording record")
		}
		records = append(records, r)
	}
	if scanner.Err() != nil {
		t.Fatal("cannot read recording")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].N < records[j].N })
	state := t.TempDir()
	reg := openRegistry(t, state, registry.Options{Name: "Lab One", Now: fixedNow, Entropy: &seqReader{}})
	store, err := channel.Open(state, channel.Options{Now: fixedNow, Entropy: &seqReader{}})
	if err != nil {
		t.Fatal("cannot open channel store")
	}
	defer store.Close()
	h := newHandler(reg)
	h.channels = store
	mapping := &replayMap{forward: map[string]string{}, reverse: map[string]string{}}
	ids := map[string]string{}
	recordedTimes := make(map[string]map[float64]time.Time)
	liveTimes := make(map[string]map[float64]time.Time)
	operatorIndex, codeCount, registrations, probed := 0, 0, 0, false
	for _, rec := range records {
		for operatorIndex < len(operators) && operators[operatorIndex].Before <= rec.N {
			op := operators[operatorIndex]
			switch op.Cmd {
			case "issue":
				for i := 0; i < op.Count; i++ {
					codeCount++
					code, _, err := reg.Issue()
					if err != nil || !mapping.add(fmt.Sprintf("joincode-%d", codeCount), code) {
						t.Fatal("issue failed")
					}
				}
			case "revoke":
				id, ok := ids[op.Seat]
				if !ok {
					t.Fatal("revoke seat has not registered")
				}
				if _, err := reg.RevokeSeat(id); err != nil {
					t.Fatal("revoke failed")
				}
			}
			operatorIndex++
		}
		if rec.Status == 0 && rec.Error != "" {
			continue
		}
		var request any
		if len(rec.Req) != 0 && string(rec.Req) != "null" {
			if json.Unmarshal(rec.Req, &request) != nil {
				t.Fatalf("record %d invalid request JSON", rec.N)
			}
			request = mapping.rewrite(request)
		}
		var raw string
		if request != nil {
			b, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("record %d request encoding", rec.N)
			}
			raw = string(b)
		}
		path := rec.Path
		for recorded, live := range mapping.forward {
			path = strings.ReplaceAll(path, recorded, live)
		}
		token := rec.Auth
		if v, ok := mapping.forward[token]; ok {
			token = v
		}
		status, _, response := call(h, rec.Method, path, token, raw)
		safePath := path
		if strings.HasPrefix(path, "/v1/channels/") {
			parts := strings.Split(path, "/")
			if len(parts) > 4 {
				parts[3], parts[4] = "{owner}", "{chat}"
				safePath = strings.Join(parts, "/")
			}
		}
		fail := func(where string) { t.Fatalf("record %d %s %s differs at %s", rec.N, rec.Method, safePath, where) }
		if status != rec.Status {
			fail("status")
		}
		var expected, actual any
		if len(rec.Res) > 0 && string(rec.Res) != "null" {
			if json.Unmarshal(rec.Res, &expected) != nil {
				fail("recorded body")
			}
		}
		if len(response) > 0 {
			if json.Unmarshal(response, &actual) != nil {
				fail("response body")
			}
		}
		if diff := replayCompare(expected, actual, "$", mapping); diff != "" {
			fail(diff)
		}
		logPath := strings.SplitN(rec.Path, "?", 2)[0]
		if rec.Method == http.MethodGet && (strings.HasSuffix(logPath, "/mirror") || strings.HasSuffix(logPath, "/comments")) && rec.Status == http.StatusOK {
			if !replayLogTimes(expected, logPath, recordedTimes) || !replayLogTimes(actual, logPath, liveTimes) {
				fail("$.entries.at")
			}
		}
		if rec.Method == http.MethodPost && rec.Path == "/v1/register" && rec.Status == http.StatusCreated {
			registrations++
			key := fmt.Sprintf("token-%d", registrations)
			token, ok := actual.(map[string]any)["token"].(string)
			if !ok || !mapping.add(key, token) {
				fail("$.token")
			}
			req, ok := request.(map[string]any)
			if !ok {
				fail("request")
			}
			ids[key], ok = req["installation_id"].(string)
			if !ok {
				fail("request.installation_id")
			}
		}
		// A synthetic negative append is needed to prove the replay notices a
		// removed generation guard; the acceptance recording has only valid appends.
		if !probed && rec.Method == http.MethodPut && rec.Status == http.StatusCreated && strings.HasPrefix(rec.Path, "/v1/channels/") {
			answer := actual.(map[string]any)
			gen := answer["generation"].(string)
			stale := "0000000000000000"
			if stale == gen {
				stale = "1111111111111111"
			}
			body := fmt.Sprintf(`{"generation":%q,"entries":[{"key":"probe","body":{}}]}`, stale)
			got, _, data := call(h, http.MethodPost, path+"/mirror", token, body)
			if got != http.StatusConflict || !bytes.Equal(data, []byte(`{"error":"stale_generation"}`)) {
				fail("stale generation probe")
			}
			probed = true
		}
	}
	if operatorIndex != len(operators) || !probed {
		t.Fatal("recording did not exercise required operations")
	}
}
