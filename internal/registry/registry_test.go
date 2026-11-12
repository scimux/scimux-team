package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/invite"
)

func TestMain(m *testing.M) {
	// Go 1.27 creates t.TempDir with mode 0777. This umask makes that 0700,
	// which is the only mode Open accepts for a directory that already exists.
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

const (
	tsEarly = "2026-11-12T09:30:00Z"
	tsMid   = "2026-11-12T09:30:01Z"
	expFar  = "2099-01-01T00:00:00Z"
)

func TestOpenWritesOneServerAndASecondOpenKeepsIt(t *testing.T) {
	dir := t.TempDir()
	idBytes := bytes.Repeat([]byte{0x01}, 16)
	start := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	r := openReg(t, dir, Options{
		Name:     "relay a",
		MaxSeats: 4,
		Now:      func() time.Time { return start },
		Entropy:  bytes.NewReader(idBytes),
	})
	if r.Path() != filepath.Join(dir, journalName) {
		t.Fatalf("path %s", r.Path())
	}
	if r.maxSeats != 4 {
		t.Fatalf("max seats %d", r.maxSeats)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", info, err)
	}
	finfo, err := os.Stat(r.Path())
	if err != nil || finfo.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", finfo, err)
	}
	got := r.Server()
	if got.ID != hex.EncodeToString(idBytes) || got.Name != "relay a" {
		t.Fatalf("server %+v", got)
	}
	body := fileBytes(t, r)
	if strings.Count(string(body), "\n") != 1 || strings.Count(string(body), `"type":"server"`) != 1 {
		t.Fatalf("journal %s", body)
	}
	var rec record
	if err := json.Unmarshal(bytes.TrimSpace(body), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID != got.ID || rec.Name != "relay a" || rec.TS != tsEarly || rec.Verifier != "" || rec.TokenSHA != "" {
		t.Fatalf("record %+v", rec)
	}

	second := openReg(t, dir, Options{
		Name:    "other name",
		Now:     func() time.Time { return start.Add(time.Hour) },
		Entropy: bytes.NewReader(bytes.Repeat([]byte{0xff}, 64)),
	})
	if second.Server() != got {
		t.Fatalf("second open %+v, want %+v", second.Server(), got)
	}
	if !bytes.Equal(fileBytes(t, second), body) {
		t.Fatal("second open rewrote the journal")
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	r := openReg(t, dir, Options{})
	got := r.Server()
	if got.Name != defaultName || !goodHex(got.ID, 16) {
		t.Fatalf("server %+v", got)
	}
	if r.maxSeats != 200 {
		t.Fatalf("max seats %d", r.maxSeats)
	}
	before := time.Now().Add(-time.Minute)
	code, row, err := r.Issue()
	after := time.Now().Add(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invite.Parse(code); err != nil || row.State != "unused" || row.Seat != "" {
		t.Fatalf("issue %s %+v %v", code, row, err)
	}
	if row.Issued.Before(before) || row.Issued.After(after) {
		t.Fatalf("issued %s", row.Issued)
	}
	inst := strings.Repeat("ab", 16)
	if _, _, err := r.Register(code, inst, "seat a"); err != nil {
		t.Fatal(err)
	}
	again := openReg(t, dir, Options{Name: "ignored"})
	if again.Server().ID != got.ID || again.Server().Name != defaultName {
		t.Fatalf("reopen %+v", again.Server())
	}
}

func TestRacingOpensAgreeOnOneServer(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	type result struct {
		r   *Registry
		err error
	}
	ch := make(chan result, n)
	for range n {
		go func() {
			r, err := Open(dir, Options{
				Name:    "relay a",
				Entropy: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 16)),
			})
			ch <- result{r, err}
		}()
	}
	var regs []*Registry
	for range n {
		res := <-ch
		if res.err != nil {
			t.Fatal(res.err)
		}
		regs = append(regs, res.r)
	}
	t.Cleanup(func() {
		for _, r := range regs {
			r.Close()
		}
	})
	want := hex.EncodeToString(bytes.Repeat([]byte{0x5a}, 16))
	for _, r := range regs {
		if r.Server().ID != want || r.Server().Name != "relay a" {
			t.Fatalf("server %+v", r.Server())
		}
	}
	body := fileBytes(t, regs[0])
	if strings.Count(string(body), "\n") != 1 || strings.Count(string(body), `"type":"server"`) != 1 {
		t.Fatalf("journal %s", body)
	}
}

func TestIssueListStatusAndRevoke(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)}
	r := openReg(t, "", Options{Now: clk.Now, Entropy: &countReader{}, Name: "relay a"})
	code, row, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	norm, err := invite.Parse(code)
	if err != nil || invite.Format(norm) != code || len(norm) != invite.CodeLength {
		t.Fatalf("code %q %v", code, err)
	}
	if row.ID != invite.CodeID(invite.Verifier(norm)) || row.State != "unused" || row.Seat != "" || row.Name != "" {
		t.Fatalf("row %+v", row)
	}
	if !row.Expires.Equal(row.Issued.Add(invite.UnusedTTL)) {
		t.Fatalf("expires %s issued %s", row.Expires, row.Issued)
	}
	list := r.List()
	if len(list) != 1 || list[0].ID != row.ID || !list[0].Issued.Equal(row.Issued) || list[0].State != "unused" {
		t.Fatalf("list %+v", list)
	}
	st := r.Status()
	if st != (Status{Codes: 1, Unused: 1}) {
		t.Fatalf("status %+v", st)
	}
	rev, err := r.Revoke(row.ID)
	if err != nil || rev.ID != row.ID || rev.State != "revoked" || rev.Seat != "" {
		t.Fatalf("revoke %+v %v", rev, err)
	}
	if r.List()[0].State != "revoked" || r.Status() != (Status{Codes: 1, Revoked: 1}) {
		t.Fatalf("after revoke %+v %+v", r.List(), r.Status())
	}
}

