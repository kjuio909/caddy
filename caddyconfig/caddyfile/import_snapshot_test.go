// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyfile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// memImportReader is an in-memory filesystem used to exercise imports
// deterministically and independently of the working directory.
type memImportReader struct {
	files map[string]string
	reads *int32
	mu    *sync.Mutex
	order *[]string
}

func (m memImportReader) ReadFile(absPath string) ([]byte, error) {
	if m.mu != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.reads != nil {
		atomic.AddInt32(m.reads, 1)
	}
	if m.order != nil {
		*m.order = append(*m.order, "read:"+absPath)
	}
	if content, ok := m.files[absPath]; ok {
		return []byte(content), nil
	}
	return nil, fmt.Errorf("open %s: no such file or directory", absPath)
}

func (m memImportReader) Glob(pattern string) ([]string, error) {
	if m.mu != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.order != nil {
		*m.order = append(*m.order, "glob:"+pattern)
	}
	dir := filepath.Dir(pattern)
	base := filepath.Base(pattern)
	var matches []string
	for name := range m.files {
		if filepath.Dir(name) != dir {
			continue
		}
		if ok, _ := filepath.Match(base, filepath.Base(name)); ok {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// blockTexts flattens parsed blocks into a deterministic textual
// signature over every token in Dispenser/segment order.
func blockTexts(blocks []ServerBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		b.WriteString("keys:")
		b.WriteString(strings.Join(block.GetKeysText(), ","))
		b.WriteString("\n")
		for _, seg := range block.Segments {
			for _, tok := range seg {
				b.WriteString(tok.Text)
				b.WriteString("|")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func parseWith(t *testing.T, reader ImportReader, input string) ([]ServerBlock, error) {
	t.Helper()
	return Parse("/root/Caddyfile", []byte(input), reader)
}

func mustParse(t *testing.T, reader ImportReader, input string) []ServerBlock {
	t.Helper()
	blocks, err := parseWith(t, reader, input)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	return blocks
}

// A single file read more than once within a call is pinned to one
// snapshot: the reader is only consulted once and later references use
// the same contents.
func TestParseSnapshotPinsFileWithinCall(t *testing.T) {
	var reads int32
	reader := memImportReader{
		files: map[string]string{"/root/s.caddy": "one.example {\n\trespond one\n}\n"},
		reads: &reads,
	}
	blocks := mustParse(t, reader, "import s.caddy\nimport s.caddy\n")
	if got, want := atomic.LoadInt32(&reads), int32(1); got != want {
		t.Fatalf("expected %d underlying read, got %d", want, got)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected two blocks from two expansions, got %d", len(blocks))
	}
}

// A reader whose contents change between calls does not retroactively
// affect the first result and the second call sees the new input.
func TestParseSnapshotIsolatedAcrossCalls(t *testing.T) {
	r := memImportReader{files: map[string]string{"/root/s.caddy": "hostA\n"}}
	first := mustParse(t, r, "import s.caddy\n")
	if got := first[0].GetKeysText()[0]; got != "hostA" {
		t.Fatalf("first call expected hostA, got %q", got)
	}

	r.files["/root/s.caddy"] = "hostB\n"
	second := mustParse(t, r, "import s.caddy\n")
	if got := second[0].GetKeysText()[0]; got != "hostB" {
		t.Fatalf("second call expected hostB, got %q", got)
	}
	if got := first[0].GetKeysText()[0]; got != "hostA" {
		t.Fatalf("first result was retroactively mutated to %q", got)
	}
}

// Parallel calls against readers returning different content never share
// state.
func TestParseConcurrentIsolation(t *testing.T) {
	const n = 16
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			r := memImportReader{files: map[string]string{
				"/root/s.caddy": fmt.Sprintf("host%d\nrespond %d\n", i, i),
			}}
			blocks, err := Parse("/root/Caddyfile", []byte("import s.caddy\n"), r)
			errs[i] = err
			if err == nil {
				results[i] = blockTexts(blocks)
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("call %d failed: %v", i, errs[i])
		}
		seen[results[i]]++
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct results, got %d", n, len(seen))
	}
}

// After a failure, parsing the same top-level filename against a
// different tree must use only the new input.
func TestParseFailureDoesNotPoisonNextCall(t *testing.T) {
	_, err := parseWith(t, memImportReader{files: map[string]string{}}, "import missing.caddy\n")
	if err == nil {
		t.Fatal("expected first call to fail")
	}
	blocks := mustParse(t, memImportReader{files: map[string]string{
		"/root/missing.caddy": "newhost\n",
	}}, "import missing.caddy\n")
	if got := blocks[0].GetKeysText()[0]; got != "newhost" {
		t.Fatalf("expected second call to use new tree, got %q", got)
	}
}

// A failed call never returns a half-populated result.
func TestParseFailureReturnsNoBlocks(t *testing.T) {
	blocks, err := parseWith(t, memImportReader{files: map[string]string{
		"/root/a.caddy": "localhost\nrespond ok\nimport later-missing.caddy\n",
	}}, "import a.caddy\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	if blocks != nil {
		t.Fatalf("expected nil blocks on failure, got %d", len(blocks))
	}
}

// Positional arguments are not visible inside a snippet defined by an
// imported file when that snippet is later used without arguments.
func TestParseArgsDoNotLeakIntoSnippetDefinition(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/a.caddy": "(s) {\n\tdir {args[0]}\n}\nimport s\n",
	}}
	_, err := parseWith(t, r, "import a.caddy OUTER\n")
	if err == nil {
		t.Fatal("expected out-of-bounds error because the snippet use has no args")
	}
	if !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("expected an out-of-bounds error, got %v", err)
	}
}

// Arguments on a parent file import do not flow into nested files.
func TestParseArgsDoNotLeakIntoNestedFile(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/parent.caddy": "import child.caddy\n",
		"/root/child.caddy":  "respond {args[0]}\n",
	}}
	_, err := parseWith(t, r, "import parent.caddy LEAK\n")
	if err == nil || !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("expected out-of-bounds error in child, got %v", err)
	}
}

