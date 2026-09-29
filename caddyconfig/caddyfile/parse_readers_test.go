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
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// readersFor builds a FileReaderFunc over an in-memory file set.
func readersFor(files map[string]string) FileReaderFunc {
	return func(name string) (io.ReadCloser, error) {
		contents, ok := files[normalizeImportPath(name)]
		if !ok {
			return nil, &fsPathError{op: "open", name: name, err: errNotExist}
		}
		return io.NopCloser(strings.NewReader(contents)), nil
	}
}

var errNotExist = errors.New("file does not exist")

type fsPathError struct {
	op, name string
	err      error
}

func (e *fsPathError) Error() string { return e.op + " " + e.name + ": " + e.err.Error() }
func (e *fsPathError) Unwrap() error { return e.err }

// flattenBlocks renders parsed blocks as a sequence of token-text
// lines: directive order and exact argument bytes are the observation
// surface these tests rely on.
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

func parseWithReaders(t *testing.T, files map[string]string) ([][]string, error) {
	t.Helper()
	readers := readersFor(files)
	blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readers)
	if err != nil {
		return nil, err
	}
	return flattenBlocks(blocks), nil
}

func TestParseReadersBasicImport(t *testing.T) {
	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile":      ":8080 {\n\timport sub/site.caddy\n}\n",
		"sub/site.caddy": "respond \"hello world\"\nredir /foo /bar\n",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"respond", "hello world"},
		{"redir", "/foo", "/bar"},
	}, lines)
}

func TestParseReadersRelativeImportIndependentOfWorkingDir(t *testing.T) {
	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile": ":8080 {\n\timport a/f.caddy\n}\n",
		"a/f.caddy": "import g.caddy\n",
		"a/g.caddy": "directive-a value\n",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-a", "value"}}, lines)
}

func TestParseReadersNormalizesSeparatorsAndDotSegments(t *testing.T) {
	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile":   ":8080 {\n\timport a/../sub/./f.caddy\n}\n",
		"sub/f.caddy": "directive-norm value\n",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-norm", "value"}}, lines)

	// backslashes resolve identically
	lines, err = parseWithReaders(t, map[string]string{
		"Caddyfile":   ":8080 {\n\timport sub\\f.caddy\n}\n",
		"sub/f.caddy": "directive-sep value\n",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"directive-sep", "value"}}, lines)
}

func TestParseReadersPreserveSpacesQuotesAndEmpty(t *testing.T) {
	files := map[string]string{
		"Caddyfile": ":8080 {\n" +
			"\timport f.caddy \"hello world\" \"\" `back tick`\n" +
			"}\n",
		"f.caddy": "a {args[0]}\nb {args[1]}\nc {args[2]}\n",
	}
	blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"a", "hello world"},
		{"b", ""},
		{"c", "back tick"},
	}, flattenBlocks(blocks))

	// quoting provenance travels with the substituted argument
	if !blocks[0].Segments[0][1].Quoted() {
		t.Errorf("expected space-bearing argument to retain its quotes")
	}
}

func TestParseReadersAdjacentTextPlainAndEscapedBraces(t *testing.T) {
	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile": ":8080 {\n\timport f.caddy VALUE\n}\n",
		"f.caddy": "join pre-{args[0]}-post\n" +
			"json {\"k\":\"{args[0]}\"}\n" +
			"plain {host} {remote} {{}}\n" +
			"escaped \\{args[0]\\}\n",
	})
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

func TestParseReadersVariadic(t *testing.T) {
	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile": ":8080 {\n\timport f.caddy one \"two words\" three\n}\n",
		"f.caddy": "all {args[:]}\n" +
			"tail {args[1:]}\n" +
			"span {args[0:2]}\n" +
			"adjacent {args[0]}:{args[1]}\n",
	})
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

func TestParseReadersArgsScopeAcrossFiles(t *testing.T) {
	files := map[string]string{
		"Caddyfile":   ":8080 {\n\timport outer.caddy outer-value\n}\n",
		"outer.caddy": "import inner.caddy\n",
		"inner.caddy": "inner {args[0]}\n",
	}
	blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
	if err == nil {
		t.Fatalf("expected undeclared placeholder to fail, got blocks %v", flattenBlocks(blocks))
	}
	if !strings.Contains(err.Error(), "inner.caddy:1") {
		t.Fatalf("expected error at inner.caddy:1, got %v", err)
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out-of-range message, got %v", err)
	}

	// redeclaring at the inner layer shadows the outer value
	files["outer.caddy"] = "import inner.caddy inner-value\n"
	lines, err := parseWithReaders(t, files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"inner", "inner-value"}}, lines)
}

func TestParseReadersArgsScopeAcrossSnippet(t *testing.T) {
	files := map[string]string{
		"Caddyfile":  "import defs.caddy outer\n:8080 {\n\timport greet\n}\n",
		"defs.caddy": "(greet) {\n\tgreet {args[0]}\n}\n",
	}
	if _, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files)); err == nil {
		t.Fatal("expected undeclared snippet placeholder to fail")
	}

	files["Caddyfile"] = "import defs.caddy outer\n:8080 {\n\timport greet inner\n}\n"
	lines, err := parseWithReaders(t, files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"greet", "inner"}}, lines)
}