func TestListOrdersByIssueTimeThenID(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)}
	r := openReg(t, "", Options{Now: clk.Now, Entropy: &countReader{}})
	if _, _, err := r.Issue(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Issue(); err != nil {
		t.Fatal(err)
	}
	same := r.List()
	if len(same) != 2 || !same[0].Issued.Equal(same[1].Issued) || same[0].ID >= same[1].ID {
		t.Fatalf("same-time order %+v", same)
	}
	clk.t = clk.t.Add(time.Second)
	if _, _, err := r.Issue(); err != nil {
		t.Fatal(err)
	}
	all := r.List()
	if len(all) != 3 || !all[2].Issued.After(all[0].Issued) || !all[2].Issued.After(all[1].Issued) {
		t.Fatalf("time order %+v", all)
	}
	if all[0].ID != same[0].ID || all[1].ID != same[1].ID {
		t.Fatalf("tie broken wrong %+v", all)
	}
}

func TestCodeStateOrderAndExpiry(t *testing.T) {
	start := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	clk := &clock{t: start}
	r := openReg(t, "", Options{Now: clk.Now, Entropy: &countReader{}})

	_, revokedRow, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Revoke(revokedRow.ID); err != nil {
		t.Fatal(err)
	}

	liveCode, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	liveInst := inst(0x11)
	liveToken, _, err := r.Register(liveCode, liveInst, "seat a")
	if err != nil {
		t.Fatal(err)
	}

	freshCode, freshRow, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	clk.t = freshRow.Expires.Add(-time.Nanosecond)
	if stateOf(t, r, freshRow.ID) != "unused" {
		t.Fatalf("before expiry %s", stateOf(t, r, freshRow.ID))
	}
	clk.t = freshRow.Expires
	if stateOf(t, r, freshRow.ID) != "expired" {
		t.Fatalf("at expiry %s", stateOf(t, r, freshRow.ID))
	}
	if stateOf(t, r, revokedRow.ID) != "revoked" {
		t.Fatal("revoked lost to expiry")
	}
	if stateOf(t, r, codeID(t, liveCode)) != "redeemed" {
		t.Fatal("redeemed lost to expiry")
	}
	if seat, ok := r.Authenticate(liveToken); !ok || seat.ID != liveInst {
		t.Fatal("redeemed token died at the code expiry")
	}
	before := fileBytes(t, r)
	if _, _, err := r.Register(freshCode, inst(0x22), "seat b"); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("expired register %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) || stateOf(t, r, freshRow.ID) != "expired" {
		t.Fatal("expired register changed the journal")
	}
	st := r.Status()
	if st.Revoked != 1 || st.Redeemed != 1 || st.Expired != 1 || st.Unused != 0 || st.Codes != 3 {
		t.Fatalf("status %+v", st)
	}

	clk.t = clk.t.Add(time.Second)
	if _, later, err := r.Issue(); err != nil || later.State != "unused" {
		t.Fatalf("later issue %+v %v", later, err)
	}
	if r.Status().Unused != 1 {
		t.Fatalf("status %+v", r.Status())
	}
}

func TestOneSeatPerCode(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, row, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := r.Register(code, inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, r)
	_, _, err = r.Register(code, inst(0x22), "seat b")
	if !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("second register %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) {
		t.Fatal("second register appended")
	}
	if stateOf(t, r, row.ID) != "redeemed" || len(r.Seats()) != 1 {
		t.Fatalf("state %s seats %+v", stateOf(t, r, row.ID), r.Seats())
	}
	if _, ok := r.Authenticate(token); !ok {
		t.Fatal("first seat lost the token")
	}
}

func TestRefusedRegistrationLeavesTheCodeUnused(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, row, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, r)
	if _, _, err := r.Register(code, "ABCD", "seat a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) || stateOf(t, r, row.ID) != "unused" {
		t.Fatal("refusal consumed the code")
	}
	token, _, err := r.Register(strings.ToLower(strings.ReplaceAll(code, "-", " ")), inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	if stateOf(t, r, row.ID) != "redeemed" {
		t.Fatal(stateOf(t, r, row.ID))
	}
	if _, ok := r.Authenticate(token); !ok {
		t.Fatal("token")
	}
}

func TestRVDomainDoesNotVerify(t *testing.T) {
	entropy := bytes.Repeat([]byte{0x21}, invite.EntropyBytes)
	norm, err := invite.Generate(bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	team := hex.EncodeToString(invite.Verifier(norm))
	foreign := rvVerifierHex(norm)
	if team == foreign {
		t.Fatal("team verifier matched the rv domain")
	}

	r := openReg(t, "", Options{
		Now:     fixed(tsEarly),
		Entropy: bytes.NewReader(append(bytes.Repeat([]byte{0x01}, 16), entropy...)),
	})
	if _, _, err := r.Issue(); err != nil {
		t.Fatal(err)
	}
	body := string(fileBytes(t, r))
	if !strings.Contains(body, team) || strings.Contains(body, foreign) || strings.Contains(body, norm) || strings.Contains(body, invite.Format(norm)) {
		t.Fatalf("journal leaked or stored the wrong verifier: %s", body)
	}

	raw, err := hex.DecodeString(foreign)
	if err != nil {
		t.Fatal(err)
	}
	dir := writeJournal(t, jl(t, serverMap())+jl(t, map[string]any{
		"type": "issue", "ts": tsEarly, "id": invite.CodeID(raw), "verifier": foreign, "expires": expFar,
	}))
	foreignReg := openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(bytes.Repeat([]byte{0x02}, 16))})
	if _, _, err := foreignReg.Register(invite.Format(norm), inst(0x11), "seat a"); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("rv verifier verified here: %v", err)
	}
	if foreignReg.List()[0].State != "unused" {
		t.Fatal(foreignReg.List()[0].State)
	}
}

