package invite

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"
)

const (
	wantAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// specCode is the encoding of bytes 1..15, computed independently of encode.
	specCode = "041061050R3GG28A1C60T3GF"
)

func TestGenerateLengthAlphabetAndFailure(t *testing.T) {
	if Alphabet != wantAlphabet || len(Alphabet) != 32 {
		t.Fatalf("Alphabet = %q", Alphabet)
	}
	seen := map[rune]bool{}
	for _, r := range Alphabet {
		if seen[r] {
			t.Fatalf("duplicate %q", r)
		}
		seen[r] = true
		if strings.ContainsRune("ILOUilou", r) || unicode.IsLower(r) {
			t.Fatalf("alphabet contains %q", r)
		}
	}

	raw := make([]byte, EntropyBytes)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	got, err := Generate(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got != specCode || len(got) != CodeLength {
		t.Fatalf("Generate = %q, want %s", got, specCode)
	}
	again, err := Generate(bytes.NewReader(append([]byte(nil), raw...)))
	if err != nil || again != got {
		t.Fatalf("Generate not deterministic: %q %q %v", got, again, err)
	}

	if _, err := Generate(bytes.NewReader(raw[:3])); err == nil {
		t.Fatal("short entropy succeeded")
	}
	if _, err := Generate(&failReader{err: errors.New("no entropy")}); err == nil {
		t.Fatal("failed entropy succeeded")
	}

	// nil uses the process reader. The assertion is the shape, not the value.
	live, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != CodeLength {
		t.Fatalf("live length %d", len(live))
	}
	for _, r := range live {
		if !strings.ContainsRune(Alphabet, r) {
			t.Fatalf("live code %q contains %q", live, r)
		}
	}
}

func TestFormatGroupsByFour(t *testing.T) {
	got := Format(specCode)
	want := "0410-6105-0R3G-G28A-1C60-T3GF"
	if got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	if strings.Count(got, "-") != CodeLength/GroupSize-1 {
		t.Fatalf("hyphens in %q", got)
	}
	if Format(specCode) != got {
		t.Fatal("Format is not stable")
	}
	for _, odd := range []string{"", "abc", specCode + "A", specCode[:CodeLength-1]} {
		if Format(odd) != odd {
			t.Fatalf("Format(%q) changed a wrong-length input", odd)
		}
	}
}

func TestParseAcceptsNormalFormsAndRejectsTheRest(t *testing.T) {
	const canon = specCode
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"canonical", canon, canon},
		{"lowercase", strings.ToLower(canon), canon},
		{"mixed", "0410" + strings.ToLower(canon[4:]), canon},
		{"hyphens", "0410-6105-0R3G-G28A-1C60-T3GF", canon},
		{"spaces", "0410 6105 0R3G G28A 1C60 T3GF", canon},
		{"hyphens and spaces", "0410-6105 0R3G-G28A 1C60-T3GF", canon},
		{"O for 0", "O41061050R3GG28A1C60T3GF", canon},
		{"o for 0", "o41061050R3GG28A1C60T3GF", canon},
		{"I for 1", "04I061050R3GG28A1C60T3GF", canon},
		{"i for 1", "04i061050R3GG28A1C60T3GF", canon},
		{"L for 1", "04L061050R3GG28A1C60T3GF", canon},
		{"l for 1", "04l061050R3GG28A1C60T3GF", canon},
		{"all transforms", "o4i0-6l05 0r3g-g28a ic60-t3gf", canon},
		{"other code", "000000000000000000000000", "000000000000000000000000"},

		{"empty", "", ""},
		{"too short", canon[:CodeLength-1], ""},
		{"too long", canon + "A", ""},
		{"foreign U", "041061050R3GG28A1C60T3GU", ""},
		{"symbol", "041061050R3GG28A1C60T3G@", ""},
		{"underscore", "0410_6105_0R3G_G28A_1C60_T3GF", ""},
		{"unicode", canon[:23] + "é", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.input)
			if tc.want == "" {
				if !errors.Is(err, ErrFormat) {
					t.Fatalf("Parse(%q) = %q, %v; want ErrFormat", tc.input, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Parse(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestParseOfFormatOfGenerateIsIdentity(t *testing.T) {
	raw := make([]byte, EntropyBytes)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	norm, err := Generate(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if norm != specCode {
		t.Fatalf("Generate = %q, want %s", norm, specCode)
	}
	back, err := Parse(Format(norm))
	if err != nil || back != norm {
		t.Fatalf("Parse(Format(Generate)) = %q, %v; want %q", back, err, norm)
	}
}

func TestVerifierAndCodeIDUseTeamDomains(t *testing.T) {
	const norm = specCode
	const other = "000000000000000000000000"

	gotV := Verifier(norm)
	wantV := specVerifier(norm, "scimux-team/join-verifier/v1")
	if !bytes.Equal(gotV, wantV) {
		t.Fatalf("Verifier = %x, want %x", gotV, wantV)
	}
	if bytes.Equal(gotV, specVerifier(norm, "scimux-rv/invite-verifier/v1")) {
		t.Fatal("verifier uses the scimux-rv domain")
	}
	if bytes.Equal(Verifier(norm), Verifier(other)) {
		t.Fatal("distinct codes share a verifier")
	}
	again := Verifier(norm)
	if !bytes.Equal(gotV, again) {
		t.Fatal("verifier is not stable")
	}

	gotID := CodeID(gotV)
	wantID := specCodeID(gotV, "scimux-team/join-id/v1")
	if gotID != wantID || len(gotID) != CodeIDLen {
		t.Fatalf("CodeID = %q, want %q", gotID, wantID)
	}
	if gotID == specCodeID(gotV, "scimux-rv/code-id/v1") {
		t.Fatal("code id uses the scimux-rv domain")
	}
	if CodeID(Verifier(other)) == gotID {
		t.Fatal("distinct verifiers share a code id")
	}
	if CodeID(gotV) != gotID {
		t.Fatal("code id is not stable")
	}
	if strings.Contains(strings.ToLower(gotID), strings.ToLower(norm)) {
		t.Fatal("code id contains the plaintext")
	}
}

func TestEncodeKeepsAPartialQuantum(t *testing.T) {
	// One byte is eight bits: one character and three leftover bits.
	// 0xff → Z (top five bits) then W (the rest, shifted up).
	if got := encode([]byte{0xff}); got != "ZW" {
		t.Fatalf("encode(0xff) = %q, want ZW", got)
	}
	raw := make([]byte, EntropyBytes)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	if got := encode(raw); got != specCode {
		t.Fatalf("encode(1..15) = %q, want %s", got, specCode)
	}
}

func TestDeclaredBounds(t *testing.T) {
	if EntropyBytes != 15 || CodeLength != 24 || GroupSize != 4 || CodeIDLen != 32 {
		t.Fatalf("sizes entropy=%d code=%d group=%d id=%d", EntropyBytes, CodeLength, GroupSize, CodeIDLen)
	}
	if CollisionRetries != 8 {
		t.Fatalf("CollisionRetries = %d", CollisionRetries)
	}
	if UnusedTTL != 30*24*time.Hour {
		t.Fatalf("UnusedTTL = %s", UnusedTTL)
	}
}

func specVerifier(normalized, domain string) []byte {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(normalized))
	return h.Sum(nil)
}

func specCodeID(verifier []byte, domain string) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write(verifier)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

type failReader struct{ err error }

func (r failReader) Read([]byte) (int, error) { return 0, r.err }