// A snippet invoked with its own argument uses that value even when an
// outer import also had arguments.
func TestParseSnippetRedeclaresArgs(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/p.caddy": "(t) {\n\trespond {args[0]}\n}\nimport t childval\n",
	}}
	blocks := mustParse(t, r, "import p.caddy outerval\n")
	if got := strings.TrimSpace(blockTexts(blocks)); !strings.Contains(got, "childval") {
		t.Fatalf("expected snippet to use its own arg, got %s", blockTexts(blocks))
	}
}

// Placeholders are only substituted as complete marks: ordinary braces,
// escaped braces and adjacent text remain literal, while complete marks
// embedded in a token and variadic marks on their own are handled.
func TestParsePlaceholderSubstitution(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/f.caddy": "key plain{brace}\nkey esc\\{x\\}\nkey a{args[0]}b\nkey {args[:]}\nkey inline{args[1:3]}\n",
	}}
	blocks := mustParse(t, r, "import f.caddy one two three\n")
	got := blockTexts(blocks)
	for _, want := range []string{
		"plain{brace}",
		"esc{x}",
		"aoneb",
		"one|two|three|",
		// variadic mark adjacent to text is not expanded, stays literal
		"inline{args[1:3]}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected result to contain %q\nfull result:\n%s", want, got)
		}
	}
}

// Empty arguments and arguments with whitespace or quotes survive as a
// single raw argument; variadic args expand in declaration order.
func TestParseRawAndVariadicArgs(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/g.caddy": "respond {args[0]}\nrespond {args[1]}\nrespond {args[:]}\n",
	}}
	blocks := mustParse(t, r, "import g.caddy \"\" \"hello world\"\n")
	segs := blockTexts(blocks)
	if !strings.Contains(segs, "respond||") {
		t.Errorf("expected empty argument to stay one empty token:\n%s", segs)
	}
	if !strings.Contains(segs, "respond|hello world|") {
		t.Errorf("expected spaced argument to stay one raw token:\n%s", segs)
	}
}

// Undeclared, negative and out-of-bounds references fail at the
// placeholder location with a normalized file path.
func TestParseInvalidArgReferences(t *testing.T) {
	cases := map[string]string{
		"x {args[2]}":   "out of bounds",
		"x {args[-1]}":  "negative index",
		"x {args[abc]}": "invalid index",
	}
	for content, want := range cases {
		r := memImportReader{files: map[string]string{"/root/b.caddy": content + "\n"}}
		_, err := parseWith(t, r, "import b.caddy a\n")
		if err == nil {
			t.Errorf("input %q: expected error", content)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("input %q: expected %q in error, got %v", content, want, err)
		}
		if !strings.Contains(err.Error(), "/root/b.caddy:1") {
			t.Errorf("input %q: expected normalized location, got %v", content, err)
		}
	}
}