func TestRegisterCheckOrder(t *testing.T) {
	start := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	clk := &clock{t: start}
	// Server id, one code, then exactly one token. Refusals must not consume it.
	var entropy []byte
	entropy = append(entropy, bytes.Repeat([]byte{0x01}, 16)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x02}, invite.EntropyBytes)...)
	tokenBytes := bytes.Repeat([]byte{0x44}, 32)
	entropy = append(entropy, tokenBytes...)
	r := openReg(t, "", Options{Now: clk.Now, Entropy: bytes.NewReader(entropy), MaxSeats: 1})
	code, row, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	wantToken := hex.EncodeToString(tokenBytes)

	refuse := func(name, code, id, seatName string, want error) {
		t.Helper()
		before := fileBytes(t, r)
		_, _, err := r.Register(code, id, seatName)
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(fileBytes(t, r), before) {
			t.Fatalf("%s appended", name)
		}
	}

	refuse("bad code beats bad id", "!!!", "nope", "", ErrJoinRefused)
	refuse("unknown code", invite.Format(strings.Repeat("0", invite.CodeLength)), inst(0x11), "seat a", ErrJoinRefused)

	clk.t = row.Expires
	refuse("expired", code, inst(0x11), "seat a", ErrJoinRefused)
	clk.t = start
	if stateOf(t, r, row.ID) != "unused" {
		t.Fatal("clock did not restore the code")
	}

	if _, err := r.Revoke(row.ID); err != nil {
		t.Fatal(err)
	}
	refuse("revoked", code, inst(0x11), "seat a", ErrJoinRefused)
	// A new code for the remaining checks. The revoked code stays revoked.
	r.entropy = &countReader{n: 100}
	live, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	r.entropy = bytes.NewReader(append(append([]byte{}, tokenBytes...), bytes.Repeat([]byte{0x55}, invite.EntropyBytes)...))

	refuse("uppercase id", live, strings.ToUpper(inst(0xab)), "seat a", ErrInvalid)
	refuse("short id", live, "abcd", "seat a", ErrInvalid)
	refuse("long id", live, inst(0x11)+"aa", "seat a", ErrInvalid)
	refuse("empty id", live, "", "seat a", ErrInvalid)
	refuse("non-hex id", live, strings.Repeat("zz", 16), "seat a", ErrInvalid)
	refuse("empty name", live, inst(0x11), "", ErrInvalid)
	refuse("long name", live, inst(0x11), strings.Repeat("a", 65), ErrInvalid)
	refuse("invalid utf-8", live, inst(0x11), "a\xff", ErrInvalid)
	refuse("leading space", live, inst(0x11), " seat", ErrInvalid)
	refuse("trailing space", live, inst(0x11), "seat ", ErrInvalid)
	refuse("control", live, inst(0x11), "sea\nt", ErrInvalid)
	if stateOf(t, r, codeID(t, live)) != "unused" {
		t.Fatal("refusals consumed the live code")
	}

	token, _, err := r.Register(live, inst(0x11), "seat a")
	if err != nil || token != wantToken {
		t.Fatalf("register %q %v", token, err)
	}
	if _, ok := r.Authenticate(token); !ok {
		t.Fatal("predicted token did not authenticate")
	}

	other, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// The installation is active and the limit is 1, so the earlier error wins.
	refuse("already registered beats full", other, inst(0x11), "seat a", ErrAlreadyRegistered)
	if stateOf(t, r, codeID(t, other)) != "unused" {
		t.Fatal("already-registered consumed the code")
	}
	refuse("full", other, inst(0x22), "seat b", ErrFull)
	if stateOf(t, r, codeID(t, other)) != "unused" {
		t.Fatal("full consumed the code")
	}

	// A redeemed code is refused even when the installation and the name are also wrong.
	refuse("redeemed beats invalid", live, "nope", "", ErrJoinRefused)
}

