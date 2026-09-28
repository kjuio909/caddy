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
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// flattenBlocks renders the parsed blocks as a sequence of
// "directive|arg|arg" lines: the directive order and exact argument
// bytes are the only observation surface these tests rely on.
func flattenBlocks(blocks []ServerBlock) [][]string {
	var out [][]string
	for _, b := range blocks {
		for _, seg := range b.Segments {
			var line []string
			for _, tok := range seg {
				line = append(line, tok.Text)
			}
			out = append(out, line)
		}
	}
	return out
}

func parseReader(t *testing.T, tree ImportFiles) ([][]string, error) {
	t.Helper()
	return parseReaderFile(t, "Caddyfile", tree)
}

func parseReaderFile(t *testing.T, root string, tree ImportFiles) ([][]string, error) {
	t.Helper()
	blocks, err := Parse(root, tree[root], WithImportFileReader(tree))
	if err != nil {
		return nil, err
	}
	return flattenBlocks(blocks), nil
}

func TestParseReaderBasicImport(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile":      []byte(":8080 {\n\timport sub/site.caddy\n}\n"),
		"sub/site.caddy": []byte("respond \"hello world\"\nredir /foo /bar"),
	}
	lines, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"respond", "hello world"},
		{"redir", "/foo", "/bar"},
	}, lines)
}

func TestParseReaderFileTreeAlias(t *testing.T) {
	// FileTree is an alias of ImportFiles and must behave identically
	tree := FileTree{"Caddyfile": []byte(":8080 {\n\timport f.caddy\n}\n"), "f.caddy": []byte("ok\n")}
	lines, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"ok"}}, lines)
}

func TestParseReaderRelativeImportIndependentOfWorkingDir(t *testing.T) {
	// relative patterns resolve against the importing virtual file,
	// never against the process working directory
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport a/f.caddy\n}\n"),
		"a/f.caddy": []byte("import g.caddy\n"),
		"a/g.caddy": []byte("directive-a value\n"),
	}
	lines, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-a", "value"}}, lines)
}

func TestParseReaderDotDotCleaning(t *testing.T) {
	// "." and ".." are cleaned against the declaring file's directory
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport ./a/../a/f.caddy\n}\n"),
		"a/f.caddy": []byte("import ./../a/g.caddy\n"),
		"a/g.caddy": []byte("cleaned value\n"),
	}
	lines, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"cleaned", "value"}}, lines)

	// escaping the tree's root is a hard error located at the import
	tree["Caddyfile"] = []byte("import ../outside.caddy\n")
	blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
	if err == nil {
		t.Fatal("expected an error for an import escaping the tree, got nil")
	}
	if blocks != nil {
		t.Fatal("expected nil blocks on failure")
	}
	if !strings.Contains(err.Error(), "resolves outside of the file tree") {
		t.Fatalf("expected an escape error, got %v", err)
	}
}

func TestParseReaderSeparatorNormalization(t *testing.T) {
	// backslashes and forward slashes must resolve identically
	tree := ImportFiles{
		"Caddyfile":   []byte(":8080 {\n\timport sub\\f.caddy\n}\n"),
		"sub/f.caddy": []byte("directive-sep value\n"),
	}
	lines, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-sep", "value"}}, lines)
}

func TestImportArgsPreserveSpacesQuotesAndEmpty(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy \"hello world\" \"\" `back tick`\n" +
			"}\n"),
		"f.caddy": []byte("a {args[0]}\nb {args[1]}\nc {args[2]}\n"),
	}
	blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := flattenBlocks(blocks)
	assertLines(t, [][]string{
		{"a", "hello world"},
		{"b", ""},
		{"c", "back tick"},
	}, lines)

	// the quoted provenance travels with the argument, so a
	// backtick argument is still distinguishable as quoted and no
	// re-tokenization happened
	if !blocks[0].Segments[0][1].Quoted() {
		t.Errorf("expected space-bearing argument to retain its quotes")
	}
}

