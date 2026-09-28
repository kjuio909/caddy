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
	"strconv"
	"strings"
	"sync"
	"testing"
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

func parseTree(t *testing.T, tree FileTree) ([][]string, error) {
	t.Helper()
	root := tree["Caddyfile"]
	blocks, err := Parse("Caddyfile", root, tree)
	if err != nil {
		return nil, err
	}
	return flattenBlocks(blocks), nil
}

func TestParseTreeBasicImport(t *testing.T) {
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n\timport sub/site.caddy\n}\n"),
		"sub/site.caddy": []byte(`respond "hello world"
redir /foo /bar`),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]string{
		{"respond", "hello world"},
		{"redir", "/foo", "/bar"},
	}
	assertLines(t, want, lines)
}

func TestParseTreeRelativeImportIndependentOfWorkingDir(t *testing.T) {
	// relative patterns resolve against the importing virtual file,
	// never against the process working directory
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n\timport a/f.caddy\n}\n"),
		"a/f.caddy": []byte("import g.caddy\n"),
		"a/g.caddy": []byte("directive-a value\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-a", "value"}}, lines)
}

func TestParseTreeSeparatorNormalization(t *testing.T) {
	// backslashes and forward slashes must resolve identically
	tree := FileTree{
		"Caddyfile":   []byte(":8080 {\n\timport sub\\f.caddy\n}\n"),
		"sub/f.caddy": []byte("directive-sep value\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-sep", "value"}}, lines)
}

func TestImportArgsPreserveSpacesQuotesAndEmpty(t *testing.T) {
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy \"hello world\" \"\" `back tick`\n" +
			"}\n"),
		"f.caddy": []byte("a {args[0]}\nb {args[1]}\nc {args[2]}\n"),
	}
	blocks, err := Parse("Caddyfile", tree["Caddyfile"], tree)
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
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy VALUE\n" +
			"}\n"),
		"f.caddy": []byte("join pre-{args[0]}-post\n" +
			"json {\"k\":\"{args[0]}\"}\n" +
			"plain {host} {remote} {{}}\n" +
			"escaped \\{args[0]\\}\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"join", "pre-VALUE-post"},
		{"json", `{"k":"VALUE"}`},
		{"plain", "{host}", "{remote}", "{{}}"},
		{"escaped", `{args[0]}`},
	}, lines)
}

func TestImportArgsVariadic(t *testing.T) {
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n" +
			"\timport f.caddy one \"two words\" three\n" +
			"}\n"),
		"f.caddy": []byte("all {args[:]}\n" +
			"tail {args[1:]}\n" +
			"span {args[0:2]}\n" +
			"adjacent {args[0]}:{args[1]}\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"all", "one", "two words", "three"},
		{"tail", "two words", "three"},
		{"span", "one", "two words"},
		{"adjacent", "one:two words"},
	}, lines)
}

func TestImportArgsDeprecatedFormStillExpands(t *testing.T) {
	tree := FileTree{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy old\n}\n"),
		"f.caddy":   []byte("d {args.0}\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"d", "old"}}, lines)
}

func TestImportArgsLexicalScopeAcrossFile(t *testing.T) {
	// the outer import passes "outer"; the inner file is reached
	// without redeclaring arguments, so its placeholder must fail
	// rather than silently seeing "outer" or becoming empty
	tree := FileTree{
		"Caddyfile":   []byte(":8080 {\n\timport outer.caddy outer-value\n}\n"),
		"outer.caddy": []byte("import inner.caddy\n"),
		"inner.caddy": []byte("inner {args[0]}\n"),
	}
	if _, err := parseTree(t, tree); err == nil {
		t.Fatal("expected an error for the undeclared placeholder, got nil")
	} else if !strings.Contains(err.Error(), "inner.caddy:1") {
		t.Fatalf("expected error to point at inner.caddy:1, got %v", err)
	} else if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected an out-of-range message, got %v", err)
	}

	// redeclaring at the inner layer shadows the outer value
	tree["outer.caddy"] = []byte("import inner.caddy inner-value\n")
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"inner", "inner-value"}}, lines)
}