func TestTokenAuthenticationAndJournalSecrets(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	norm, err := invite.Parse(code)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := r.Register(code, inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 || !goodHex(token, 32) {
		t.Fatalf("token %q", token)
	}
	seat, ok := r.Authenticate(token)
	if !ok || seat.ID != inst(0x11) || seat.Name != "seat a" || seat.Registered.IsZero() {
		t.Fatalf("auth %+v %v", seat, ok)
	}
	found, ok := r.Seat(inst(0x11))
	if !ok || found != seat {
		t.Fatalf("seat %+v %+v %v", found, seat, ok)
	}
	if _, ok := r.Seat(inst(0x99)); ok {
		t.Fatal("unknown seat")
	}
	wrong := token[:len(token)-1] + "0"
	if token[len(token)-1] == '0' {
		wrong = token[:len(token)-1] + "1"
	}
	if _, ok := r.Authenticate(wrong); ok {
		t.Fatal("wrong token")
	}
	if _, ok := r.Authenticate(""); ok {
		t.Fatal("empty token")
	}
	body := string(fileBytes(t, r))
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	if !strings.Contains(body, hash) || strings.Contains(body, token) || strings.Contains(body, norm) || strings.Contains(body, code) {
		t.Fatal("journal stored a secret or dropped the hash")
	}
	if _, err := r.Revoke(inst(0x11)); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate(token); ok {
		t.Fatal("revoked token")
	}
	if _, ok := r.Seat(inst(0x11)); ok {
		t.Fatal("revoked seat")
	}
	if len(r.Seats()) != 0 {
		t.Fatal("seats not empty")
	}
}

func TestSeatsAreActiveOnlyAndOrdered(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	high, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	low, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(high, inst(0xff), "seat high"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(low, inst(0x00), "a b"); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("a", 64)
	third, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(third, inst(0x10), longName); err != nil {
		t.Fatal(err)
	}
	seats := r.Seats()
	if len(seats) != 3 || seats[0].ID != inst(0x00) || seats[1].ID != inst(0x10) || seats[2].ID != inst(0xff) {
		t.Fatalf("order %+v", seats)
	}
	if seats[0].Name != "a b" || seats[1].Name != longName {
		t.Fatalf("names %+v", seats)
	}
	if _, err := r.Revoke(inst(0x00)); err != nil {
		t.Fatal(err)
	}
	left := r.Seats()
	if len(left) != 2 || left[0].ID != inst(0x10) {
		t.Fatalf("after revoke %+v", left)
	}
}

func TestSeatLimit(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}, MaxSeats: 1})
	first, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	second, secondRow, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(first, inst(0x11), "seat a"); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, r)
	if _, _, err := r.Register(second, inst(0x22), "seat b"); !errors.Is(err, ErrFull) {
		t.Fatalf("limit %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) || stateOf(t, r, secondRow.ID) != "unused" {
		t.Fatal("full register appended")
	}

	unlimited := openReg(t, "", Options{Entropy: &countReader{}})
	if unlimited.maxSeats != defaultMaxSeats {
		t.Fatalf("default %d", unlimited.maxSeats)
	}
	code, _, err := unlimited.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unlimited.Register(code, inst(0x33), "seat c"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeByCodeAndByInstallation(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	unused, unusedRow, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, r)
	row, err := r.Revoke(unusedRow.ID)
	if err != nil || row.State != "revoked" || row.ID != unusedRow.ID || row.Seat != "" {
		t.Fatalf("unused revoke %+v %v", row, err)
	}
	if bytes.Equal(fileBytes(t, r), before) {
		t.Fatal("unused revoke appended nothing")
	}
	again, err := r.Revoke(unusedRow.ID)
	if err != nil || again.State != "revoked" || again.ID != unusedRow.ID {
		t.Fatalf("second %+v %v", again, err)
	}
	if strings.Count(string(fileBytes(t, r)), `"type":"revoke"`) != 1 {
		t.Fatal("second revoke appended")
	}
	if _, _, err := r.Register(unused, inst(0x11), "seat a"); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("revoked code %v", err)
	}

	live, liveRow, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(live, inst(0x22), "seat b"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(fileBytes(t, r)), "\n")
	bySeat, err := r.Revoke(inst(0x22))
	if err != nil || bySeat.ID != liveRow.ID || bySeat.State != "revoked" || bySeat.Seat != inst(0x22) || bySeat.Name != "seat b" {
		t.Fatalf("by seat %+v %v", bySeat, err)
	}
	var found bool
	for _, row := range r.List() {
		if row.ID == liveRow.ID && row.Name == "seat b" && row.State == "revoked" && row.Seat == inst(0x22) {
			found = true
		}
	}
	if !found {
		t.Fatalf("list %+v", r.List())
	}
	second, err := r.Revoke(liveRow.ID)
	if err != nil || second.ID != liveRow.ID {
		t.Fatalf("second redeemed %+v %v", second, err)
	}
	if strings.Count(string(fileBytes(t, r)), "\n") != lines+1 {
		t.Fatal("second redeemed revoke appended")
	}
	if _, err := r.Revoke(inst(0x22)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("inactive installation %v", err)
	}
	unknownBefore := fileBytes(t, r)
	if _, err := r.Revoke(inst(0x99)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown %v", err)
	}
	if _, err := r.Revoke("not-a-key"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("garbage %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), unknownBefore) {
		t.Fatal("unknown revoke appended")
	}
}

func TestReregisterAfterRevokeReportsTheID(t *testing.T) {
	dir := t.TempDir()
	a := openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: &countReader{n: 1}, Name: "relay a"})
	b := openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: &countReader{n: 200}, Name: "other"})
	if b.Server() != a.Server() {
		t.Fatalf("servers %+v %+v", a.Server(), b.Server())
	}
	code, _, err := a.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.List()) != 0 {
		t.Fatal("b saw the issue before refresh")
	}
	if _, err := b.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(b.List()) != 1 || b.List()[0].State != "unused" {
		t.Fatalf("b after refresh %+v", b.List())
	}
	token, _, err := a.Register(code, inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Seats()) != 0 {
		t.Fatal("b saw the seat before replaying")
	}
	// Revoke replays first, so the bind is visible and the seat ends.
	if _, err := b.Revoke(inst(0x11)); err != nil {
		t.Fatal(err)
	}
	got, err := a.Refresh()
	if err != nil || len(got) != 1 || got[0] != inst(0x11) {
		t.Fatalf("refresh reported %v %v", got, err)
	}
	again, err := a.Refresh()
	if err != nil || len(again) != 0 {
		t.Fatalf("second refresh %v %v", again, err)
	}
	if _, ok := a.Authenticate(token); ok {
		t.Fatal("old token survived refresh")
	}
	fresh, _, err := a.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// The revoke is already applied, so this registration reports nothing.
	newToken, reported, err := a.Register(fresh, inst(0x11), "seat a")
	if err != nil || len(reported) != 0 {
		t.Fatalf("same registry reregister %v %v", reported, err)
	}
	if _, ok := a.Authenticate(newToken); !ok {
		t.Fatal("new token")
	}
	if _, ok := a.Authenticate(token); ok {
		t.Fatal("old token")
	}

	// A registration that replays the revoke itself reports the installation.
	dir = t.TempDir()
	a = openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: &countReader{n: 1}})
	b = openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: &countReader{n: 200}})
	code, _, err = a.Issue()
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := a.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = a.Register(code, inst(0x22), "seat b"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Revoke(codeID(t, code)); err != nil {
		t.Fatal(err)
	}
	// Issue would replay the revoke itself. Register is the call that sees it.
	replacement, reported, err := a.Register(next, inst(0x22), "seat b")
	if err != nil || len(reported) != 1 || reported[0] != inst(0x22) {
		t.Fatalf("reported %v %v", reported, err)
	}
	reported[0] = "ffffffffffffffffffffffffffffffff"
	if seats := a.Seats(); len(seats) != 1 || seats[0].ID != inst(0x22) || seats[0].Name != "seat b" {
		t.Fatalf("alias %+v", seats)
	}
	if _, ok := a.Authenticate(replacement); !ok {
		t.Fatal("replacement token")
	}
}