func TestImportArgsAdjacentTextAndPlainBraces(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy VALUE\n" +
			"}\n"),
		"f.caddy": []byte("join pre-{args[0]}-post\n" +
			"json {\"k\":\"{args[0]}\"}\n" +
			"plain {host} {remote} {{}}\n" +
			"escaped \\{args[0]\\}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"join", "pre-VALUE-post"},
		{"json", `{"k":"VALUE"}`},
		{"plain", "{host}", "{remote}", "{{}}"},
		{"escaped", `{args[0]}`},
	}, layers)
}

func TestImportArgsVariadic(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy one \"two words\" three\n" +
			"}\n"),
		"f.caddy": []byte("all {args[:]}\n" +
			"tail {args[1:]}\n" +
			"span {args[0:2]}\n" +
			"adjacent {args[0]}:{args[1]}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"all", "one", "two words", "three"},
		{"tail", "two words", "three"},
		{"span", "one", "two words"},
		{"adjacent", "one:two words"},
	}, layers)
}

func TestImportArgsDeprecatedFormStillExpands(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy old\n}\n"),
		"f.caddy":   []byte("d {args.0}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"d", "old"}}, layers)
}

func TestImportArgsLexicalScopeAcrossFile(t *testing.T) {
	// the outer import passes "outer"; the inner file is reached
	// without redeclaring arguments, so its placeholder must fail
	// rather than silently seeing "outer" or becoming empty
	tree := ImportFiles{
		"Caddyfile":   []byte(":8080 {\n\timport outer.caddy outer-value\n}\n"),
		"outer.caddy": []byte("import inner.caddy\n"),
		"inner.caddy": []byte("inner {args[0]}\n"),
	}
	if _, err := parseReader(t, tree); err == nil {
		t.Fatal("expected an error for the undeclared placeholder, got nil")
	} else if !strings.Contains(err.Error(), "inner.caddy:1") {
		t.Fatalf("expected error to point at inner.caddy:1, got %v", err)
	} else if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected an out-of-range message, got %v", err)
	}

	// redeclaring at the inner layer shadows the outer value
	tree["outer.caddy"] = []byte("import inner.caddy inner-value\n")
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"inner", "inner-value"}}, layers)
}

func TestImportArgsLexicalScopeAcrossSnippet(t *testing.T) {
	// a snippet defined by an imported file forms its own layer:
	// the file's arguments are not visible to the snippet unless
	// the snippet import supplies them itself
	tree := ImportFiles{
		"Caddyfile":  []byte("import defs.caddy outer\n:8080 {\n\timport greet\n}\n"),
		"defs.caddy": []byte("(greet) {\n\tgreet {args[0]}\n}\n"),
	}
	if _, err := parseReader(t, tree); err == nil {
		t.Fatal("expected an error for the undeclared snippet placeholder, got nil")
	}

	tree["Caddyfile"] = []byte("import defs.caddy outer\n:8080 {\n\timport greet inner\n}\n")
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"greet", "inner"}}, layers)
}

func TestImportArgsDoNotLeakBetweenCalls(t *testing.T) {
	// a successful call with an argument must not leave the value
	// visible to a later call that declares none
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy first\n}\n"),
		"f.caddy":   []byte("d {args[0]}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	assertLines(t, [][]string{{"d", "first"}}, layers)

	tree["Caddyfile"] = []byte(":8080 {\n\timport f.caddy\n}\n")
	if _, err := parseReader(t, tree); err == nil {
		t.Fatal("expected an out-of-range error without arguments, got nil")
	}

	// and a following call with new arguments observes only the new ones
	tree["Caddyfile"] = []byte(":8080 {\n\timport f.caddy second\n}\n")
	layers, err = parseReader(t, tree)
	if err != nil {
		t.Fatalf("third parse: %v", err)
	}
	assertLines(t, [][]string{{"d", "second"}}, layers)
}

