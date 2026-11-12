package team

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scimux/scimux-team/internal/registry"
)

const (
	exitOK       = 0
	exitInternal = 1
	exitUsage    = 2
	exitUnknown  = 3
	exitIO       = 4
)

// Seams tests replace. Production uses the real clock, the real home
// directory and crypto/rand via a nil entropy reader.
var (
	userHomeDir = os.UserHomeDir
	nowFn       = time.Now
	entropy     io.Reader
	serveFn     = func(s *http.Server, ln net.Listener) error { return s.Serve(ln) }
	shutdownFn  = func(s *http.Server, ctx context.Context) error { return s.Shutdown(ctx) }
	flockFn     = syscall.Flock
	chmodFile   = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
)

// afterOpen, when set, runs after an operator command opens the registry
// and before that command reads or writes it. Production leaves it nil.
// A test uses it to append a journal line in that window.
var afterOpen func(*registry.Registry)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// ExitCode reports the process status for err. nil is 0. An error that
// is not an exitError is internal, so a forgotten wrap cannot look like success.
func ExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return exitInternal
}

func usageErr(err error) error {
	return &exitError{code: exitUsage, err: err}
}

func ioErr(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitIO, err: err}
}

// Run parses args and either serves or runs one operator command.
func Run(args []string, version string, stdout, stderr io.Writer) error {
	return run(context.Background(), args, version, stdout, stderr)
}

func run(parent context.Context, args []string, version string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("scimux-team", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	stateFlag := fs.String("state-dir", "", "state directory (default $HOME/.scimux-team)")
	name := fs.String("name", "", "server name used on the first start")
	baseURL := fs.String("url", "", "base URL printed as a join link by issue")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return ioErr(err)
	}
	if *baseURL != "" {
		if err := validateBaseURL(*baseURL); err != nil {
			return err
		}
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return serve(parent, *addr, *stateFlag, *name, stdout, stderr)
	}
	return admin(rest, *stateFlag, *name, *baseURL, *jsonOut, stdout, stderr)
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !allowedBase(u) {
		return usageErr(errors.New("-url must be an http or https URL with a host and no path, query or fragment"))
	}
	return nil
}

func allowedBase(u *url.URL) bool {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return true
}

func admin(args []string, stateFlag, name, baseURL string, jsonOut bool, stdout, stderr io.Writer) error {
	cmd := args[0]
	filter := ""
	switch cmd {
	case "issue":
		if len(args) != 1 {
			return usageErr(errors.New("issue takes no arguments"))
		}
	case "list":
		if len(args) == 2 {
			switch args[1] {
			case "--unused":
				filter = "unused"
			case "--revoked":
				filter = "revoked"
			default:
				return usageErr(errors.New("list accepts only --unused or --revoked"))
			}
		} else if len(args) != 1 {
			return usageErr(errors.New("list accepts at most one filter"))
		}
	case "revoke":
		if len(args) != 2 {
			return usageErr(errors.New("revoke requires an id or installation id"))
		}
	case "status":
		if len(args) != 1 {
			return usageErr(errors.New("status takes no arguments"))
		}
	default:
		return usageErr(fmt.Errorf("unknown command %q", cmd))
	}
	dir, err := resolveState(stateFlag)
	if err != nil {
		return err
	}
	reg, err := openState(dir, name)
	if err != nil {
		return err
	}
	defer reg.Close()
	if afterOpen != nil {
		afterOpen(reg)
	}

	switch cmd {
	case "issue":
		return cmdIssue(reg, baseURL, jsonOut, stdout, stderr)
	case "list":
		return cmdList(reg, filter, jsonOut, stdout)
	case "revoke":
		return cmdRevoke(reg, args[1], jsonOut, stdout)
	default:
		return cmdStatus(reg, jsonOut, stdout)
	}
}

func cmdIssue(reg *registry.Registry, baseURL string, jsonOut bool, stdout, stderr io.Writer) error {
	code, row, err := reg.Issue()
	if err != nil {
		if err.Error() == "registry: invite collision" {
			return &exitError{code: exitInternal, err: err}
		}
		return ioErr(err)
	}
	link := ""
	if baseURL != "" {
		link = baseURL + "/join#" + code
	}
	if jsonOut {
		obj := map[string]string{
			"code":    code,
			"expires": row.Expires.UTC().Format(time.RFC3339Nano),
			"id":      row.ID,
		}
		if link != "" {
			obj["link"] = link
		}
		return writeJSONValue(stdout, obj)
	}
	line := code
	if link != "" {
		line = link
	}
	if _, err := fmt.Fprintln(stdout, line); err != nil {
		return ioErr(err)
	}
	if _, err := fmt.Fprintf(stderr, "id: %s\n", row.ID); err != nil {
		return ioErr(err)
	}
	return nil
}