func TestCollisionRetry(t *testing.T) {
	server := bytes.Repeat([]byte{0x01}, 16)
	codeA := bytes.Repeat([]byte{0x11}, invite.EntropyBytes)
	codeB := bytes.Repeat([]byte{0x22}, invite.EntropyBytes)
	var buf []byte
	buf = append(buf, server...)
	buf = append(buf, codeA...)
	for range invite.CollisionRetries - 1 {
		buf = append(buf, codeA...)
	}
	buf = append(buf, codeB...)
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(buf)})
	first, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	wantA, err := invite.Generate(bytes.NewReader(codeA))
	if err != nil {
		t.Fatal(err)
	}
	if first != invite.Format(wantA) {
		t.Fatalf("first %s", first)
	}
	second, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := invite.Generate(bytes.NewReader(codeB))
	if err != nil {
		t.Fatal(err)
	}
	if second != invite.Format(wantB) {
		t.Fatalf("second %s, want %s", second, invite.Format(wantB))
	}

	var exhaust []byte
	exhaust = append(exhaust, server...)
	exhaust = append(exhaust, codeA...)
	exhaust = append(exhaust, bytes.Repeat(codeA, invite.CollisionRetries)...)
	stuck := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(exhaust)})
	if _, _, err := stuck.Issue(); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, stuck)
	if _, _, err := stuck.Issue(); !errors.Is(err, errCollision) {
		t.Fatalf("exhaust %v", err)
	}
	if !bytes.Equal(fileBytes(t, stuck), before) || stuck.Status().Codes != 1 {
		t.Fatal("collision wrote a record")
	}
}

func TestEntropyFailureWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	_, err := Open(dir, Options{Entropy: errReader{errors.New("entropy failed")}})
	if err == nil || err.Error() != "entropy failed" {
		t.Fatalf("open %v", err)
	}
	body, readErr := os.ReadFile(filepath.Join(dir, journalName))
	if readErr != nil || len(body) != 0 {
		t.Fatalf("journal %q %v", body, readErr)
	}

	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &prefixReader{
		rest: bytes.Repeat([]byte{0x01}, 16),
		err:  errors.New("entropy failed"),
	}})
	before := fileBytes(t, r)
	if _, _, err := r.Issue(); err == nil || err.Error() != "entropy failed" {
		t.Fatalf("issue %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) || r.Status().Codes != 0 {
		t.Fatal("failed issue wrote")
	}

	okReg := openReg(t, r.dir, Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	if okReg.Server().ID != r.Server().ID {
		t.Fatal("server changed")
	}
	code, row, err := okReg.Issue()
	if err != nil {
		t.Fatal(err)
	}
	okReg.entropy = errReader{io.ErrUnexpectedEOF}
	before = fileBytes(t, okReg)
	if _, _, err := okReg.Register(code, inst(0x11), "seat a"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("register entropy %v", err)
	}
	if !bytes.Equal(fileBytes(t, okReg), before) || stateOf(t, okReg, row.ID) != "unused" {
		t.Fatal("failed register wrote")
	}
}

func TestReturnedSlicesAreCopies(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	list := r.List()
	list[0].ID = "changed"
	list[0].Name = "changed"
	if got := r.List(); got[0].ID == "changed" || got[0].Name == "changed" {
		t.Fatalf("list aliased %+v", got)
	}
	if _, _, err := r.Register(code, inst(0x11), "seat a"); err != nil {
		t.Fatal(err)
	}
	seats := r.Seats()
	seats[0].Name = "changed"
	if r.Seats()[0].Name != "seat a" {
		t.Fatal("seats aliased")
	}
}

func TestCloseReleasesTheFileAndKeepsLoadedState(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := r.Register(code, inst(0x11), "seat a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Issue(); !errors.Is(err, errClosed) {
		t.Fatalf("issue %v", err)
	}
	if _, err := r.Refresh(); !errors.Is(err, errClosed) {
		t.Fatalf("refresh %v", err)
	}
	if _, err := r.Revoke(inst(0x11)); !errors.Is(err, errClosed) {
		t.Fatalf("revoke %v", err)
	}
	if _, _, err := r.Register(code, inst(0x22), "seat b"); !errors.Is(err, errClosed) {
		t.Fatalf("register %v", err)
	}
	if r.List()[0].State != "redeemed" || r.Server().Name == "" {
		t.Fatal("loaded state dropped")
	}
	if _, ok := r.Authenticate(token); !ok {
		t.Fatal("authenticate after close")
	}
}

func TestTornTailIsRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()
	a := openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: &countReader{n: 1}})
	if _, _, err := a.Issue(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(a.Path(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("TORN"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := a.Refresh(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fileBytes(t, a)), "TORN") == false {
		t.Fatal("refresh truncated the tail")
	}
	if a.Status().Codes != 1 {
		t.Fatal("refresh applied the tail")
	}
	b := openReg(t, dir, Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(bytes.Repeat([]byte{0x03}, 16))})
	if strings.Contains(string(fileBytes(t, b)), "TORN") {
		t.Fatal("open left the torn tail")
	}
	if b.Server() != a.Server() || b.Status().Codes != 1 {
		t.Fatalf("b %+v codes %d", b.Server(), b.Status().Codes)
	}
	if _, _, err := a.Issue(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Refresh(); err != nil || b.Status().Codes != 2 {
		t.Fatalf("b refresh %v %+v", err, b.Status())
	}
}

func TestOpenRefusesBadPaths(t *testing.T) {
	if _, err := Open("", Options{}); !errors.Is(err, errStateDir) {
		t.Fatalf("empty %v", err)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Open(missing, Options{MaxSeats: -1}); !errors.Is(err, errNegative) {
		t.Fatalf("negative %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("negative limit created a directory")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, Options{}); err == nil {
		t.Fatal("opened a file as a directory")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, journalName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("opened a directory as the journal")
	}
}

func TestMissingStateDirectoryIsCreated0700(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	dir := filepath.Join(parent, "state")
	openReg(t, dir, Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16)), Now: fixed(tsEarly)})
	for _, path := range []string{parent, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %o", path, info.Mode().Perm())
		}
	}
}