func TestImportArgsErrorsAreHard(t *testing.T) {
	for i, tc := range []struct {
		name    string
		content string
		wantSub string
	}{
		{"insufficient args", "d {args[2]}\n", "out of range"},
		{"non-numeric index", "d {args[x]}\n", "invalid argument"},
		{"negative index", "d {args[-1]}\n", "invalid argument"},
		{"empty index", "d {args[]}\n", "empty index"},
		{"inverted range", "d {args[2:1]}\n", "start index greater than its end index"},
		{"range too far", "d {args[0:5]}\n", "out of range"},
		{"embedded variadic", "d x{args[:]}y\n", "must be a token on its own"},
		{"malformed args brace", "d {args[}\n", "invalid argument placeholder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := ImportFiles{
				"Caddyfile": []byte(":8080 {\n\timport f.caddy a b\n}\n"),
				"f.caddy":   []byte(tc.content),
			}
			blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
			if err == nil {
				t.Fatalf("case %d: expected an error, got blocks %v", i, flattenBlocks(blocks))
			}
			if blocks != nil {
				t.Fatalf("case %d: failed parse returned non-nil blocks", i)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("case %d: error %q does not contain %q", i, err.Error(), tc.wantSub)
			}
			// location must identify the imported file and line 1,
			// using the normalized virtual path
			if !strings.Contains(err.Error(), "f.caddy:1") {
				t.Fatalf("case %d: error %q does not locate f.caddy:1", i, err.Error())
			}
		})
	}
}

func TestParseRejectsUnreadableAndUnclosedQuote(t *testing.T) {
	t.Run("missing import target", func(t *testing.T) {
		tree := ImportFiles{"Caddyfile": []byte("import nope.caddy\n")}
		blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if blocks != nil {
			t.Fatal("expected nil blocks on failure")
		}
		if !strings.Contains(err.Error(), "nope.caddy") {
			t.Fatalf("expected error to name nope.caddy, got %v", err)
		}
	})

	t.Run("import target is a directory", func(t *testing.T) {
		tree := ImportFiles{
			"Caddyfile":   []byte("import dir\n"),
			"dir/f.caddy": []byte("x\n"),
		}
		if _, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree)); err == nil {
			t.Fatal("expected an error when importing a directory")
		}
	})

	t.Run("reader error propagates", func(t *testing.T) {
		reader := errorReader{}
		_, err := Parse("Caddyfile", []byte("import f.caddy\n"), WithImportFileReader(reader))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !errors.Is(err, errReaderBoom) {
			t.Fatalf("expected the reader error to propagate, got %v", err)
		}
	})

	t.Run("unclosed quote in root", func(t *testing.T) {
		blocks, err := Parse("Caddyfile", []byte("respond \"never closed"))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !errors.Is(err, errUnclosedQuote) {
			t.Fatalf("expected errUnclosedQuote, got %v", err)
		}
		if blocks != nil {
			t.Fatal("expected nil blocks on failure")
		}
	})

	t.Run("unclosed quote in imported file", func(t *testing.T) {
		tree := ImportFiles{
			"Caddyfile": []byte("import f.caddy\n"),
			"f.caddy":   []byte("respond \"never closed\n"),
		}
		_, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "f.caddy") {
			t.Fatalf("expected error to name f.caddy, got %v", err)
		}
	})
}

func TestParseReaderGlobOrderAndNoDedup(t *testing.T) {
	// glob matches expand in sorted name order
	tree := ImportFiles{
		"Caddyfile":    []byte(":8080 {\n\timport conf/*.caddy\n}\n"),
		"conf/b.caddy": []byte("directive-b\n"),
		"conf/a.caddy": []byte("directive-a\n"),
		"conf/.hidden": []byte("directive-hidden\n"), // skipped for bare star
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"directive-a"},
		{"directive-b"},
	}, layers)

	// the same file imported twice from different statements (and
	// twice from different branches) expands per statement
	tree = ImportFiles{
		"Caddyfile":    []byte(":8080 {\n\timport shared.caddy\n\timport shared.caddy\n}\n"),
		"shared.caddy": []byte("once\n"),
	}
	layers, err = parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"once"}, {"once"}}, layers)
}

