package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.test/m\n\ngo 1.22\n"
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runCheck(t *testing.T, dir string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run([]string{"./..."}, dir, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestFullyCoveredPackagePasses(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": `package p

func Sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}
`,
		"p/p_test.go": `package p

import "testing"

func TestSign(t *testing.T) {
	if Sign(-2) != -1 || Sign(2) != 1 {
		t.Fatal("sign")
	}
}
`,
	})
	code, out := runCheck(t, dir)
	if code != 0 || !strings.Contains(out, "covercheck: ok") {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
}

func TestReportsMissingBranchAndStatement(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": `package p

func Clamp(n int) int {
	if n > 10 {
		n = 10
	}
	return n
}

func Unused() int {
	return 7
}
`,
		"p/p_test.go": `package p

import "testing"

func TestClamp(t *testing.T) {
	if Clamp(20) != 10 {
		t.Fatal("clamp")
	}
}
`,
	})
	code, out := runCheck(t, dir)
	if code != 1 {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
	// Statement coverage of Clamp is complete; only the branch rule sees
	// that the condition was never false.
	for _, want := range []string{
		"p/p.go:4: if condition is never false",
		"p/p.go:11: statements never run",
		"2 problem(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "never true") {
		t.Errorf("unexpected problem:\n%s", out)
	}
}

func TestExemptions(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": `package p

import "os"

func Clamp(n int) int {
	// coverage-exempt: the lower bound cannot be reached from a test
	if n > 10 {
		n = 10
	}
	return n
}

func Exit() {
	// coverage-exempt: ends the process
	os.Exit(3)
}

// coverage-exempt: nothing here needs it
func Covered() int { return 1 }

func Bare(n int) int {
	// coverage-exempt:
	if n > 10 {
		n = 10
	}
	return n
}
`,
		"p/p_test.go": `package p

import "testing"

func TestAll(t *testing.T) {
	if Clamp(20) != 10 || Covered() != 1 || Bare(20) != 10 {
		t.Fatal("values")
	}
}
`,
	})
	code, out := runCheck(t, dir)
	if code != 1 {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"exempt p/p.go:6: the lower bound cannot be reached from a test",
		"exempt p/p.go:14: ends the process",
		"p/p.go:18: exemption exempts nothing",
		"p/p.go:22: exemption has no reason",
		"2 problem(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFailingTestsAreAnError(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": "package p\n\nfunc One() int { return 1 }\n",
		"p/p_test.go": `package p

import "testing"

func TestOne(t *testing.T) { t.Fatal("always") }
`,
	})
	code, out := runCheck(t, dir)
	if code != 2 || !strings.Contains(out, "tests fail") {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
}

func TestWorkingTreeIsNotModified(t *testing.T) {
	src := "package p\n\nfunc Sign(n int) int {\n\tif n < 0 {\n\t\treturn -1\n\t}\n\treturn 1\n}\n"
	dir := writeModule(t, map[string]string{
		"p/p.go":      src,
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestSign(t *testing.T) { Sign(1); Sign(-1) }\n",
	})
	if code, out := runCheck(t, dir); code != 0 {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "p", "p.go"))
	if err != nil || string(got) != src {
		t.Fatalf("source changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "p"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("package directory changed: %v, %d entries", err, len(entries))
	}
}

func TestDefaultPackagesSkipMissingDirectories(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"internal/p/p.go":      "package p\n\nfunc One() int { return 1 }\n",
		"internal/p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) { One() }\n",
		"tools/x/x.go":         "package x\n\nfunc Untested() int { return 1 }\n",
	})
	var out, errOut bytes.Buffer
	// No cmd directory exists, and tools is not part of the default set.
	if code := run(nil, dir, &out, &errOut); code != 0 {
		t.Fatalf("code = %d, output:\n%s%s", code, out.String(), errOut.String())
	}
}