func TestImportArgsLexicalScopeAcrossSnippet(t *testing.T) {
	// a snippet defined by an imported file forms its own layer:
	// the file's arguments are not visible to the snippet unless
	// the snippet import supplies them itself
	tree := FileTree{
		"Caddyfile":  []byte("import defs.caddy outer\n:8080 {\n\timport greet\n}\n"),
		"defs.caddy": []byte("(greet) {\n\tgreet {args[0]}\n}\n"),
	}
	if _, err := parseTree(t, tree); err == nil {
		t.Fatal("expected an error for the undeclared snippet placeholder, got nil")
	}

	tree["Caddyfile"] = []byte("import defs.caddy outer\n:8080 {\n\timport greet inner\n}\n")
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"greet", "inner"}}, lines)
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
			tree := FileTree{
				"Caddyfile": []byte(":8080 {\n\timport f.caddy a b\n}\n"),
				"f.caddy":   []byte(tc.content),
			}
			blocks, err := Parse("Caddyfile", tree["Caddyfile"], tree)
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
		tree := FileTree{"Caddyfile": []byte("import nope.caddy\n")}
		blocks, err := Parse("Caddyfile", tree["Caddyfile"], tree)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if blocks != nil {
			t.Fatal("expected nil blocks on failure")
		}
	})

	t.Run("import target is a directory", func(t *testing.T) {
		tree := FileTree{
			"Caddyfile":   []byte("import dir\n"),
			"dir/f.caddy": []byte("x\n"),
		}
		if _, err := Parse("Caddyfile", tree["Caddyfile"], tree); err == nil {
			t.Fatal("expected an error when importing a directory")
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
		tree := FileTree{
			"Caddyfile": []byte("import f.caddy\n"),
			"f.caddy":   []byte("respond \"never closed\n"),
		}
		_, err := Parse("Caddyfile", tree["Caddyfile"], tree)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "f.caddy") {
			t.Fatalf("expected error to name f.caddy, got %v", err)
		}
	})
}

func TestParseTreeGlobOrderAndNoDedup(t *testing.T) {
	// glob matches expand in sorted name order
	tree := FileTree{
		"Caddyfile":    []byte(":8080 {\n\timport conf/*.caddy\n}\n"),
		"conf/b.caddy": []byte("directive-b\n"),
		"conf/a.caddy": []byte("directive-a\n"),
		"conf/.hidden": []byte("directive-hidden\n"), // skipped for bare star
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"directive-a"},
		{"directive-b"},
	}, lines)

	// the same file imported twice from different statements (and
	// twice from different branches) expands per statement
	tree = FileTree{
		"Caddyfile":    []byte(":8080 {\n\timport shared.caddy\n\timport shared.caddy\n}\n"),
		"shared.caddy": []byte("once\n"),
	}
	lines, err = parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"once"}, {"once"}}, lines)
}

func TestParseTreeSnapshotIsolationAndReparse(t *testing.T) {
	mkTree := func(value string) FileTree {
		return FileTree{
			"Caddyfile": []byte(":8080 {\n\timport f.caddy\n}\n"),
			"f.caddy":   []byte("value " + value + "\n"),
		}
	}

	// fail first with an unreadable target, then immediately succeed
	// with a different tree using the same top-level filename
	bad := FileTree{"Caddyfile": []byte("import f.caddy\n")}
	if _, err := Parse("Caddyfile", bad["Caddyfile"], bad); err == nil {
		t.Fatal("expected first parse to fail")
	}
	lines, err := parseTree(t, mkTree("second"))
	if err != nil {
		t.Fatalf("second parse after failure: %v", err)
	}
	assertLines(t, [][]string{{"value", "second"}}, lines)

	// mutating the caller's map after Parse must not change a result
	// that was computed from the snapshot
	tree := mkTree("frozen")
	blocks, err := Parse("Caddyfile", tree["Caddyfile"], tree)
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
				blocks, perr := Parse("Caddyfile", tree["Caddyfile"], tree)
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

func TestParseTreeUnrelatedDirectivesAndNoArgImportsCompatible(t *testing.T) {
	tree := FileTree{
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
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"first", "one"},
		{"middle", "two"},
		{"repeated"},
		{"repeated"},
		{"last"},
	}, lines)
}

func TestParseTreeImportedSnippetWithArgs(t *testing.T) {
	tree := FileTree{
		"Caddyfile":      []byte("import snippets.caddy\n:8080 {\n\timport site example.com\n}\n"),
		"snippets.caddy": []byte("(site) {\n\thost {args[0]}\n}\n"),
	}
	lines, err := parseTree(t, tree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"host", "example.com"}}, lines)
}

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
