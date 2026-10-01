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
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// bom is a complete UTF-8 byte order mark.
var bom = []byte{0xEF, 0xBB, 0xBF}

func TestFormatByteBoundaries(t *testing.T) {
	for i, tc := range []struct {
		description string
		input       []byte
		expect      []byte
	}{
		// ---- byte order mark ---------------------------------------
		{
			description: "single leading BOM is preserved as file marker",
			input:       concat(bom, []byte("a{\n\tb\n}\n")),
			expect:      concat(bom, []byte("a {\n\tb\n}\n")),
		},
		{
			description: "leading BOM preserved with CRLF input",
			input:       concat(bom, []byte("a {\r\n\tb\r\n}\r\n")),
			expect:      concat(bom, []byte("a {\n\tb\n}\n")),
		},
		{
			description: "BOM appears only once even when followed by content that formats",
			input:       concat(bom, []byte("  x  \n")),
			expect:      concat(bom, []byte("x\n")),
		},
		{
			description: "BOM in ordinary content is kept verbatim",
			input:       []byte("x " + "\uFEFF" + " y\n"),
			expect:      []byte("x " + "\uFEFF" + " y\n"),
		},
		{
			description: "BOM inside a quoted argument is kept verbatim",
			input:       []byte("respond \"\uFEFF\"\n"),
			expect:      []byte("respond \"\uFEFF\"\n"),
		},
		{
			description: "BOM inside a heredoc body is kept verbatim",
			input:       []byte("h <<E\nbody\uFEFFmore\nE\n"),
			expect:      []byte("h <<E\nbody\uFEFFmore\nE\n"),
		},
		{
			description: "truncated BOM of two bytes is not stitched or dropped",
			input:       []byte{0xEF, 0xBB, 'a', '\n'},
			expect:      []byte{0xEF, 0xBB, 'a', '\n'},
		},
		{
			description: "truncated BOM of one byte is not stitched or dropped",
			input:       []byte{0xEF, 'a', '\n'},
			expect:      []byte{0xEF, 'a', '\n'},
		},
		{
			description: "second BOM is content, not another file marker",
			input:       concat(bom, bom, []byte("a\n")),
			expect:      concat(bom, bom, []byte("a\n")),
		},

		// ---- line endings ------------------------------------------
		{
			description: "CRLF structural lines normalized to LF",
			input:       []byte("a {\r\n\tb\r\n}\r\n"),
			expect:      []byte("a {\n\tb\n}\n"),
		},
		{
			description: "bare CR structural lines normalized to LF",
			input:       []byte("a {\r\tb\r}\r"),
			expect:      []byte("a {\n\tb\n}\n"),
		},
		{
			description: "mixed LF, CRLF and CR structural lines normalized to LF",
			input:       []byte("a {\n\tb\r\n\tc\rd\n}\n"),
			expect:      []byte("a {\n\tb\n\tc\n\td\n}\n"),
		},
		{
			description: "CR inside a comment is a line boundary",
			input:       []byte("# c\r\nb\r\n"),
			expect:      []byte("# c\nb\n"),
		},
		{
			description: "CRLF inside a quoted argument is preserved",
			input:       []byte("respond \"a\r\nb\"\n"),
			expect:      []byte("respond \"a\r\nb\"\n"),
		},
		{
			description: "bare CR inside a quoted argument is preserved",
			input:       []byte("respond \"a\rb\"\n"),
			expect:      []byte("respond \"a\rb\"\n"),
		},
		{
			description: "CRLF on the heredoc opening marker line is structural",
			input:       []byte("h <<E\r\nline\r\nE\r\n"),
			expect:      []byte("h <<E\nline\r\nE\n"),
		},

		// ---- heredoc bodies ----------------------------------------
		{
			description: "heredoc body keeps spaces, tabs, CR, braces and comment bytes",
			input:       []byte("block {\n\th <<E\n  two  sp\ttab\rCR { } # hash\nE\n}\n"),
			expect:      []byte("block {\n\th <<E\n  two  sp\ttab\rCR { } # hash\nE\n}\n"),
		},
		{
			description: "multiple heredocs keep independent bodies",
			input:       []byte("a {\n\th <<E\nl 1\r\nE\n\th2 <<F\nx\ty\nF\n}\n"),
			expect:      []byte("a {\n\th <<E\nl 1\r\nE\n\th2 <<F\nx\ty\nF\n}\n"),
		},
		{
			description: "structural formatting resumes after heredoc",
			input:       []byte("block {\n\th <<E\nbody\nE\n\trespond \"x\"   200\n}\n"),
			expect:      []byte("block {\n\th <<E\nbody\nE\n\trespond \"x\" 200\n}\n"),
		},
		{
			description: "indented closing marker keeps padding correspondence",
			input:       []byte("block {\n\th <<E\n\tbody\n\tE\n}\n"),
			expect:      []byte("block {\n\th <<E\n\tbody\n\tE\n}\n"),
		},
		{
			description: "marker only on a final line without boundary does not close",
			input:       []byte("h <<E\nbody\nE"),
			expect:      []byte("h <<E\nbody\nE"),
		},
		{
			description: "text that merely ends with marker letters is not a closing marker",
			input:       []byte("h <<E\nbody\nEX\nE\n"),
			expect:      []byte("h <<E\nbody\nEX\nE\n"),
		},
		{
			description: "escaped heredoc is a literal token and stays on one line",
			input:       []byte("block {\n\theredoc \\<<X\n\trespond \"x\"     200\n}\n"),
			expect:      []byte("block {\n\theredoc \\<<X\n\trespond \"x\" 200\n}\n"),
		},

		// ---- quoted / escaped byte boundaries -----------------------
		{
			description: "escaped backslash preserved",
			input:       []byte("foo \\\"literal\\\"\n"),
			expect:      []byte("foo \\\"literal\\\"\n"),
		},
		{
			description: "empty quoted argument preserved",
			input:       []byte("respond \"\" 200\n"),
			expect:      []byte("respond \"\" 200\n"),
		},
		{
			description: "adjacent braces that form valid placeholders stay inline",
			input:       []byte("a{}{}\n"),
			expect:      []byte("a{}{}\n"),
		},
		{
			description: "multiline quoted argument keeps its bytes",
			input:       []byte("i {\n\"foo\nbar\"\n}\n"),
			expect:      []byte("i {\n\t\"foo\nbar\"\n}\n"),
		},

		// ---- imports, snippets, wildcards --------------------------
		{
			description: "import paths are not expanded or reordered",
			input:       []byte("import ./b.caddy\nimport ./a.caddy\n"),
			expect:      []byte("import ./b.caddy\nimport ./a.caddy\n"),
		},
		{
			description: "wildcard import text is not expanded",
			input:       []byte("import ./conf.d/*.caddy\n"),
			expect:      []byte("import ./conf.d/*.caddy\n"),
		},
		{
			description: "snippet definition and expansion text untouched",
			input:       []byte("(mysnippet) {\n\trespond \"hi\"\n}\n\nimport mysnippet\n"),
			expect:      []byte("(mysnippet) {\n\trespond \"hi\"\n}\n\nimport mysnippet\n"),
		},

		// ---- comments and blank lines ------------------------------
		{
			description: "comments and blank lines keep relative positions",
			input:       []byte("a {\n  # one\n\n  b\n  # two\n}\n"),
			expect:      []byte("a {\n\t# one\n\n\tb\n\t# two\n}\n"),
		},
		{
			description: "hash inside a token is not a comment",
			input:       []byte("redir / /some/#/path\n"),
			expect:      []byte("redir / /some/#/path\n"),
		},
	} {
		actual := Format(append([]byte(nil), tc.input...))
		if !bytes.Equal(actual, tc.expect) {
			t.Errorf("\n[TEST %d: %s]\nexpected: %q\nactual:   %q",
				i, tc.description, tc.expect, actual)
		}
		// every result in this table is stable under repeated formatting
		if again := Format(append([]byte(nil), actual...)); !bytes.Equal(again, actual) {
			t.Errorf("\n[TEST %d: %s] not idempotent\nfirst:  %q\nsecond: %q",
				i, tc.description, actual, again)
		}
	}
}