// Relative imports resolve from the declaring file's directory with "."
// and ".." segments cleaned.
func TestParseRelativeImportCleaned(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/dir/./a.caddy": "import ../sub/./b.caddy\n",
		"/root/sub/b.caddy":   "newhost\n",
	}}
	// mem reader keys are literal; emulate cleaning by also keying the
	// cleaned pattern target.
	r.files["/root/dir/a.caddy"] = r.files["/root/dir/./a.caddy"]
	blocks := mustParse(t, r, "import /root/dir/a.caddy\n")
	if got := blocks[0].GetKeysText()[0]; got != "newhost" {
		t.Fatalf("expected cleaned relative import, got %q", got)
	}
}

// Wildcard matches expand in lexicographic order; the same file named by
// two separate statements expands twice.
func TestParseWildcardOrderAndDuplicates(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/d/z.caddy": "hostz {\n\trespond z\n}\n",
		"/root/d/a.caddy": "hosta {\n\trespond a\n}\n",
		"/root/d/m.caddy": "hostm {\n\trespond m\n}\n",
	}}
	blocks := mustParse(t, r, "import /root/d/*.caddy\n")
	got := []string{blocks[0].GetKeysText()[0], blocks[1].GetKeysText()[0], blocks[2].GetKeysText()[0]}
	if want := []string{"hosta", "hostm", "hostz"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expected lexicographic order %v, got %v", want, got)
	}

	dup := mustParse(t, r, "import /root/d/a.caddy\nimport /root/d/a.caddy\n")
	if len(dup) != 2 {
		t.Fatalf("expected duplicate import to expand twice, got %d blocks", len(dup))
	}
}

// A wildcard that matches nothing preserves the successful no-op
// semantics and must not produce an error.
func TestParseWildcardNoMatchIsNotError(t *testing.T) {
	r := memImportReader{files: map[string]string{}}
	blocks, err := parseWith(t, r, "import /root/missing/*.caddy\n")
	if err != nil {
		t.Fatalf("expected no error for unmatched glob, got %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("expected no blocks, got %d", len(blocks))
	}
}

// An active import cycle fails with a stable first-failure location and
// a readable recursion chain.
func TestParseImportCycleChain(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/x.caddy": "import y.caddy\n",
		"/root/y.caddy": "import z.caddy\n",
		"/root/z.caddy": "import x.caddy\n",
	}}
	_, err := parseWith(t, r, "import x.caddy\n")
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	want := "import cycle detected: /root/x.caddy -> /root/y.caddy -> /root/z.caddy -> /root/x.caddy"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected readable chain %q, got %v", want, err)
	}
}

// Unreadable targets and unterminated quotes fail and name the path.
func TestParseUnreadableAndUnclosedQuote(t *testing.T) {
	if _, err := parseWith(t, memImportReader{files: map[string]string{}},
		"import /root/nope.caddy\n"); err == nil {
		t.Fatal("expected an error for an unreadable target")
	}

	r := memImportReader{files: map[string]string{
		"/root/q.caddy": "respond \"unterminated\n",
	}}
	_, err := parseWith(t, r, "import q.caddy\n")
	if err == nil {
		t.Fatal("expected an error for an unterminated quote")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("expected unterminated quote error, got %v", err)
	}
}

// Identical inputs produce identical directive bytes and error text.
func TestParseDeterministic(t *testing.T) {
	r := memImportReader{files: map[string]string{
		"/root/d/z.caddy": "hostz\n",
		"/root/d/a.caddy": "hosta\n",
	}}
	b1, e1 := parseWith(t, r, "import /root/d/*.caddy\n")
	b2, e2 := parseWith(t, r, "import /root/d/*.caddy\n")
	if blockTexts(b1) != blockTexts(b2) {
		t.Fatal("successful parses differed")
	}
	if fmt.Sprint(e1) != fmt.Sprint(e2) {
		t.Fatalf("error text differed: %v vs %v", e1, e2)
	}

	bad := memImportReader{files: map[string]string{
		"/root/c.caddy": "import c.caddy\n",
	}}
	_, f1 := parseWith(t, bad, "import c.caddy\n")
	_, f2 := parseWith(t, bad, "import c.caddy\n")
	if f1.Error() != f2.Error() {
		t.Fatalf("failure text differed: %q vs %q", f1, f2)
	}
}
