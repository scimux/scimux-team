package invite

import "testing"

// FuzzParse checks that parsing never panics and that an accepted code
// is already in canonical form.
func FuzzParse(f *testing.F) {
	f.Add("0410-6105-0R3G-G28A-1C60-T3GF")
	f.Add("041061050r3gg28a1c60t3gf")
	f.Add("o4i0-6l05 0r3g-g28a ic60-t3gf")
	f.Add("")
	f.Add("!!!")
	f.Add("000000000000000000000000")
	f.Add(string([]byte{0x00, 0xff, 0xfe}))

	f.Fuzz(func(t *testing.T, s string) {
		n, err := Parse(s)
		if err != nil {
			return
		}
		if len(n) != CodeLength {
			t.Fatalf("accepted %q normalised to len %d", s, len(n))
		}
		for _, r := range n {
			if !containsRune(Alphabet, r) {
				t.Fatalf("accepted %q normalised to %q, contains %q", s, n, r)
			}
		}
		n2, err := Parse(n)
		if err != nil || n2 != n {
			t.Fatalf("Parse not idempotent: %q then %q (%v)", n, n2, err)
		}
	})
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
