// Command covercheck is the coverage gate of this repository.
//
// It enforces two rules on the non-test Go files of the listed packages:
//
//  1. Statements: every statement runs in the package's own tests.
//  2. Branches: every `if` condition is seen both true and false in the
//     package's own tests.
//
// Go's own cover tool measures statements only. For the branch rule this
// tool copies the module to a temporary directory, wraps every `if`
// condition in a recording call, and runs the tests there. The working tree
// is never modified.
//
// A deviation needs a motivation in the source:
//
//	// coverage-exempt: <reason>
//
// The comment exempts an `if` when it sits on the line of the `if` or on the
// line above it. It exempts unreached statements when it sits inside their
// block or on the line above the block. An exemption without a reason, and
// an exemption that exempts nothing, both fail the gate. Every exemption in
// use is printed, so a reviewer sees each one with its reason.
//
// Usage, from the module root:
//
//	go run ./tools/covercheck            # ./cmd/... and ./internal/..., where present
//	go run ./tools/covercheck ./internal/journal
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const marker = "coverage-exempt:"

func main() {
	os.Exit(run(os.Args[1:], ".", os.Stdout, os.Stderr))
}

type goPackage struct {
	Dir        string
	ImportPath string
	Name       string
	GoFiles    []string
}

type exemption struct {
	file   string // module-relative, slash-separated
	line   int
	reason string
	used   bool
}

type ifSite struct {
	file string
	line int
	ex   *exemption
}

func run(args []string, dir string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// A product directory that does not exist yet is skipped, so the
		// gate works from the first package onwards.
		for _, name := range []string{"cmd", "internal"} {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.IsDir() {
				args = append(args, "./"+name+"/...")
			}
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "covercheck: no cmd or internal directory yet")
		return 2
	}
	problems, exemptions, err := check(args, dir, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "covercheck:", err)
		return 2
	}
	for _, ex := range exemptions {
		if ex.used {
			fmt.Fprintf(stdout, "exempt %s:%d: %s\n", ex.file, ex.line, ex.reason)
		}
	}
	for _, p := range problems {
		fmt.Fprintln(stdout, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stdout, "covercheck: FAIL, %d problem(s)\n", len(problems))
		return 1
	}
	fmt.Fprintln(stdout, "covercheck: ok, statements and branches fully covered")
	return 0
}