func TestExistingDirectoryMustAlreadyBe0700(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16)), Now: fixed(tsEarly)})
	if err == nil || err.Error() != "state directory: mode 0755, want 0700" {
		t.Fatalf("Open %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != "keep" {
		t.Fatalf("marker %q %v", body, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, journalName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("Open wrote a journal")
	}
}

func TestFileInPlaceOfStateDirectoryIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(dir, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, Options{})
	if err == nil || err.Error() != "state directory: not a directory" {
		t.Fatalf("Open %v", err)
	}
	body, err := os.ReadFile(dir)
	if err != nil || string(body) != "keep" {
		t.Fatalf("file %q %v", body, err)
	}

	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err = Open(link, Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{0x02}, 16))})
	if err == nil || err.Error() != "state directory: not a directory" {
		t.Fatalf("symlink %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode %v %v", info, err)
	}
	body, err = os.ReadFile(marker)
	if err != nil || string(body) != "keep" {
		t.Fatalf("marker %q %v", body, err)
	}
	if _, statErr := os.Stat(filepath.Join(target, journalName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("Open wrote through the symlink")
	}

	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(parent, "child"), Options{}); err == nil {
		t.Fatal("opened a directory under a file")
	}
	body, err = os.ReadFile(parent)
	if err != nil || string(body) != "parent" {
		t.Fatalf("parent %q %v", body, err)
	}

	if _, err := Open("bad\x00name", Options{}); err == nil {
		t.Fatal("opened a path containing NUL")
	}
}

func TestServerNameFollowsTheSeatRule(t *testing.T) {
	bad := []struct {
		label string
		name  string
	}{
		{"leading space", " a"},
		{"trailing space", "a "},
		{"65 bytes", strings.Repeat("b", 65)},
		{"newline", "a\nb"},
		{"escape", "a\x1bb"},
		{"invalid utf-8", "\xff"},
	}
	for _, tc := range bad {
		t.Run(tc.label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			_, err := Open(dir, Options{Name: tc.name, Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16))})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Open %v", err)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Open created %s", dir)
			}
		})
	}

	t.Run("existing directory is unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "marker")
		if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Open(dir, Options{Name: " a", Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16))})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("Open %v", err)
		}
		after, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mode().Perm() != before.Mode().Perm() {
			t.Fatalf("mode %o", after.Mode().Perm())
		}
		body, err := os.ReadFile(marker)
		if err != nil || string(body) != "keep" {
			t.Fatalf("marker %q %v", body, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, journalName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("Open wrote a journal")
		}
	})

	t.Run("existing journal is unchanged", func(t *testing.T) {
		dir := writeJournal(t, jl(t, serverMap()))
		path := filepath.Join(dir, journalName)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Open(dir, Options{Name: " a", Now: fixed(tsEarly)})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("Open %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("journal changed %q %v", after, err)
		}
	})

	replay := []struct {
		label string
		name  string
	}{
		{"replay leading space", " a"},
		{"replay trailing space", "a "},
		{"replay 65 bytes", strings.Repeat("b", 65)},
		{"replay newline", "a\nb"},
		{"replay escape", "a\x1bb"},
	}
	for _, tc := range replay {
		t.Run(tc.label, func(t *testing.T) {
			rec := serverMap()
			rec["name"] = tc.name
			dir := writeJournal(t, jl(t, rec))
			_, err := Open(dir, Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16))})
			if !errors.Is(err, errMalformed) {
				t.Fatalf("replay %v", err)
			}
		})
	}
}

func TestMarshalFailureWritesNothing(t *testing.T) {
	orig := marshalRec
	t.Cleanup(func() { marshalRec = orig })
	marshalRec = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Open(dir, Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16))}); err == nil || err.Error() != "marshal failed" {
		t.Fatalf("open %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil || len(body) != 0 {
		t.Fatalf("journal %q %v", body, err)
	}

	marshalRec = orig
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	marshalRec = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	before := fileBytes(t, r)
	if _, _, err := r.Issue(); err == nil || err.Error() != "marshal failed" {
		t.Fatalf("issue %v", err)
	}
	codeEntropy := &countReader{n: 50}
	marshalRec = orig
	r.entropy = codeEntropy
	code, row, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	marshalRec = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	if _, _, err := r.Register(code, inst(0x11), "seat a"); err == nil || err.Error() != "marshal failed" {
		t.Fatalf("register %v", err)
	}
	if stateOf(t, r, row.ID) != "unused" {
		t.Fatal("marshal failure bound the code")
	}
	if _, err := r.Revoke(row.ID); err == nil || err.Error() != "marshal failed" {
		t.Fatalf("revoke %v", err)
	}
	if !bytes.Equal(fileBytes(t, r), before) && stateOf(t, r, row.ID) != "unused" {
		t.Fatal("journal changed")
	}
	// before was taken before the successful Issue, so compare against a fresh read's stability:
	stable := fileBytes(t, r)
	if strings.Count(string(stable), `"type":"bind"`) != 0 || strings.Count(string(stable), `"type":"revoke"`) != 0 {
		t.Fatalf("unexpected records %s", stable)
	}
}