func TestFormatMalformedPassthrough(t *testing.T) {
	for i, tc := range []struct {
		description string
		input       []byte
	}{
		{description: "unclosed double quote", input: []byte("respond \"oops\n")},
		{description: "unclosed backtick quote", input: []byte("x `bt\n")},
		{description: "dangling escape at end of input", input: []byte("foo \\")},
		{description: "unclosed structural brace", input: []byte("a {\n\tb\n")},
		{description: "extra closing brace", input: []byte("a {\n\tb\n}\n}\n")},
		{description: "unterminated heredoc", input: []byte("h <<E\nbody\n")},
		{description: "heredoc marker without line boundary", input: []byte("h <<E\nbody\nE")},
		{description: "empty heredoc marker", input: []byte("h <<\nbody\n")},
		{description: "invalid heredoc marker characters", input: []byte("h <<!\nbody\n!\n")},
		{description: "adjacent structural open braces", input: []byte("_ {{ }\n")},
		{description: "truncated leading BOM", input: []byte{0xEF, 0xBB, 'a'}},
		{description: "truncated leading BOM run into a different byte", input: []byte{0xEF, 'a'}},
	} {
		input := append([]byte(nil), tc.input...)
		actual := Format(input)
		if !bytes.Equal(actual, tc.input) {
			t.Errorf("\n[TEST %d: %s] malformed input must be returned byte-for-byte\ninput: %q\nout:   %q",
				i, tc.description, tc.input, actual)
		}
	}
}

