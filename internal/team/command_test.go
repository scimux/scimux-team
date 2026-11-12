package team

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/invite"
	"github.com/scimux/scimux-team/internal/registry"
)

const revokeSentence = "the seat's token stops working and its channels and memberships are removed"

func TestExitCodes(t *testing.T) {
	if ExitCode(nil) != exitOK {
		t.Fatal("nil")
	}
	if ExitCode(errors.New("plain")) != exitInternal {
		t.Fatal("plain")
	}
	wrapped := fmt.Errorf("wrap: %w", usageErr(errors.New("u")))
	if ExitCode(wrapped) != exitUsage || usageErr(errors.New("u")).Error() != "u" {
		t.Fatalf("usage %v", wrapped)
	}
	unknown := &exitError{code: exitUnknown, err: errors.New("unknown")}
	if ExitCode(unknown) != exitUnknown {
		t.Fatal("unknown")
	}
	if ExitCode(ioErr(errors.New("io"))) != exitIO || ioErr(nil) != nil {
		t.Fatal("io")
	}
}

func TestIssueHumanAndJSON(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	codeA, idA := mustCode(t, rep(0x11, invite.EntropyBytes))
	codeB, idB := mustCode(t, rep(0x22, invite.EntropyBytes))
	entropy = cat(rep(0x01, 16), rep(0x11, invite.EntropyBytes), rep(0x22, invite.EntropyBytes))
	nowFn = fixedNow
	exp := ts(fixedNow().Add(invite.UnusedTTL))

	stdout, stderr, code, err := runCmd("-state-dir", dir, "-name", "relay a", "issue")
	if err != nil || code != exitOK || stdout != codeA+"\n" || stderr != "id: "+idA+"\n" {
		t.Fatalf("human code=%d err=%v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}
	if strings.Count(stdout, codeA) != 1 {
		t.Fatalf("code printed %d times", strings.Count(stdout, codeA))
	}

	stdout, stderr, code, err = runCmd("-state-dir", dir, "-json", "issue")
	want := `{"code":"` + codeB + `","expires":"` + exp + `","id":"` + idB + `"}` + "\n"
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("json code=%d err=%v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}

	text := fileText(t, filepath.Join(dir, "registry.jsonl"))
	if !strings.Contains(text, `"name":"relay a"`) || !strings.Contains(text, `"id":"`+hex.EncodeToString(rep(0x01, 16))+`"`) {
		t.Fatalf("server record %s", text)
	}
	stdout, _, code, err = runCmd("-state-dir", dir, "-name", "other", "status")
	if err != nil || code != exitOK || !strings.Contains(stdout, "codes: 2\n") {
		t.Fatalf("status after rename %d %v %q", code, err, stdout)
	}
	text = fileText(t, filepath.Join(dir, "registry.jsonl"))
	if strings.Contains(text, `"name":"other"`) || !strings.Contains(text, `"name":"relay a"`) {
		t.Fatal("a later command rewrote the server name")
	}

	linkDir := t.TempDir()
	entropy = cat(rep(0x03, 16), rep(0x33, invite.EntropyBytes))
	base := "http://relay.example:8080"
	stdout, stderr, code, err = runCmd("-state-dir", linkDir, "-url", base, "issue")
	link := base + "/join#" + codeFrom(t, rep(0x33, invite.EntropyBytes))
	id := idFrom(t, rep(0x33, invite.EntropyBytes))
	if err != nil || code != exitOK || stdout != link+"\n" || stderr != "id: "+id+"\n" {
		t.Fatalf("link code=%d err=%v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}

	jsonDir := t.TempDir()
	entropy = cat(rep(0x04, 16), rep(0x34, invite.EntropyBytes))
	stdout, stderr, code, err = runCmd("-state-dir", jsonDir, "-json", "-url", "https://relay.example", "issue")
	codeC := codeFrom(t, rep(0x34, invite.EntropyBytes))
	idC := idFrom(t, rep(0x34, invite.EntropyBytes))
	want = `{"code":"` + codeC + `","expires":"` + exp + `","id":"` + idC + `","link":"https://relay.example/join#` + codeC + `"}` + "\n"
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("json link code=%d err=%v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}
}

func TestURLValidation(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	bad := []string{
		"ftp://relay.example",
		"http://relay.example/join",
		"http://relay.example?x=1",
		"http://relay.example#frag",
		"http://relay.example/",
		"http://",
		"http://[::1",
	}
	for _, raw := range bad {
		dir := filepath.Join(t.TempDir(), "state")
		stdout, stderr, code, err := runCmd("-state-dir", dir, "-url", raw, "issue")
		if code != exitUsage || err == nil || err.Error() != "-url must be an http or https URL with a host and no path, query or fragment" || stdout != "" {
			t.Fatalf("url %q code=%d err=%v stderr %q stdout %q", raw, code, err, stderr, stdout)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("url %q created %s", raw, dir)
		}
	}
	good := []string{
		"https://relay.example",
		"http://user:pass@relay.example:8080",
		"HTTP://relay.example",
	}
	for i, raw := range good {
		dir := t.TempDir()
		fill := byte(0x40 + i)
		entropy = cat(rep(0x05, 16), rep(fill, invite.EntropyBytes))
		stdout, stderr, code, err := runCmd("-state-dir", dir, "-url", raw, "issue")
		wantCode := codeFrom(t, rep(fill, invite.EntropyBytes))
		if err != nil || code != exitOK || stdout != raw+"/join#"+wantCode+"\n" || !strings.HasPrefix(stderr, "id: ") {
			t.Fatalf("url %q code=%d err=%v\nstdout %q\nstderr %q", raw, code, err, stdout, stderr)
		}
	}
}

func TestListHumanJSONAndFilters(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	empty := t.TempDir()
	entropy = cat(rep(0x01, 16))
	stdout, stderr, code, err := runCmd("-state-dir", empty, "list")
	if err != nil || code != exitOK || stdout != "" || stderr != "" {
		t.Fatalf("empty human %d %v %q %q", code, err, stdout, stderr)
	}
	stdout, stderr, code, err = runCmd("-state-dir", empty, "-json", "list")
	if err != nil || code != exitOK || stdout != "[]\n" || stderr != "" {
		t.Fatalf("empty json %d %v %q %q", code, err, stdout, stderr)
	}

	dir := t.TempDir()
	a, b, c := issueThree(t, dir)
	entropy = bytes.NewReader(nil)
	stdout, stderr, code, err = runCmd("-state-dir", dir, "list")
	want := humanRow(a, "unused", "-", "-") + humanRow(b, "redeemed", inst(0x11), "north seat") + humanRow(c, "revoked", "-", "-")
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("list %d %v\n got %q\nwant %q", code, err, stdout, want)
	}
	mustNotContain(t, "list", stdout, a.code, b.code, c.code)

	stdout, _, code, err = runCmd("-state-dir", dir, "-json", "list")
	wantJSON := "[" + jsonRow(a, "unused", "", "") + "," + jsonRow(b, "redeemed", inst(0x11), "north seat") + "," + jsonRow(c, "revoked", "", "") + "]\n"
	if err != nil || code != exitOK || stdout != wantJSON {
		t.Fatalf("json list %d %v\n got %q\nwant %q", code, err, stdout, wantJSON)
	}
	if strings.Contains(strings.Split(stdout, "},{")[0], `"seat"`) || strings.Contains(strings.Split(stdout, "},{")[0], `"name"`) {
		t.Fatalf("unused json has a seat %s", stdout)
	}

	stdout, _, code, err = runCmd("-state-dir", dir, "list", "--unused")
	if err != nil || code != exitOK || stdout != humanRow(a, "unused", "-", "-") {
		t.Fatalf("unused %d %v %q", code, err, stdout)
	}
	stdout, _, code, err = runCmd("-state-dir", dir, "-json", "list", "--revoked")
	if err != nil || code != exitOK || stdout != "["+jsonRow(c, "revoked", "", "")+"]\n" {
		t.Fatalf("revoked json %d %v %q", code, err, stdout)
	}

	nowFn = func() time.Time { return a.expires }
	stdout, _, code, err = runCmd("-state-dir", dir, "list")
	want = humanRow(a, "expired", "-", "-") + humanRow(b, "redeemed", inst(0x11), "north seat") + humanRow(c, "revoked", "-", "-")
	if err != nil || code != exitOK || stdout != want {
		t.Fatalf("expired list %d %v\n got %q\nwant %q", code, err, stdout, want)
	}
	stdout, _, code, err = runCmd("-state-dir", dir, "list", "--unused")
	if err != nil || code != exitOK || stdout != "" {
		t.Fatalf("unused after expiry %d %v %q", code, err, stdout)
	}
}

func TestRevokeHumanJSONAndUnknown(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	dir := t.TempDir()
	a, b, _ := issueThree(t, dir)
	entropy = bytes.NewReader(nil)
	before := fileSize(t, filepath.Join(dir, "registry.jsonl"))

	stdout, stderr, code, err := runCmd("-state-dir", dir, "revoke", a.id)
	want := "revoked " + a.id + "\n" + revokeSentence + "\n"
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("revoke %d %v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}
	mustNotContain(t, "revoke", stdout+stderr, a.code)
	grown := fileSize(t, filepath.Join(dir, "registry.jsonl"))
	if grown <= before {
		t.Fatal("first revoke did not append")
	}

	stdout, _, code, err = runCmd("-state-dir", dir, "revoke", a.id)
	if err != nil || code != exitOK || stdout != want || fileSize(t, filepath.Join(dir, "registry.jsonl")) != grown {
		t.Fatalf("second %d %v size %d want %d stdout %q", code, err, fileSize(t, filepath.Join(dir, "registry.jsonl")), grown, stdout)
	}

	stdout, stderr, code, err = runCmd("-state-dir", dir, "-json", "revoke", inst(0x11))
	wantJSON := `{"id":"` + b.id + `","note":"` + revokeSentence + `","state":"revoked"}` + "\n"
	if err != nil || code != exitOK || stdout != wantJSON || stderr != "" || strings.Contains(stdout, inst(0x11)) {
		t.Fatalf("json revoke %d %v\nstdout %q", code, err, stdout)
	}

	stdout, _, code, err = runCmd("-state-dir", dir, "revoke", "no-such")
	if code != exitUnknown || err == nil || err.Error() != "unknown" || stdout != "" {
		t.Fatalf("unknown %d %v %q", code, err, stdout)
	}
}

func TestStatusHumanAndJSON(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	dir := t.TempDir()
	entropy = cat(rep(0x01, 16))
	logPath := filepath.Join(dir, "registry.jsonl")
	stdout, stderr, code, err := runCmd("-state-dir", dir, "status")
	want := "codes: 0\nunused: 0\nredeemed: 0\nrevoked: 0\nexpired: 0\nlog: " + logPath + "\n"
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("status %d %v\n got %q\nwant %q", code, err, stdout, want)
	}
	stdout, stderr, code, err = runCmd("-state-dir", dir, "-json", "status")
	want = `{"codes":0,"unused":0,"redeemed":0,"revoked":0,"expired":0,"log":"` + logPath + `"}` + "\n"
	if err != nil || code != exitOK || stdout != want || stderr != "" {
		t.Fatalf("json status %d %v %q", code, err, stdout)
	}
	info, err := os.Stat(logPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode %v %v", info, err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", dirInfo, err)
	}

	full := t.TempDir()
	a, _, _ := issueThree(t, full)
	entropy = bytes.NewReader(nil)
	stdout, _, code, err = runCmd("-state-dir", full, "status")
	want = "codes: 3\nunused: 1\nredeemed: 1\nrevoked: 1\nexpired: 0\nlog: " + filepath.Join(full, "registry.jsonl") + "\n"
	if err != nil || code != exitOK || stdout != want {
		t.Fatalf("full status %d %v\n got %q\nwant %q", code, err, stdout, want)
	}
	nowFn = func() time.Time { return a.expires }
	stdout, _, code, err = runCmd("-state-dir", full, "-json", "status")
	want = `{"codes":3,"unused":0,"redeemed":1,"revoked":1,"expired":1,"log":"` + filepath.Join(full, "registry.jsonl") + `"}` + "\n"
	if err != nil || code != exitOK || stdout != want {
		t.Fatalf("expired status %d %v\n got %q\nwant %q", code, err, stdout, want)
	}
}

func TestUsageErrors(t *testing.T) {
	restoreSeams(t)
	cases := []struct {
		args []string
		msg  string
	}{
		{[]string{"issue", "extra"}, "issue takes no arguments"},
		{[]string{"issue", "-json"}, "issue takes no arguments"},
		{[]string{"list", "--expired"}, "list accepts only --unused or --revoked"},
		{[]string{"list", "--unused", "--revoked"}, "list accepts at most one filter"},
		{[]string{"revoke"}, "revoke requires an id or installation id"},
		{[]string{"revoke", "one", "two"}, "revoke requires an id or installation id"},
		{[]string{"status", "extra"}, "status takes no arguments"},
		{[]string{"frob"}, `unknown command "frob"`},
	}
	for _, tc := range cases {
		dir := filepath.Join(t.TempDir(), "state")
		args := append([]string{"-state-dir", dir}, tc.args...)
		stdout, _, code, err := runCmd(args...)
		if code != exitUsage || err == nil || err.Error() != tc.msg || stdout != "" {
			t.Fatalf("args %v code=%d err=%v stdout %q", tc.args, code, err, stdout)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("args %v created state", tc.args)
		}
	}
	stdout, stderr, code, err := runCmd("-bogus")
	if code != exitUsage || err == nil || !strings.Contains(err.Error(), "flag") || stdout != "" || stderr == "" {
		t.Fatalf("flag %d %v stderr %q", code, err, stderr)
	}
}

func TestVersionDoesNotTouchState(t *testing.T) {
	restoreSeams(t)
	dir := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	err := Run([]string{"-version", "-state-dir", dir, "issue"}, "v9-test", &stdout, &stderr)
	if err != nil || ExitCode(err) != exitOK || stdout.String() != "v9-test\n" || stderr.Len() != 0 {
		t.Fatalf("version %v %q %q", err, stdout.String(), stderr.String())
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("version created state")
	}
	err = Run([]string{"-version", "-state-dir", dir}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("version write %v", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("failed version created state")
	}
}

func TestStateDirFromHome(t *testing.T) {
	restoreSeams(t)
	home := t.TempDir()
	userHomeDir = func() (string, error) { return home, nil }
	entropy = cat(rep(0x01, 16), rep(0x11, invite.EntropyBytes))
	nowFn = fixedNow
	stdout, stderr, code, err := runCmd("issue")
	wantCode := codeFrom(t, rep(0x11, invite.EntropyBytes))
	if err != nil || code != exitOK || stdout != wantCode+"\n" || !strings.HasPrefix(stderr, "id: ") {
		t.Fatalf("home issue %d %v %q %q", code, err, stdout, stderr)
	}
	state := filepath.Join(home, ".scimux-team")
	info, err := os.Stat(state)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("home dir %v %v", info, err)
	}
	reg := openRegistry(t, state, registry.Options{Now: fixedNow, Entropy: bytes.NewReader(nil)})
	if reg.Server().Name != "scimux team" {
		t.Fatalf("default name %q", reg.Server().Name)
	}

	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	_, _, code, err = runCmd("status")
	if code != exitIO || err == nil || !strings.Contains(err.Error(), "no home") {
		t.Fatalf("home err %d %v", code, err)
	}
	userHomeDir = func() (string, error) { return "", nil }
	_, _, code, err = runCmd("status")
	if code != exitIO || err == nil || err.Error() != "empty home directory" {
		t.Fatalf("empty home %d %v", code, err)
	}
	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	entropy = cat(rep(0x02, 16))
	_, _, code, err = runCmd("-state-dir", t.TempDir(), "status")
	if err != nil || code != exitOK {
		t.Fatalf("explicit dir ignored home error %d %v", code, err)
	}
}

func TestIssueCollisionAndEntropyFailure(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	one := rep(0x11, invite.EntropyBytes)
	raw := rep(0x01, 16)
	raw = append(raw, one...)
	for range invite.CollisionRetries {
		raw = append(raw, one...)
	}
	dir := t.TempDir()
	entropy = bytes.NewReader(raw)
	stdout, stderr, code, err := runCmd("-state-dir", dir, "issue")
	want, id := mustCode(t, one)
	if err != nil || code != exitOK || stdout != want+"\n" || stderr != "id: "+id+"\n" {
		t.Fatalf("first %d %v %q %q", code, err, stdout, stderr)
	}
	stdout, stderr, code, err = runCmd("-state-dir", dir, "issue")
	if code != exitInternal || err == nil || err.Error() != "registry: invite collision" || stdout != "" || stderr != "" {
		t.Fatalf("collision %d %v %q %q", code, err, stdout, stderr)
	}
	if strings.Count(fileText(t, filepath.Join(dir, "registry.jsonl")), `"type":"issue"`) != 1 {
		t.Fatal("collision appended a code")
	}

	short := t.TempDir()
	entropy = bytes.NewReader(rep(0x02, 16))
	stdout, _, code, err = runCmd("-state-dir", short, "issue")
	if code != exitIO || err == nil || stdout != "" || strings.Contains(fileText(t, filepath.Join(short, "registry.jsonl")), `"type":"issue"`) {
		t.Fatalf("short %d %v %q", code, err, stdout)
	}
	if !strings.Contains(fileText(t, filepath.Join(short, "registry.jsonl")), `"type":"server"`) {
		t.Fatal("server record missing")
	}

	none := t.TempDir()
	entropy = bytes.NewReader(nil)
	stdout, _, code, err = runCmd("-state-dir", none, "issue")
	body := fileText(t, filepath.Join(none, "registry.jsonl"))
	if code != exitIO || err == nil || stdout != "" || strings.Contains(body, "server") || strings.Contains(body, "issue") {
		t.Fatalf("empty entropy %d %v %q file %q", code, err, stdout, body)
	}
}

func TestCorruptJournalIsIO(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	dir := t.TempDir()
	entropy = cat(rep(0x01, 16))
	if _, _, code, err := runCmd("-state-dir", dir, "status"); err != nil || code != exitOK {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "registry.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"nope\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	stdout, _, code, err := runCmd("-state-dir", dir, "status")
	if code != exitIO || err == nil || stdout != "" {
		t.Fatalf("corrupt status %d %v %q", code, err, stdout)
	}

	live := t.TempDir()
	a, _, _ := issueThree(t, live)
	entropy = bytes.NewReader(nil)
	afterOpen = func(reg *registry.Registry) {
		f, err := os.OpenFile(reg.Path(), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("{\"type\":\"nope\"}\n"); err != nil {
			t.Fatal(err)
		}
	}
	journal := filepath.Join(live, "registry.jsonl")
	revokes := strings.Count(fileText(t, journal), `"type":"revoke"`)
	stdout, _, code, err = runCmd("-state-dir", live, "revoke", a.id)
	if code != exitIO || err == nil || errors.Is(err, registry.ErrUnknown) || stdout != "" {
		t.Fatalf("window %d %v %q", code, err, stdout)
	}
	if strings.Count(fileText(t, journal), `"type":"revoke"`) != revokes {
		t.Fatal("failed revoke appended")
	}
}

func TestOperatorWriteFailures(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	dir := t.TempDir()
	entropy = cat(rep(0x01, 16), rep(0x11, invite.EntropyBytes))
	err := Run([]string{"-state-dir", dir, "issue"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("stdout %v", err)
	}
	stdout, _, code, err := runCmd("-state-dir", dir, "list")
	if err != nil || code != exitOK || !strings.Contains(stdout, " unused ") {
		t.Fatalf("stored anyway %d %v %q", code, err, stdout)
	}

	dir = t.TempDir()
	entropy = cat(rep(0x02, 16), rep(0x12, invite.EntropyBytes))
	var out bytes.Buffer
	err = Run([]string{"-state-dir", dir, "issue"}, "dev", &out, failWriter{})
	want := codeFrom(t, rep(0x12, invite.EntropyBytes)) + "\n"
	if ExitCode(err) != exitIO || out.String() != want {
		t.Fatalf("stderr %v %q", err, out.String())
	}

	dir = t.TempDir()
	entropy = cat(rep(0x03, 16), rep(0x13, invite.EntropyBytes))
	err = Run([]string{"-state-dir", dir, "-json", "issue"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("json %v", err)
	}

	dir = t.TempDir()
	issueThree(t, dir)
	entropy = bytes.NewReader(nil)
	err = Run([]string{"-state-dir", dir, "list"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("list %v", err)
	}
	err = Run([]string{"-state-dir", dir, "-json", "list"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("list json %v", err)
	}
	err = Run([]string{"-state-dir", dir, "status"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("status %v", err)
	}
	err = Run([]string{"-state-dir", dir, "-json", "status"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("status json %v", err)
	}
	err = Run([]string{"-state-dir", dir, "revoke", "no-such-but-write"}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitUnknown {
		t.Fatalf("unknown still unknown %v", err)
	}
	aID := codeIDAt(t, dir, 0)
	err = Run([]string{"-state-dir", dir, "revoke", aID}, "dev", failWriter{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("revoke write %v", err)
	}
	stdout, _, code, err = runCmd("-state-dir", dir, "status")
	if err != nil || code != exitOK || !strings.Contains(stdout, "revoked: 2\n") {
		t.Fatalf("revoke stuck %d %v %q", code, err, stdout)
	}
}

func TestServeAnswersHelloAndStops(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	entropy = bytes.NewReader(rep(0x01, 16))
	nowFn = fixedNow
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a", "-json")
	if srv.errb.Len() != 0 {
		t.Fatalf("stderr %q", srv.errb.String())
	}
	if strings.Count(srv.out.String(), "\n") != 1 || strings.Contains(srv.out.String(), "{") {
		t.Fatalf("stdout %q", srv.out.String())
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + srv.addr + "/v1/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol":1,"server":{"id":"` + hex.EncodeToString(rep(0x01, 16)) + `","name":"relay a"}}`
	if resp.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("hello %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", resp.Header)
	}
	info, err := os.Stat(filepath.Join(dir, "server.lock"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock %v %v", info, err)
	}
	srv.wait(t)
	conn, err := net.DialTimeout("tcp", srv.addr, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener still open")
	}
}

func TestSecondServerFailsAndCommandsShareTheDirectory(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	entropy = bytes.NewReader(rep(0x01, 16))
	nowFn = fixedNow
	srv := startServer(t, dir, "127.0.0.1:0")
	var out2, err2 bytes.Buffer
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", dir}, "dev", &out2, &err2)
	if err == nil || err.Error() != "server already running for this state directory" || ExitCode(err) != exitInternal || out2.Len() != 0 {
		t.Fatalf("second %v stdout %q stderr %q", err, out2.String(), err2.String())
	}

	stdout, stderr, code, err := runWithin([]string{"-state-dir", dir, "status"})
	if err != nil || code != exitOK || !strings.Contains(stdout, "codes: 0\n") || stderr != "" {
		t.Fatalf("status while serving %d %v %q %q", code, err, stdout, stderr)
	}
	entropy = bytes.NewReader(rep(0x11, invite.EntropyBytes))
	want := codeFrom(t, rep(0x11, invite.EntropyBytes))
	stdout, stderr, code, err = runWithin([]string{"-state-dir", dir, "issue"})
	if err != nil || code != exitOK || stdout != want+"\n" || !strings.HasPrefix(stderr, "id: ") {
		t.Fatalf("issue while serving %d %v %q %q", code, err, stdout, stderr)
	}
	mustNotContain(t, "server", srv.out.String()+srv.errb.String(), want)

	srv.wait(t)
	entropy = bytes.NewReader(nil)
	again := startServer(t, dir, "127.0.0.1:0")
	again.wait(t)
}

func TestNeedsPublicWarning(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", false},
		{"127.0.0.1:0", false},
		{"[::1]:8080", false},
		{"localhost:8080", false},
		{"LOCALHOST:9", false},
		{"0.0.0.0:8080", true},
		{"8.8.8.8:80", true},
		{"relay.example:80", true},
		{":8080", true},
		{"127.0.0.1", true},
		{"", true},
		{"[::1]", true},
	}
	for _, tc := range cases {
		if got := needsPublicWarning(tc.addr); got != tc.want {
			t.Errorf("needsPublicWarning(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestNonLoopbackWarnsAndListenFailureDoesNot(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	entropy = bytes.NewReader(rep(0x01, 16))
	dir := t.TempDir()
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"-addr", "127.0.0.1:99999", "-state-dir", dir}, "dev", &bytes.Buffer{}, &stderr)
	if ExitCode(err) != exitInternal || err == nil || !strings.Contains(err.Error(), "listen") || stderr.Len() != 0 {
		t.Fatalf("bad port %v stderr %q", err, stderr.String())
	}

	entropy = bytes.NewReader(rep(0x02, 16))
	stderr.Reset()
	err = run(context.Background(), []string{"-addr", "192.0.2.1:80", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &stderr)
	if ExitCode(err) != exitInternal || err == nil || !strings.Contains(err.Error(), "listen") || stderr.String() != publicWarning+"\n" {
		t.Fatalf("non-loopback %v stderr %q", err, stderr.String())
	}

	entropy = bytes.NewReader(rep(0x03, 16))
	stderr.Reset()
	err = run(context.Background(), []string{"-addr", "127.0.0.1", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &stderr)
	if ExitCode(err) != exitInternal || stderr.String() != publicWarning+"\n" {
		t.Fatalf("missing port %v stderr %q", err, stderr.String())
	}
}

func TestServeRejectsBadState(t *testing.T) {
	restoreSeams(t)
	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "no home") {
		t.Fatalf("home %v", err)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", file}, "dev", &stdout, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || stdout.Len() != 0 {
		t.Fatalf("file %v stdout %q", err, stdout.String())
	}
}

func TestServeLockAndLifecycleFailures(t *testing.T) {
	restoreSeams(t)
	nowFn = fixedNow
	cchmod := chmodFile

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "server.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	entropy = bytes.NewReader(rep(0x01, 16))
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", dir}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || strings.Contains(err.Error(), "already running") {
		t.Fatalf("lock dir %v", err)
	}

	entropy = bytes.NewReader(rep(0x02, 16))
	chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != exitIO || err == nil || !strings.Contains(err.Error(), "chmod failed") {
		t.Fatalf("chmod %v", err)
	}

	entropy = bytes.NewReader(rep(0x03, 16))
	chmodFile = cchmod
	flockFn = func(int, int) error { return syscall.EAGAIN }
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "server already running for this state directory" || ExitCode(err) != exitInternal || errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("again %v", err)
	}

	entropy = bytes.NewReader(rep(0x04, 16))
	flockFn = func(int, int) error { return syscall.ENOLCK }
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != exitInternal || !errors.Is(err, syscall.ENOLCK) || strings.Contains(err.Error(), "already running") {
		t.Fatalf("nolock %v", err)
	}
	flockFn = syscall.Flock

	entropy = bytes.NewReader(rep(0x05, 16))
	w := &captureFail{}
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", w, &bytes.Buffer{})
	if ExitCode(err) != exitIO {
		t.Fatalf("listen line %v", err)
	}
	addr := boundAddr(t, w.buf.String())
	conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatal("listener stayed open after a failed listen line")
	}

	entropy = bytes.NewReader(rep(0x06, 16))
	serveFn = func(s *http.Server, ln net.Listener) error {
		ln.Close()
		return errors.New("serve failed")
	}
	shutdownFn = func(*http.Server, context.Context) error { return errors.New("shut failed") }
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "serve failed" {
		t.Fatalf("both %v", err)
	}

	entropy = bytes.NewReader(rep(0x07, 16))
	serveFn = func(s *http.Server, ln net.Listener) error {
		ln.Close()
		return http.ErrServerClosed
	}
	shutdownFn = func(s *http.Server, ctx context.Context) error { return s.Shutdown(ctx) }
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	}()
	cancel()
	select {
	case err = <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closed serve did not return")
	}

	entropy = bytes.NewReader(rep(0x08, 16))
	serveFn = func(s *http.Server, ln net.Listener) error {
		ln.Close()
		return nil
	}
	ctx, cancel = context.WithCancel(context.Background())
	errc = make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-state-dir", t.TempDir()}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	}()
	cancel()
	select {
	case err = <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nil serve did not return")
	}

	entropy = bytes.NewReader(rep(0x09, 16))
	serveFn = func(s *http.Server, ln net.Listener) error { return s.Serve(ln) }
	realShut := shutdownFn
	shutdownFn = func(s *http.Server, ctx context.Context) error {
		if shutErr := realShut(s, ctx); shutErr != nil {
			return shutErr
		}
		return errors.New("shut failed")
	}
	shutSrv := startServer(t, t.TempDir(), "127.0.0.1:0")
	shutSrv.finish()
	if shutSrv.err == nil || shutSrv.err.Error() != "shut failed" {
		t.Fatalf("shutdown %v", shutSrv.err)
	}
}

func TestJoinLinkRegistersAgainstTheServer(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	nowFn = fixedNow
	entropy = cat(rep(0x01, 16), rep(0x44, 32))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	entropy = bytes.NewReader(rep(0x11, invite.EntropyBytes))
	base := "http://" + srv.addr
	stdout, stderr, code, err := runWithin([]string{"-state-dir", dir, "-url", base, "issue"})
	wantCode := codeFrom(t, rep(0x11, invite.EntropyBytes))
	id := idFrom(t, rep(0x11, invite.EntropyBytes))
	if err != nil || code != exitOK || stdout != base+"/join#"+wantCode+"\n" || stderr != "id: "+id+"\n" {
		t.Fatalf("issue %d %v\nstdout %q\nstderr %q", code, err, stdout, stderr)
	}
	frag := strings.TrimPrefix(strings.TrimSpace(stdout), base+"/join#")
	client := &http.Client{Timeout: 2 * time.Second}
	status, body := postRegister(t, client, base, frag, inst(0x11), "seat a")
	token := hex.EncodeToString(rep(0x44, 32))
	want := `{"token":"` + token + `","server":{"id":"` + hex.EncodeToString(rep(0x01, 16)) + `","name":"relay a"}}`
	if status != http.StatusCreated || body != want {
		t.Fatalf("register %d %s", status, body)
	}
	status, body = postRegister(t, client, base, frag, inst(0x22), "seat b")
	if status != http.StatusForbidden || body != `{"error":"join_refused"}` {
		t.Fatalf("second %d %s", status, body)
	}
	journal := fileText(t, filepath.Join(dir, "registry.jsonl"))
	mustNotContain(t, "journal", journal, token, wantCode, frag)
	mustNotContain(t, "server", srv.out.String()+srv.errb.String(), token, wantCode, frag, "seat a")
	srv.wait(t)
}

func TestServeOutputKeepsSecrets(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	nowFn = fixedNow
	entropy = cat(rep(0x08, 16), rep(0x55, 32))
	srv := startServer(t, dir, "127.0.0.1:0", "-name", "relay a")
	listened := srv.out.String()
	entropy = bytes.NewReader(rep(0x19, invite.EntropyBytes))
	stdout, _, code, err := runWithin([]string{"-state-dir", dir, "issue"})
	if err != nil || code != exitOK {
		t.Fatal(err)
	}
	join := strings.TrimSpace(stdout)
	client := &http.Client{Timeout: 2 * time.Second}
	hello, err := client.Get("http://" + srv.addr + "/v1/hello")
	if err != nil {
		t.Fatal(err)
	}
	hello.Body.Close()
	status, body := postRegister(t, client, "http://"+srv.addr, join, inst(0x21), "seat a")
	if status != http.StatusCreated {
		t.Fatalf("register %d %s", status, body)
	}
	var created registerResp
	if err := decodeCreated(body, &created); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+srv.addr+"/v1/seats", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	seatBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(seatBody), "seat a") {
		t.Fatalf("seats %d %v %s", resp.StatusCode, err, seatBody)
	}
	if srv.out.String() != listened || srv.errb.Len() != 0 {
		t.Fatalf("server grew\nstdout %q\nstderr %q", srv.out.String(), srv.errb.String())
	}
	norm := strings.ReplaceAll(join, "-", "")
	mustNotContain(t, "server", srv.out.String()+srv.errb.String(), join, norm, created.Token, "seat a")
	srv.wait(t)
}

func TestOpenOfAFileIsIO(t *testing.T) {
	restoreSeams(t)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, code, err := runCmd("-state-dir", file, "status")
	if code != exitIO || err == nil {
		t.Fatalf("file state %d %v", code, err)
	}
}

type issuedCode struct {
	code    string
	id      string
	issued  time.Time
	expires time.Time
}

func issueThree(t *testing.T, dir string) (a, b, c issuedCode) {
	t.Helper()
	at := fixedNow()
	clk := func() time.Time {
		cur := at
		at = at.Add(time.Minute)
		return cur
	}
	reg := openRegistry(t, dir, registry.Options{
		Name:    "relay a",
		Now:     clk,
		Entropy: cat(rep(0x01, 16), rep(0x11, invite.EntropyBytes), rep(0x22, invite.EntropyBytes), rep(0x33, invite.EntropyBytes), rep(0x44, 32)),
	})
	var err error
	var row registry.Code
	if a.code, row, err = reg.Issue(); err != nil {
		t.Fatal(err)
	}
	a.id, a.issued, a.expires = row.ID, row.Issued, row.Expires
	if b.code, row, err = reg.Issue(); err != nil {
		t.Fatal(err)
	}
	b.id, b.issued, b.expires = row.ID, row.Issued, row.Expires
	if c.code, row, err = reg.Issue(); err != nil {
		t.Fatal(err)
	}
	c.id, c.issued, c.expires = row.ID, row.Issued, row.Expires
	if _, _, err = reg.Register(b.code, inst(0x11), "north seat"); err != nil {
		t.Fatal(err)
	}
	if _, err = reg.Revoke(c.id); err != nil {
		t.Fatal(err)
	}
	return a, b, c
}

func humanRow(row issuedCode, state, seat, name string) string {
	return fmt.Sprintf("%s %s %s %s %s %s\n", row.id, state, ts(row.issued), ts(row.expires), seat, name)
}

func jsonRow(row issuedCode, state, seat, name string) string {
	s := `{"id":"` + row.id + `","state":"` + state + `","issued":"` + ts(row.issued) + `","expires":"` + ts(row.expires) + `"`
	if seat != "" {
		s += `,"seat":"` + seat + `"`
	}
	if name != "" {
		s += `,"name":"` + name + `"`
	}
	return s + "}"
}

func codeIDAt(t *testing.T, dir string, index int) string {
	t.Helper()
	reg := openRegistry(t, dir, registry.Options{Now: fixedNow, Entropy: bytes.NewReader(nil)})
	rows := reg.List()
	if index >= len(rows) {
		t.Fatalf("index %d of %d", index, len(rows))
	}
	return rows[index].ID
}

type running struct {
	cancel context.CancelFunc
	out    *signalWriter
	errb   *bytes.Buffer
	errc   chan error
	addr   string
	once   sync.Once
	exited bool
	err    error
}

func startServer(t *testing.T, dir, addr string, extra ...string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := newSignalWriter("listening on http://")
	errb := &bytes.Buffer{}
	errc := make(chan error, 1)
	args := append([]string{"-addr", addr, "-state-dir", dir}, extra...)
	r := &running{cancel: cancel, out: out, errb: errb, errc: errc}
	t.Cleanup(r.finish)
	go func() {
		errc <- run(ctx, args, "dev", out, errb)
	}()
	select {
	case <-out.seen:
		r.addr = boundAddr(t, out.String())
	case err := <-errc:
		r.exited = true
		r.err = err
		t.Fatalf("serve ended: %v stderr %s", err, errb.String())
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not listen")
	}
	return r
}

func (r *running) finish() {
	r.once.Do(func() {
		if r.exited {
			return
		}
		r.cancel()
		select {
		case r.err = <-r.errc:
		case <-time.After(5 * time.Second):
			r.err = errors.New("server did not stop")
		}
	})
}

func (r *running) wait(t *testing.T) {
	t.Helper()
	r.finish()
	if r.err != nil {
		t.Fatal(r.err)
	}
}

func boundAddr(t *testing.T, out string) string {
	t.Helper()
	const prefix = "scimux-team listening on http://"
	line := strings.TrimSpace(out)
	if !strings.HasPrefix(line, prefix) || strings.Contains(line, "\n") {
		t.Fatalf("listen line %q", out)
	}
	return strings.TrimPrefix(line, prefix)
}

type signalWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	needle string
	seen   chan struct{}
	once   sync.Once
}

func newSignalWriter(needle string) *signalWriter {
	return &signalWriter{needle: needle, seen: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.needle) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

type captureFail struct {
	buf bytes.Buffer
}

func (w *captureFail) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return 0, errors.New("write failed")
}

func restoreSeams(t *testing.T) {
	t.Helper()
	e, n, h := entropy, nowFn, userHomeDir
	s, sd, f := serveFn, shutdownFn, flockFn
	c, m, a := chmodFile, marshalJSON, afterOpen
	t.Cleanup(func() {
		entropy, nowFn, userHomeDir = e, n, h
		serveFn, shutdownFn, flockFn = s, sd, f
		chmodFile, marshalJSON, afterOpen = c, m, a
	})
	entropy = bytes.NewReader(nil)
	nowFn = fixedNow
	userHomeDir = h
	serveFn = s
	shutdownFn = sd
	flockFn = f
	chmodFile = c
	marshalJSON = m
	afterOpen = nil
}

func runCmd(args ...string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	err := Run(args, "dev", &stdout, &stderr)
	return stdout.String(), stderr.String(), ExitCode(err), err
}

func runWithin(args []string) (string, string, int, error) {
	type result struct {
		stdout, stderr string
		code           int
		err            error
	}
	ch := make(chan result, 1)
	go func() {
		stdout, stderr, code, err := runCmd(args...)
		ch <- result{stdout, stderr, code, err}
	}()
	select {
	case r := <-ch:
		return r.stdout, r.stderr, r.code, r.err
	case <-time.After(5 * time.Second):
		return "", "", exitInternal, errors.New("command blocked")
	}
}

func postRegister(t *testing.T, client *http.Client, base, code, id, name string) (int, string) {
	t.Helper()
	resp, err := client.Post(base+"/v1/register", "application/json", strings.NewReader(jsonBody(code, id, name)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func decodeCreated(body string, dst *registerResp) error {
	return json.Unmarshal([]byte(body), dst)
}

func mustCode(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	return codeFrom(t, raw), idFrom(t, raw)
}

func codeFrom(t *testing.T, raw []byte) string {
	t.Helper()
	norm, err := invite.Generate(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return invite.Format(norm)
}

func idFrom(t *testing.T, raw []byte) string {
	t.Helper()
	norm, err := invite.Generate(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return invite.CodeID(invite.Verifier(norm))
}

func ts(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func rep(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func cat(parts ...[]byte) *bytes.Reader {
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	return bytes.NewReader(all)
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func mustNotContain(t *testing.T, label, haystack string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(haystack, secret) {
			t.Fatalf("%s contains shared text", label)
		}
	}
}