func TestParseReadersArgsErrorsAreHard(t *testing.T) {
	cases := []struct {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"Caddyfile": ":8080 {\n\timport f.caddy a b\n}\n",
				"f.caddy":   tc.content,
			}
			blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
			if err == nil {
				t.Fatalf("expected an error, got blocks %v", flattenBlocks(blocks))
			}
			if blocks != nil {
				t.Fatal("failed parse returned non-nil blocks")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
			if !strings.Contains(err.Error(), "f.caddy:1") {
				t.Fatalf("error %q does not locate f.caddy:1", err.Error())
			}
		})
	}
}

func TestParseReadersMissingUnreadableDirectoryAndUnclosed(t *testing.T) {
	t.Run("missing exact target", func(t *testing.T) {
		files := map[string]string{"Caddyfile": "import nope.caddy\n"}
		blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
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

	t.Run("reader error surfaces", func(t *testing.T) {
		boom := errors.New("disk on fire")
		readers := FileReaderFunc(func(name string) (io.ReadCloser, error) {
			if name == "f.caddy" {
				return nil, boom
			}
			return io.NopCloser(strings.NewReader("")), nil
		})
		_, err := Parse("Caddyfile", []byte("import f.caddy\n"), readers)
		if err == nil || !strings.Contains(err.Error(), "disk on fire") {
			t.Fatalf("expected read error to surface, got %v", err)
		}
	})

	t.Run("import target is a directory", func(t *testing.T) {
		mapfs := fstest.MapFS{
			"dir/f.caddy": &fstest.MapFile{Data: []byte("x\n")},
		}
		_, err := Parse("Caddyfile", []byte("import dir\n"), FromFS(mapfs))
		if err == nil {
			t.Fatal("expected an error when importing a directory")
		}
	})

	t.Run("unclosed quote in root", func(t *testing.T) {
		blocks, err := Parse("Caddyfile", []byte("respond \"never closed"), nil)
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
		files := map[string]string{
			"Caddyfile": "import f.caddy\n",
			"f.caddy":   "respond \"never closed\n",
		}
		_, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
		if err == nil || !strings.Contains(err.Error(), "f.caddy") {
			t.Fatalf("expected error naming f.caddy, got %v", err)
		}
	})
}

func TestParseReadersCycleReportsChain(t *testing.T) {
	files := map[string]string{
		"Caddyfile": "import a.caddy\n",
		"a.caddy":   "import b.caddy\n",
		"b.caddy":   "import a.caddy\n",
	}
	_, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
	if err == nil {
		t.Fatal("expected cycle error")
	}
	for _, want := range []string{"cycle", "a.caddy", "b.caddy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestParseReadersGlobFromFS(t *testing.T) {
	mapfs := fstest.MapFS{
		"Caddyfile":    &fstest.MapFile{Data: []byte(":8080 {\n\timport conf/*.caddy\n\timport shared.caddy\n\timport shared.caddy\n}\n")},
		"conf/b.caddy": &fstest.MapFile{Data: []byte("directive-b\n")},
		"conf/a.caddy": &fstest.MapFile{Data: []byte("directive-a\n")},
		"conf/.hidden": &fstest.MapFile{Data: []byte("directive-hidden\n")},
		"shared.caddy": &fstest.MapFile{Data: []byte("once\n")},
	}
	root := []byte(":8080 {\n\timport conf/*.caddy\n\timport shared.caddy\n\timport shared.caddy\n}\n")
	blocks, err := Parse("Caddyfile", root, FromFS(mapfs))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{
		{"directive-a"},
		{"directive-b"},
		{"once"},
		{"once"},
	}, flattenBlocks(blocks))
}

func TestParseReadersGlobNoMatchIsNotError(t *testing.T) {
	files := map[string]string{
		"Caddyfile": ":8080 {\n\theader\n\timport conf/*.caddy\n\trespond\n}\n",
	}
	// an open-only reader cannot glob: pattern contributes nothing and
	// surrounding directives keep their order and success semantics
	blocks, err := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
	if err != nil {
		t.Fatalf("glob without matches should succeed, got %v", err)
	}
	assertLines(t, [][]string{{"header"}, {"respond"}}, flattenBlocks(blocks))
}

func TestParseReadersSnapshotOneVersionPerCall(t *testing.T) {
	// a reader that changes its answer on every open: one Parse call
	// imports the same file twice and must observe one version only
	var opens atomic.Int64
	readers := FileReaderFunc(func(name string) (io.ReadCloser, error) {
		if name == "f.caddy" {
			opens.Add(1)
			return io.NopCloser(strings.NewReader(fmt.Sprintf("value %d\n", opens.Load()))), nil
		}
		return io.NopCloser(strings.NewReader(":8080 {\n\timport f.caddy\n\timport f.caddy\n}\n")), nil
	})
	blocks, err := Parse("Caddyfile", []byte(":8080 {\n\timport f.caddy\n\timport f.caddy\n}\n"), readers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertLines(t, [][]string{{"value", "1"}, {"value", "1"}}, flattenBlocks(blocks))
}

func TestParseReadersReplacedReaderDoesNotAffectSecondCall(t *testing.T) {
	// fail first with an unreadable target, then parse the same
	// top-level filename against a different file set
	bad := FileReaderFunc(func(name string) (io.ReadCloser, error) {
		if name == "Caddyfile" {
			return io.NopCloser(strings.NewReader("import f.caddy\n")), nil
		}
		return nil, errNotExist
	})
	if _, err := Parse("Caddyfile", []byte("import f.caddy\n"), bad); err == nil {
		t.Fatal("expected first parse to fail")
	}

	lines, err := parseWithReaders(t, map[string]string{
		"Caddyfile": ":8080 {\n\timport f.caddy\n}\n",
		"f.caddy":   "value second\n",
	})
	if err != nil {
		t.Fatalf("second parse after failure: %v", err)
	}
	assertLines(t, [][]string{{"value", "second"}}, lines)
}

func TestParseReadersLaterChangesDoNotRetroactivelyMutate(t *testing.T) {
	backing := map[string][]byte{
		"Caddyfile": []byte(":8080 {\n\timport f.caddy\n}\n"),
		"f.caddy":   []byte("value frozen\n"),
	}
	readers := FileReaderFunc(func(name string) (io.ReadCloser, error) {
		data, ok := backing[name]
		if !ok {
			return nil, errNotExist
		}
		return io.NopCloser(strings.NewReader(string(data))), nil
	})
	blocks, err := Parse("Caddyfile", backing["Caddyfile"], readers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	backing["f.caddy"] = []byte("value mutated\n")
	assertLines(t, [][]string{{"value", "frozen"}}, flattenBlocks(blocks))
}

func TestParseReadersConcurrentIsolation(t *testing.T) {
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
				files := map[string]string{
					"Caddyfile": ":8080 {\n\timport f.caddy\n}\n",
					"f.caddy":   "value " + want + "\n",
				}
				blocks, perr := Parse("Caddyfile", []byte(files["Caddyfile"]), readersFor(files))
				if perr != nil {
					errCh <- perr
					return
				}
				got := flattenBlocks(blocks)
				if len(got) != 1 || len(got[0]) != 2 || got[0][1] != want {
					errCh <- fmt.Errorf("parallel parse observed foreign content: want %q got %v", want, got)
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

func TestParseReadersUnrelatedDirectivesAndPlainImportsCompatible(t *testing.T) {
	files := map[string]string{
		"Caddyfile": "localhost:8080 {\n" +
			"\tfirst one\n" +
			"\timport plain.caddy\n" +
			"\timport repeat.caddy\n" +
			"\timport repeat.caddy\n" +
			"\tlast\n" +
			"}\n",
		"plain.caddy":  "middle two\n",
		"repeat.caddy": "repeated\n",
	}
	lines, err := parseWithReaders(t, files)
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

func TestParseReadersSameInputStableBytesAndError(t *testing.T) {
	// deterministic ordering across many globs and repeated calls
	names := []string{"conf/m.caddy", "conf/a.caddy", "conf/z.caddy", "conf/b.caddy"}
	all := append([]string{}, names...)
	sort.Strings(all)

	mapfs := fstest.MapFS{
		"Caddyfile": &fstest.MapFile{Data: []byte(":8080 {\n\timport conf/*.caddy\n}\n")},
	}
	for _, n := range names {
		mapfs[n] = &fstest.MapFile{Data: []byte("d " + pathBase(n) + "\n")}
	}
	var first [][]string
	for i := 0; i < 5; i++ {
		blocks, err := Parse("Caddyfile", mapfs["Caddyfile"].Data, FromFS(mapfs))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		lines := flattenBlocks(blocks)
		if first == nil {
			first = lines
			continue
		}
		if fmt.Sprintf("%v", lines) != fmt.Sprintf("%v", first) {
			t.Fatalf("iteration %d: %v != %v", i, lines, first)
		}
	}
	want := make([][]string, 0, len(all))
	for _, n := range all {
		want = append(want, []string{"d", pathBase(n)})
	}
	assertLines(t, want, first)

	// identical failing inputs produce identical error text regardless
	// of call index
	failing := map[string]string{
		"Caddyfile": ":8080 {\n\timport f.caddy\n}\n",
		"f.caddy":   "d {args[9]}\n",
	}
	var msg string
	for i := 0; i < 3; i++ {
		_, err := Parse("Caddyfile", []byte(failing["Caddyfile"]), readersFor(failing))
		if err == nil {
			t.Fatal("expected failure")
		}
		if msg == "" {
			msg = err.Error()
		} else if err.Error() != msg {
			t.Fatalf("unstable error text:\n%s\n%s", msg, err.Error())
		}
	}
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