func TestFormatDeterminism(t *testing.T) {
	inputs := [][]byte{
		concat(bom, []byte("a{\r\n b\r\n}\r\n")),
		[]byte("block {\n\th <<E\nx  x\r\t{ #\nE\n\tr \"\"  1\n}\n"),
		[]byte("import ./z.caddy\nimport ./a.caddy\n{\n\tdebug\n}\n"),
		[]byte("respond \"unclosed"),
	}
	for i, in := range inputs {
		first := Format(append([]byte(nil), in...))
		for k := 0; k < 20; k++ {
			if got := Format(append([]byte(nil), in...)); !bytes.Equal(got, first) {
				t.Fatalf("input %d not deterministic on iteration %d", i, k)
			}
		}
	}
}

func TestFormatResultDecoupledFromInput(t *testing.T) {
	original := []byte("block {\n\th <<E\nb  b\r\t{ #\nE\n\tr  1\n}\n")
	input := append([]byte(nil), original...)
	result := Format(input)
	snapshot := append([]byte(nil), result...)

	// mutating the input after Format returns must not change the result
	for i := range input {
		input[i] = 'X'
	}
	if !bytes.Equal(result, snapshot) {
		t.Fatalf("result aliases the input buffer:\nresult:   %q\nsnapshot: %q", result, snapshot)
	}

	// reusing the same backing array for new content must not leak state
	reused := input[:0]
	reused = append(reused, []byte("respond \"fresh\" 200\n")...)
	fresh := Format(reused)
	if bytes.Equal(fresh, result) {
		t.Fatalf("state carried over from a previous Format call")
	}
	if string(fresh) != "respond \"fresh\" 200\n" {
		t.Fatalf("unexpected result on reused slice: %q", fresh)
	}
}

func TestFormatParallelSafe(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			in := []byte(fmt.Sprintf("\uFEFFsite%d {\r\n\th <<E\r\nv %d\r\nE\r\n\trespond \"x\"  %d\r\n}\r\n", g, g, g))
			want := Format(append([]byte(nil), in...))
			for k := 0; k < 200; k++ {
				got := Format(append([]byte(nil), in...))
				if !bytes.Equal(got, want) {
					t.Errorf("parallel call diverged for %d/%d", g, k)
					return
				}
				// the result must be safe to format again (idempotent
				// or, for malformed input, identical passthrough)
				if again := Format(append([]byte(nil), got...)); !bytes.Equal(again, got) {
					t.Errorf("parallel result not stable for %d/%d", g, k)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// concat joins byte slices without allocating an alias-prone caller buffer.
func concat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}