func cmdList(reg *registry.Registry, filter string, jsonOut bool, stdout io.Writer) error {
	rows := reg.List()
	if filter != "" {
		kept := rows[:0]
		for _, row := range rows {
			if row.State == filter {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	if jsonOut {
		out := make([]codeJSON, 0, len(rows))
		for _, row := range rows {
			out = append(out, codeJSON{
				ID:      row.ID,
				State:   row.State,
				Issued:  row.Issued.UTC().Format(time.RFC3339Nano),
				Expires: row.Expires.UTC().Format(time.RFC3339Nano),
				Seat:    row.Seat,
				Name:    row.Name,
			})
		}
		return writeJSONValue(stdout, out)
	}
	for _, row := range rows {
		seat, name := row.Seat, row.Name
		if seat == "" {
			seat = "-"
		}
		if name == "" {
			name = "-"
		}
		if _, err := fmt.Fprintf(stdout, "%s %s %s %s %s %s\n", row.ID, row.State, row.Issued.UTC().Format(time.RFC3339Nano), row.Expires.UTC().Format(time.RFC3339Nano), seat, name); err != nil {
			return ioErr(err)
		}
	}
	return nil
}

type codeJSON struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Issued  string `json:"issued"`
	Expires string `json:"expires"`
	Seat    string `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
}

type statusJSON struct {
	Codes    int    `json:"codes"`
	Unused   int    `json:"unused"`
	Redeemed int    `json:"redeemed"`
	Revoked  int    `json:"revoked"`
	Expired  int    `json:"expired"`
	Log      string `json:"log"`
}

func cmdRevoke(reg *registry.Registry, key string, jsonOut bool, stdout io.Writer) error {
	row, err := reg.Revoke(key)
	if err != nil {
		if errors.Is(err, registry.ErrUnknown) {
			return &exitError{code: exitUnknown, err: err}
		}
		return ioErr(err)
	}
	if jsonOut {
		return writeJSONValue(stdout, map[string]string{
			"id":    row.ID,
			"note":  revokeNote,
			"state": row.State,
		})
	}
	if _, err := fmt.Fprintf(stdout, "revoked %s\n%s\n", row.ID, revokeNote); err != nil {
		return ioErr(err)
	}
	return nil
}

func cmdStatus(reg *registry.Registry, jsonOut bool, stdout io.Writer) error {
	st := reg.Status()
	out := statusJSON{
		Codes:    st.Codes,
		Unused:   st.Unused,
		Redeemed: st.Redeemed,
		Revoked:  st.Revoked,
		Expired:  st.Expired,
		Log:      reg.Path(),
	}
	if jsonOut {
		return writeJSONValue(stdout, out)
	}
	_, err := fmt.Fprintf(stdout, "codes: %d\nunused: %d\nredeemed: %d\nrevoked: %d\nexpired: %d\nlog: %s\n",
		out.Codes, out.Unused, out.Redeemed, out.Revoked, out.Expired, out.Log)
	return ioErr(err)
}

func writeJSONValue(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return ioErr(enc.Encode(v))
}

func serve(parent context.Context, addr, stateFlag, name string, stdout, stderr io.Writer) error {
	dir, err := resolveState(stateFlag)
	if err != nil {
		return err
	}
	if needsPublicWarning(addr) {
		fmt.Fprintln(stderr, publicWarning)
	}
	reg, err := openState(dir, name)
	if err != nil {
		return err
	}
	defer reg.Close()
	lock, err := lockServer(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := newHTTPServer(newHandler(reg))
	if _, err := fmt.Fprintf(stdout, "scimux-team listening on http://%s\n", ln.Addr()); err != nil {
		ln.Close()
		return ioErr(err)
	}
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Copy the seams before the goroutine starts. A test may replace the
	// package variables after this server returns, and Shutdown does not
	// wait for a goroutine that has not entered Serve yet.
	serve := serveFn
	shutDown := shutdownFn
	errc := make(chan error, 1)
	go func() {
		err := serve(srv, ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		stop()
	}
	shut, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := shutDown(srv, shut); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func lockServer(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ioErr(err)
	}
	if err := chmodFile(f, 0o600); err != nil {
		f.Close()
		return nil, ioErr(err)
	}
	if err := flockFn(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("server already running for this state directory")
		}
		return nil, err
	}
	return f, nil
}

// openState reports a server name the protocol rejects as a usage error.
// Every other Open failure is an I/O error. Register's ErrInvalid is not
// mapped here.
func openState(dir, name string) (*registry.Registry, error) {
	reg, err := registry.Open(dir, registry.Options{Name: name, Now: nowFn, Entropy: entropy})
	if errors.Is(err, registry.ErrInvalid) {
		return nil, usageErr(err)
	}
	if err != nil {
		return nil, ioErr(err)
	}
	return reg, nil
}

func resolveState(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", ioErr(err)
	}
	if home == "" {
		return "", ioErr(errors.New("empty home directory"))
	}
	return filepath.Join(home, ".scimux-team"), nil
}

func needsPublicWarning(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return host == ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}
