package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzJournal checks that arbitrary file contents never panic and that
// every offset passed to apply is the start of that line.
func FuzzJournal(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("a\n"))
	f.Add([]byte("a\nb"))
	f.Add([]byte("\n"))
	f.Add([]byte("a\n\nb\n"))
	f.Add([]byte("no newline"))
	f.Add([]byte{0x00, '\n', 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "j")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		jf, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer jf.Close()

		type gotLine struct {
			off  int64
			line []byte
		}
		var got []gotLine
		err = jf.Refresh(func(off int64, line []byte) error {
			if bytes.IndexByte(line, '\n') >= 0 {
				t.Fatalf("newline in line at %d", off)
			}
			got = append(got, gotLine{off: off, line: append([]byte(nil), line...)})
			return nil
		})
		if err != nil && !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("refresh: %v", err)
		}
		var rebuilt []byte
		for _, g := range got {
			if int(g.off) != len(rebuilt) || int(g.off) > len(data) {
				t.Fatalf("offset %d, reconstructed %d, file %d", g.off, len(rebuilt), len(data))
			}
			end := int(g.off) + len(g.line)
			if end >= len(data) || data[end] != '\n' || !bytes.Equal(data[g.off:end], g.line) {
				t.Fatalf("offset %d is not a line start", g.off)
			}
			line, err := jf.ReadLine(g.off)
			if err != nil || !bytes.Equal(line, g.line) {
				t.Fatalf("ReadLine(%d) = %q, %v", g.off, line, err)
			}
			rebuilt = append(rebuilt, g.line...)
			rebuilt = append(rebuilt, '\n')
		}
		if err == nil {
			if !bytes.HasPrefix(data, rebuilt) {
				t.Fatal("replay dropped a prefix")
			}
			rest := data[len(rebuilt):]
			if bytes.IndexByte(rest, '\n') >= 0 {
				t.Fatal("replay skipped a complete line")
			}
			if len(rest) > MaxLineBytes {
				t.Fatal("replay accepted an oversized fragment")
			}
		}
	})
}