func check(patterns []string, dir string, stderr io.Writer) ([]string, []*exemption, error) {
	root, err := goOutput(dir, "list", "-m", "-f", "{{.Dir}}")
	if err != nil {
		return nil, nil, err
	}
	root = strings.TrimSpace(root)
	pkgs, err := listPackages(dir, patterns)
	if err != nil {
		return nil, nil, err
	}
	if len(pkgs) == 0 {
		return nil, nil, errors.New("no packages match")
	}

	tmp, err := os.MkdirTemp("", "covercheck-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	shadow := filepath.Join(tmp, "module")
	if err := copyTree(root, shadow); err != nil {
		return nil, nil, err
	}

	var exemptions []*exemption
	var sites []ifSite
	var importPaths []string
	for _, pkg := range pkgs {
		importPaths = append(importPaths, pkg.ImportPath)
		relDir, err := filepath.Rel(root, pkg.Dir)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range pkg.GoFiles {
			rel := filepath.ToSlash(filepath.Join(relDir, name))
			exs, err := instrument(filepath.Join(shadow, relDir, name), rel, &sites)
			if err != nil {
				return nil, nil, err
			}
			exemptions = append(exemptions, exs...)
		}
		helper := fmt.Sprintf(helperSource, pkg.Name)
		if err := os.WriteFile(filepath.Join(shadow, relDir, "zz_covercheck_gen.go"), []byte(helper), 0o600); err != nil {
			return nil, nil, err
		}
	}

	var problems []string

	// Branch rule: run the tests on the instrumented copy.
	hits := filepath.Join(tmp, "hits")
	cmd := exec.Command("go", append([]string{"test", "-count=1"}, importPaths...)...)
	cmd.Dir = shadow
	cmd.Env = append(os.Environ(), "COVERCHECK_OUT="+hits)
	if out, err := cmd.CombinedOutput(); err != nil {
		stderr.Write(out)
		return nil, nil, errors.New("tests fail on the instrumented copy")
	}
	seen, err := readHits(hits)
	if err != nil {
		return nil, nil, err
	}
	for id, site := range sites {
		for _, outcome := range []bool{true, false} {
			if seen[[2]int{id, boolInt(outcome)}] {
				continue
			}
			if site.ex != nil {
				site.ex.used = true
				continue
			}
			problems = append(problems, fmt.Sprintf("%s:%d: if condition is never %v", site.file, site.line, outcome))
		}
	}

	// Statement rule: Go's own cover tool on the working tree.
	profile := filepath.Join(tmp, "cover.out")
	cmd = exec.Command("go", append([]string{"test", "-count=1", "-coverprofile=" + profile}, importPaths...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		stderr.Write(out)
		return nil, nil, errors.New("tests fail")
	}
	modPath, err := goOutput(dir, "list", "-m", "-f", "{{.Path}}")
	if err != nil {
		return nil, nil, err
	}
	blocks, err := readProfile(profile, strings.TrimSpace(modPath))
	if err != nil {
		return nil, nil, err
	}
	for _, b := range blocks {
		if b.count > 0 {
			continue
		}
		exempt := false
		for _, ex := range exemptions {
			if ex.file == b.file && ex.line >= b.startLine-1 && ex.line <= b.endLine {
				ex.used = true
				exempt = true
			}
		}
		if !exempt {
			problems = append(problems, fmt.Sprintf("%s:%d: statements never run (to line %d)", b.file, b.startLine, b.endLine))
		}
	}

	for _, ex := range exemptions {
		if ex.reason == "" {
			problems = append(problems, fmt.Sprintf("%s:%d: exemption has no reason", ex.file, ex.line))
		} else if !ex.used {
			problems = append(problems, fmt.Sprintf("%s:%d: exemption exempts nothing", ex.file, ex.line))
		}
	}
	sort.Strings(problems)
	return problems, exemptions, nil
}

func goOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func listPackages(dir string, patterns []string) ([]goPackage, error) {
	out, err := goOutput(dir, append([]string{"list", "-json"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	var pkgs []goPackage
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var pkg goPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

// copyTree copies the module without its history and without untracked
// scratch, neither of which a test may depend on.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == ".git" || rel == "attic" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, info.Mode().Perm())
	})
}

// instrument wraps every `if` condition of one file in the shadow copy and
// returns the exemption comments of that file.
func instrument(path, rel string, sites *[]ifSite) ([]*exemption, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var exemptions []*exemption
	byLine := map[int]*exemption{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			i := strings.Index(c.Text, marker)
			if i < 0 {
				continue
			}
			reason := strings.TrimSpace(strings.TrimSuffix(c.Text[i+len(marker):], "*/"))
			ex := &exemption{file: rel, line: fset.Position(c.Pos()).Line, reason: reason}
			exemptions = append(exemptions, ex)
			byLine[ex.line] = ex
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		line := fset.Position(stmt.If).Line
		ex := byLine[line]
		if ex == nil {
			ex = byLine[line-1]
		}
		id := len(*sites)
		*sites = append(*sites, ifSite{file: rel, line: line, ex: ex})
		stmt.Cond = &ast.CallExpr{
			Fun: ast.NewIdent("covercheckHit"),
			Args: []ast.Expr{
				&ast.BasicLit{Kind: token.INT, Value: strconv.Itoa(id)},
				&ast.CallExpr{Fun: ast.NewIdent("bool"), Args: []ast.Expr{stmt.Cond}},
			},
		}
		return true
	})
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, err
	}
	return exemptions, os.WriteFile(path, buf.Bytes(), 0o600)
}

const helperSource = `// Code generated by covercheck. DO NOT EDIT.

package %s

import (
	"os"
	"strconv"
	"sync"
)

var (
	covercheckMu   sync.Mutex
	covercheckSeen = map[[2]int]bool{}
)

func covercheckHit(id int, v bool) bool {
	outcome := 0
	if v {
		outcome = 1
	}
	key := [2]int{id, outcome}
	covercheckMu.Lock()
	defer covercheckMu.Unlock()
	if covercheckSeen[key] {
		return v
	}
	covercheckSeen[key] = true
	f, err := os.OpenFile(os.Getenv("COVERCHECK_OUT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic("covercheck: " + err.Error())
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(id) + " " + strconv.Itoa(outcome) + "\n"); err != nil {
		panic("covercheck: " + err.Error())
	}
	return v
}
`

func readHits(path string) (map[[2]int]bool, error) {
	seen := map[[2]int]bool{}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var id, outcome int
		if _, err := fmt.Sscanf(sc.Text(), "%d %d", &id, &outcome); err != nil {
			return nil, fmt.Errorf("hits file: %v", err)
		}
		seen[[2]int{id, outcome}] = true
	}
	return seen, sc.Err()
}

type block struct {
	file               string
	startLine, endLine int
	count              int
}

// readProfile reads a Go cover profile. A block listed more than once keeps
// its highest count.
func readProfile(path, modPath string) ([]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	index := map[string]int{}
	var blocks []block
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// name.go:startLine.startCol,endLine.endCol numStmt count
		colon := strings.LastIndex(line, ":")
		fields := strings.Fields(line[colon+1:])
		if colon < 0 || len(fields) != 3 {
			return nil, fmt.Errorf("cover profile: bad line %q", line)
		}
		var b block
		var c1, c2 int
		if _, err := fmt.Sscanf(fields[0], "%d.%d,%d.%d", &b.startLine, &c1, &b.endLine, &c2); err != nil {
			return nil, fmt.Errorf("cover profile: bad line %q", line)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("cover profile: bad line %q", line)
		}
		b.count = count
		b.file = strings.TrimPrefix(strings.TrimPrefix(line[:colon], modPath), "/")
		key := b.file + ":" + fields[0]
		if i, ok := index[key]; ok {
			if count > blocks[i].count {
				blocks[i].count = count
			}
			continue
		}
		index[key] = len(blocks)
		blocks = append(blocks, b)
	}
	return blocks, sc.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