func TestStrictReplay(t *testing.T) {
	norm, id, ver := codeParts(t, 0x31)
	otherNorm, otherID, otherVer := codeParts(t, 0x32)
	seat := inst(0x11)
	hash := strings.Repeat("ab", 32)
	goodBind := func(codeID, ts string) string {
		t.Helper()
		return jl(t, map[string]any{
			"type": "bind", "ts": ts, "id": codeID, "seat": seat, "name": "seat a", "token_sha256": hash,
		})
	}
	base := jl(t, serverMap()) + jl(t, issueMap(id, ver, tsEarly, expFar))

	cases := []struct {
		name string
		body string
		want error
	}{
		{"unknown type", base + jl(t, map[string]any{"type": "nope", "ts": tsEarly}), errUnknownType},
		{"missing type", jl(t, map[string]any{"ts": tsEarly, "id": serverHex, "name": "relay a"}), errMalformed},
		{"null", "null\n", errMalformed},
		{"bad json", "{\n", nil},
		{"unknown field", jl(t, map[string]any{"type": "server", "ts": tsEarly, "id": serverHex, "name": "relay a", "extra": 1}), nil},
		{"trailing data", strings.TrimSuffix(jl(t, serverMap()), "\n") + jl(t, serverMap()), errTrailing},
		{"empty line", base + "\n", errEmpty},
		{"missing time", jl(t, map[string]any{"type": "server", "id": serverHex, "name": "relay a"}), nil},
		{"bad time", jl(t, map[string]any{"type": "server", "ts": "yesterday", "id": serverHex, "name": "relay a"}), nil},
		{"bad server id", jl(t, map[string]any{"type": "server", "ts": tsEarly, "id": "zz", "name": "relay a"}), errMalformed},
		{"empty server name", jl(t, map[string]any{"type": "server", "ts": tsEarly, "id": serverHex, "name": ""}), errMalformed},
		{"uppercase server id", jl(t, map[string]any{"type": "server", "ts": tsEarly, "id": strings.ToUpper(serverHex), "name": "relay a"}), errMalformed},
		{"server extra field", jl(t, map[string]any{"type": "server", "ts": tsEarly, "id": serverHex, "name": "relay a", "verifier": ver}), errMalformed},
		{"second server", jl(t, serverMap()) + jl(t, serverMap()), errSecond},
		{"issue before server", jl(t, issueMap(id, ver, tsEarly, expFar)), errBefore},
		{"bind before server", goodBind(id, tsEarly), errBefore},
		{"revoke before server", jl(t, map[string]any{"type": "revoke", "ts": tsEarly, "id": id}), errBefore},
		{"malformed issue", base + jl(t, map[string]any{"type": "issue", "ts": tsEarly, "id": id, "expires": expFar}), errMalformed},
		{"uppercase verifier", base + jl(t, issueMap(id, strings.ToUpper(ver), tsEarly, expFar)), errMalformed},
		{"issue extra", base + jl(t, map[string]any{"type": "issue", "ts": tsMid, "id": id, "verifier": ver, "expires": expFar, "name": "x"}), errMalformed},
		{"missing expires", base + jl(t, map[string]any{"type": "issue", "ts": tsMid, "id": id, "verifier": ver}), nil},
		{"bad expires", base + jl(t, issueMap(id, ver, tsMid, "soon")), nil},
		{"id mismatch", base + jl(t, issueMap(strings.Repeat("cd", 16), ver, tsMid, expFar)), errIssueID},
		{"duplicate issue", base + jl(t, issueMap(id, ver, tsMid, expFar)), errDuplicate},
		{"bind unknown", base + goodBind(strings.Repeat("cd", 16), tsEarly), errNoCode},
		{"bind bad id", base + jl(t, map[string]any{"type": "bind", "ts": tsEarly, "id": "zz", "seat": seat, "name": "seat a", "token_sha256": hash}), errMalformed},
		{"bind expired", jl(t, serverMap()) + jl(t, issueMap(id, ver, tsEarly, tsEarly)) + goodBind(id, tsEarly), errNotUnused},
		{"bind revoked", base + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": id}) + goodBind(id, tsMid), errNotUnused},
		{"bind twice", base + goodBind(id, tsMid) + goodBind(id, tsMid), errNotUnused},
		{"bind bad name", base + jl(t, map[string]any{"type": "bind", "ts": tsMid, "id": id, "seat": seat, "name": "", "token_sha256": hash}), errMalformed},
		{"bind bad seat", base + jl(t, map[string]any{"type": "bind", "ts": tsMid, "id": id, "seat": "zz", "name": "seat a", "token_sha256": hash}), errMalformed},
		{"bind bad token", base + jl(t, map[string]any{"type": "bind", "ts": tsMid, "id": id, "seat": seat, "name": "seat a", "token_sha256": "zz"}), errMalformed},
		{"bind extra", base + jl(t, map[string]any{"type": "bind", "ts": tsMid, "id": id, "seat": seat, "name": "seat a", "token_sha256": hash, "verifier": ver}), errMalformed},
		{"bind active", base + jl(t, issueMap(otherID, otherVer, tsMid, expFar)) + goodBind(id, tsMid) + jl(t, map[string]any{
			"type": "bind", "ts": tsMid, "id": otherID, "seat": seat, "name": "seat a", "token_sha256": strings.Repeat("cd", 32),
		}), errActive},
		{"revoke unknown", base + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": strings.Repeat("cd", 16)}), errNoCode},
		{"revoke bad id", base + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": "zz"}), errMalformed},
		{"revoke extra", base + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": id, "seat": seat}), errMalformed},
	}
	if invite.CodeID(invite.Verifier(otherNorm)) != otherID {
		t.Fatal("fixture")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeJournal(t, tc.body)
			_, err := Open(dir, Options{Now: fixed(tsEarly), Entropy: bytes.NewReader(bytes.Repeat([]byte{0x01}, 16))})
			if err == nil {
				t.Fatal("opened a bad journal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.name == "unknown field" && !strings.Contains(err.Error(), "unknown field") {
				t.Fatal(err)
			}
			if tc.name == "missing time" && !strings.Contains(err.Error(), "missing time") {
				t.Fatal(err)
			}
			if tc.name == "bad time" && !strings.Contains(err.Error(), "bad time") {
				t.Fatal(err)
			}
			if tc.name == "bad expires" && !strings.Contains(err.Error(), "bad time") {
				t.Fatal(err)
			}
			if tc.name == "missing expires" && !strings.Contains(err.Error(), "missing time") {
				t.Fatal(err)
			}
		})
	}

	// A second revoke line is idempotent. The seat is gone and Open succeeds.
	body := base + goodBind(id, tsMid) + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": id}) + jl(t, map[string]any{"type": "revoke", "ts": tsMid, "id": id})
	r := openReg(t, writeJournal(t, body), Options{Now: fixed(tsEarly)})
	if r.List()[0].State != "revoked" || len(r.Seats()) != 0 {
		t.Fatalf("double revoke %+v seats %d", r.List(), len(r.Seats()))
	}
	lines := strings.Count(string(fileBytes(t, r)), "\n")
	if _, err := r.Revoke(id); err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(fileBytes(t, r)), "\n") != lines {
		t.Fatal("api revoke appended after two journal revokes")
	}

	// Whitespace after one object is still one record.
	padded := strings.TrimSuffix(jl(t, serverMap()), "\n") + "  \n"
	if _, err := Open(writeJournal(t, padded), Options{
		Now:     fixed(tsEarly),
		Entropy: bytes.NewReader(bytes.Repeat([]byte{0x7a}, 16)),
	}); err != nil {
		t.Fatal(err)
	}
	_ = norm
}

func TestCorruptTailSticks(t *testing.T) {
	norm, id, ver := codeParts(t, 0x41)
	base := jl(t, serverMap()) + jl(t, issueMap(id, ver, tsEarly, expFar))
	r := openReg(t, writeJournal(t, base), Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	if r.Status().Unused != 1 {
		t.Fatal(r.Status())
	}
	appendRaw(t, r.Path(), `{"type":"nope","ts":"2026-11-12T09:30:00Z"}`+"\n")
	before := fileBytes(t, r)
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"refresh", func() error { _, err := r.Refresh(); return err }},
		{"refresh again", func() error { _, err := r.Refresh(); return err }},
		{"issue", func() error { _, _, err := r.Issue(); return err }},
		{"revoke", func() error { _, err := r.Revoke(id); return err }},
		{"register", func() error { _, _, err := r.Register(norm, inst(0x11), "seat a"); return err }},
	} {
		err := call.fn()
		if !errors.Is(err, errUnknownType) {
			t.Fatalf("%s %v", call.name, err)
		}
	}
	if !bytes.Equal(fileBytes(t, r), before) || stateOf(t, r, id) != "unused" {
		t.Fatal("corrupt tail changed state")
	}
}

func TestNameBoundaries(t *testing.T) {
	r := openReg(t, "", Options{Now: fixed(tsEarly), Entropy: &countReader{}})
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	name := "a\u00e9b"
	if _, _, err := r.Register(code, inst(0x11), name); err != nil {
		t.Fatal(err)
	}
	if got := r.Seats()[0].Name; got != name {
		t.Fatalf("name %q", got)
	}
	if !validName(strings.Repeat("a", 64)) || validName(strings.Repeat("a", 65)) || validName(" ") || validName("a ") || validName(" a") {
		t.Fatal("validName boundaries")
	}
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type countReader struct{ n uint64 }

func (c *countReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n)
		c.n++
	}
	return len(p), nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type prefixReader struct {
	rest []byte
	err  error
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if len(p.rest) == 0 {
		if p.err == nil {
			return 0, io.EOF
		}
		return 0, p.err
	}
	n := copy(b, p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

func openReg(t *testing.T, dir string, opts Options) *Registry {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	r, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func fileBytes(t *testing.T, r *Registry) []byte {
	t.Helper()
	b, err := os.ReadFile(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixed(ts string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

func inst(b byte) string {
	return strings.Repeat(hex.EncodeToString([]byte{b}), 16)
}

func codeID(t *testing.T, formatted string) string {
	t.Helper()
	norm, err := invite.Parse(formatted)
	if err != nil {
		t.Fatal(err)
	}
	return invite.CodeID(invite.Verifier(norm))
}

func stateOf(t *testing.T, r *Registry, id string) string {
	t.Helper()
	for _, row := range r.List() {
		if row.ID == id {
			return row.State
		}
	}
	t.Fatalf("missing code %s", id)
	return ""
}

func codeParts(t *testing.T, fill byte) (norm, id, verifier string) {
	t.Helper()
	var err error
	norm, err = invite.Generate(bytes.NewReader(bytes.Repeat([]byte{fill}, invite.EntropyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	sum := invite.Verifier(norm)
	return norm, invite.CodeID(sum), hex.EncodeToString(sum)
}

func rvVerifierHex(norm string) string {
	h := sha256.New()
	h.Write([]byte("scimux-rv/invite-verifier/v1"))
	h.Write([]byte{0})
	h.Write([]byte(norm))
	return hex.EncodeToString(h.Sum(nil))
}

const serverHex = "0123456789abcdef0123456789abcdef"

func serverMap() map[string]any {
	return map[string]any{"type": "server", "ts": tsEarly, "id": serverHex, "name": "relay a"}
}

func issueMap(id, verifier, ts, exp string) map[string]any {
	return map[string]any{"type": "issue", "ts": ts, "id": id, "verifier": verifier, "expires": exp}
}

func jl(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func writeJournal(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
