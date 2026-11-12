package team

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
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

var (
	userHomeDir = os.UserHomeDir
	nowFn       = time.Now
	entropy     io.Reader
	serveFn     = func(s *http.Server, ln net.Listener) error { return s.Serve(ln) }
	shutdownFn  = func(s *http.Server, ctx context.Context) error { return s.Shutdown(ctx) }
	flockFn     = syscall.Flock
	chmodFile   = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
)

var afterOpen func(*registry.Registry)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// ExitCode reports the process status for err.
func ExitCode(err error) int { return 0 }

func usageErr(err error) error { return errors.New("stub") }

func ioErr(err error) error { return errors.New("stub") }

// Run parses args and either serves or runs one operator command.
func Run(args []string, version string, stdout, stderr io.Writer) error { return nil }

func run(parent context.Context, args []string, version string, stdout, stderr io.Writer) error {
	return nil
}

func needsPublicWarning(addr string) bool { return false }