func TestParseReaderGlobNoMatchIsNotAnError(t *testing.T) {
	// existing success semantics: a glob with no matches imports nothing
	tree := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\tfirst\n\timport conf/*.caddy\n\tlast\n}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"first"}, {"last"}}, layers)
}

func TestParseReaderSnapshotIsolationAndReparse(t *testing.T) {
	mkTree := func(value string) ImportFiles {
		return ImportFiles{
			"Caddyfile": []byte(":8080 {\n\timport f.caddy\n}\n"),
			"f.caddy":   []byte("value " + value + "\n"),
		}
	}

	// fail first with an unreadable target, then immediately succeed
	// with a different set of static contents using the same
	// top-level filename: the result may only come from this input
	bad := ImportFiles{"Caddyfile": []byte("import f.caddy\n")}
	if _, err := Parse("Caddyfile", bad["Caddyfile"], WithImportFileReader(bad)); err == nil {
		t.Fatal("expected first parse to fail")
	}
	layers, err := parseReader(t, mkTree("second"))
	if err != nil {
		t.Fatalf("second parse after failure: %v", err)
	}
	assertLines(t, [][]string{{"value", "second"}}, layers)

	// mutating the caller's map after Parse must not change a result
	// that was computed from the snapshot
	tree := mkTree("frozen")
	blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tree["f.caddy"] = []byte("value mutated\n")
	assertLines(t, [][]string{{"value", "frozen"}}, flattenBlocks(blocks))

	// concurrent parses with distinct trees over the same filenames;
	// every worker must observe only its own argument value
	const workers = 32
	const iterations = 20
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			want := "worker-" + strconv.Itoa(w)
			for i := 0; i < iterations; i++ {
				tree := mkTree(want)
				blocks, perr := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
				if perr != nil {
					errCh <- perr
					return
				}
				got := flattenBlocks(blocks)
				if len(got) != 1 || len(got[0]) != 2 || got[0][1] != want {
					errCh <- fmt.Errorf("parallel parse observed foreign arguments: want %q got %v", want, got)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestParseReaderDeterministicRepetition(t *testing.T) {
	// the same input parsed repeatedly must yield identical bytes,
	// regardless of the reader's backing allocations
	reader := staticReader{files: map[string]string{
		"conf/b.caddy": "from-b\n",
		"conf/a.caddy": "from-a\n",
	}}
	root := []byte(":8080 {\n\timport conf/*.caddy\n}\n")

	var first [][]string
	for i := 0; i < 5; i++ {
		blocks, err := Parse("Caddyfile", root, WithImportFileReader(reader))
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		got := flattenBlocks(blocks)
		if i == 0 {
			first = got
			continue
		}
		assertLines(t, first, got)
	}
}

func TestParseReaderUnrelatedDirectivesAndNoArgImportsCompatible(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile": []byte("localhost:8080 {\n" +
			"\tfirst one\n" +
			"\timport plain.caddy\n" +
			"\timport repeat.caddy\n" +
			"\timport repeat.caddy\n" +
			"\tlast\n" +
			"}\n"),
		"plain.caddy":  []byte("middle two\n"),
		"repeat.caddy": []byte("repeated\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"first", "one"},
		{"middle", "two"},
		{"repeated"},
		{"repeated"},
		{"last"},
	}, layers)
}

func TestParseReaderImportedSnippetWithArgs(t *testing.T) {
	tree := ImportFiles{
		"Caddyfile":      []byte("import snippets.caddy\n:8080 {\n\timport site example.com\n}\n"),
		"snippets.caddy": []byte("(site) {\n\thost {args[0]}\n}\n"),
	}
	layers, err := parseReader(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"host", "example.com"}}, layers)
}

func TestParseReaderFromFS(t *testing.T) {
	fsys := fstest.MapFS{
		"sub/f.caddy": &fstest.MapFile{Data: []byte("from-fs value\n")},
	}
	blocks, err := Parse("Caddyfile",
		[]byte(":8080 {\n\timport sub/f.caddy\n}\n"),
		WithImportFileReader(NewImportFileReader(fsys)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"from-fs", "value"}}, flattenBlocks(blocks))
}

func TestImportCycleReportsStableLoop(t *testing.T) {
	t.Run("snippets", func(t *testing.T) {
		input := []byte("(import1) {\n\timport import2\n}\n" +
			"(import2) {\n\timport import1\n}\n" +
			"import import1\n")
		var first string
		for i := 0; i < 3; i++ {
			blocks, err := Parse("Caddyfile", input)
			if err == nil {
				t.Fatalf("iteration %d: expected a cycle error, got blocks %v", i, blocks)
			}
			if blocks != nil {
				t.Fatalf("iteration %d: expected nil blocks on failure", i)
			}
			msg := err.Error()
			if !strings.Contains(msg, "a cycle of imports exists between Caddyfile:import2 and Caddyfile:import1") {
				t.Fatalf("iteration %d: unexpected cycle message: %q", i, msg)
			}
			if !strings.Contains(msg, "Caddyfile:import1 -> Caddyfile:import2 -> Caddyfile:import1") {
				t.Fatalf("iteration %d: message %q lacks the closed loop order", i, msg)
			}
			if i == 0 {
				first = msg
			} else if msg != first {
				t.Fatalf("cycle error is not stable:\nfirst:  %q\nrepeat: %q", first, msg)
			}
		}
	})

	t.Run("files", func(t *testing.T) {
		tree := ImportFiles{
			"Caddyfile": []byte("import a.caddy\n"),
			"a.caddy":   []byte("import b.caddy\n"),
			"b.caddy":   []byte("import a.caddy\n"),
		}
		blocks, err := Parse("Caddyfile", tree["Caddyfile"], WithImportFileReader(tree))
		if err == nil {
			t.Fatal("expected a cycle error, got blocks")
		}
		if blocks != nil {
			t.Fatal("expected nil blocks on failure")
		}
		msg := err.Error()
		if !strings.Contains(msg, "a cycle of imports exists between b.caddy and a.caddy") {
			t.Fatalf("unexpected cycle message: %q", msg)
		}
		if !strings.Contains(msg, "a.caddy -> b.caddy -> a.caddy") {
			t.Fatalf("message %q lacks the closed loop order", msg)
		}
	})
}

func TestParseAfterFailureOnlySeesNewInput(t *testing.T) {
	// a failed parse must not leave partial state that a follow-up
	// parse with fresh, valid contents could observe
	failing := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy a b\n}\n"),
		"f.caddy":   []byte("d {args[5]}\n"),
	}
	if blocks, err := Parse("Caddyfile", failing["Caddyfile"], WithImportFileReader(failing)); err == nil {
		t.Fatalf("expected first parse to fail, got %v", flattenBlocks(blocks))
	}

	valid := ImportFiles{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy only\n}\n"),
		"f.caddy":   []byte("d {args[0]}\nirrelevant site-directive kept\n"),
	}
	blocks, err := Parse("Caddyfile", valid["Caddyfile"], WithImportFileReader(valid))
	if err != nil {
		t.Fatalf("second parse failed: %v", err)
	}
	assertLines(t, [][]string{
		{"d", "only"},
		{"irrelevant", "site-directive", "kept"},
	}, flattenBlocks(blocks))
}

// staticReader is a hand-written ImportFileReader that returns
// contents from a string map and deterministic glob matches.
type staticReader struct {
	files map[string]string
}

func (s staticReader) ReadFile(name string) ([]byte, error) {
	contents, ok := s.files[NormalizeImportPath(name)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(contents), nil
}

func (s staticReader) Glob(pattern string) ([]string, error) {
	var matches []string
	for name := range s.files {
		ok, err := path.Match(NormalizeImportPath(pattern), NormalizeImportPath(name))
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, name)
		}
	}
	return matches, nil
}

var errReaderBoom = errors.New("reader boom")

// errorReader fails every read with a sentinel error.
type errorReader struct{}

func (errorReader) ReadFile(string) ([]byte, error) { return nil, errReaderBoom }
func (errorReader) Glob(string) ([]string, error)   { return nil, errReaderBoom }

func assertLines(t *testing.T, want, got [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %d directive lines, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("line %d: expected %v, got %v", i, want[i], got[i])
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("line %d, arg %d: expected %q, got %q", i, j, want[i][j], got[i][j])
			}
		}
	}
}
