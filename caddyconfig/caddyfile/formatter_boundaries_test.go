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
	"strings"
	"sync"
	"testing"
)

// bom is a complete UTF-8 byte order mark.
var bom = []byte{0xEF, 0xBB, 0xBF}

func mustEqual(t *testing.T, got, want []byte, what string) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch:\nwant %q\n got %q", what, want, got)
	}
}

func TestFormatLeadingBOM(t *testing.T) {
	// a complete leading BOM is preserved as the file marker, and
	// exactly one BOM sits at the start of the output
	in := append(append([]byte{}, bom...), []byte("a {\n\tb\n}\n")...)
	out := Format(in)
	if !bytes.HasPrefix(out, bom) {
		t.Fatalf("output does not start with BOM: %q", out)
	}
	if bytes.Count(out, bom) != 1 {
		t.Fatalf("expected exactly one BOM, got %d in %q", bytes.Count(out, bom), out)
	}
	mustEqual(t, out, append(append([]byte{}, bom...), []byte("a {\n\tb\n}\n")...), "BOM output")

	// formatting again keeps the single leading BOM (idempotent)
	mustEqual(t, Format(out), out, "BOM idempotency")
}

func TestFormatBOMInContentQuotesAndHeredoc(t *testing.T) {
	// a BOM in ordinary content keeps its bytes
	in := []byte("a \xEF\xBB\xBF b\n")
	if !bytes.Contains(Format(in), bom) {
		t.Errorf("BOM in content was lost: %q", Format(in))
	}

	// a BOM inside a quoted argument is preserved
	in = []byte("respond \"x\xEF\xBB\xBFy\"\n")
	if !bytes.Contains(Format(in), bom) {
		t.Errorf("BOM in quoted arg was lost: %q", Format(in))
	}

	// a BOM inside a heredoc body is preserved byte for byte
	in = []byte("block {\n\theredoc <<E\n\tx\xEF\xBB\xBFy\n\tE\n}\n")
	out := Format(in)
	if !bytes.Contains(out, bom) {
		t.Errorf("BOM in heredoc body was lost: %q", out)
	}

	// two BOMs at the start: one file marker, the other is content;
	// both byte sequences survive
	in = append(append([]byte{}, bom...), append(append([]byte{}, bom...), []byte("a\n")...)...)
	out = Format(in)
	if bytes.Count(out, bom) != 2 {
		t.Errorf("expected 2 BOMs (marker + content), got %d in %q", bytes.Count(out, bom), out)
	}
}

func TestFormatTruncatedLeadingBOM(t *testing.T) {
	for _, in := range [][]byte{
		{0xEF},
		{0xEF, 0xBB},
	} {
		// a truncated leading BOM must come back byte for byte, with no
		// stitching, dropping, rewriting, or appended newline
		out := Format(in)
		mustEqual(t, out, in, "truncated BOM")
		// repeat calls are stable and do not "complete" the BOM over time
		mustEqual(t, Format(out), in, "truncated BOM repeat")
	}
}

func TestFormatLineEndingsInStructuredParts(t *testing.T) {
	want := []byte("a {\n\tb\n}\n")

	for name, in := range map[string][]byte{
		"CRLF":  []byte("a {\r\nb\r\n}\r\n"),
		"CR":    []byte("a {\rb\r}\r"),
		"mixed": []byte("a {\r\nb\n}\r"),
	} {
		mustEqual(t, Format(in), want, name+" line endings")
	}

	// a bare CR is a line ending too (not an inter-token space): all
	// three flavors normalize to LF
	mustEqual(t, Format([]byte("abc\rdef\n")), []byte("abc\ndef\n"), "bare CR between tokens")
}

func TestFormatLineEndingsInQuotesArePreserved(t *testing.T) {
	// inside quotes, CRLF bytes survive verbatim
	in := []byte("respond \"a\r\nb\"\r\n")
	want := []byte("respond \"a\r\nb\"\n")
	mustEqual(t, Format(in), want, "CRLF inside quote")

	// inside backticks too
	in = []byte("respond `a\r\nb`\r\n")
	want = []byte("respond `a\r\nb`\n")
	mustEqual(t, Format(in), want, "CRLF inside backticks")
}

func TestFormatHeredocBodyIsVerbatim(t *testing.T) {
	// spaces, tabs, CRs, CRLFs, braces, and comment-looking bytes in the
	// heredoc body must not be reformatted
	in := []byte("block {\n\theredoc <<E\n" +
		"  keep    spaces\tand\ttabs\r\n" +
		"# not a comment { } }\r\n" +
		"\tE\n" +
		"}\n")
	out := Format(in)
	want := []byte("block {\n\theredoc <<E\n" +
		"  keep    spaces\tand\ttabs\r\n" +
		"# not a comment { } }\r\n" +
		"\tE\n" +
		"}\n")
	mustEqual(t, out, want, "heredoc verbatim body")

	// the closing marker is honored only on a line boundary; marker text
	// embedded in another line is ordinary body
	in = []byte("block {\n\tx <<E\nbody\nxxE\nEmore\nE\n}\n")
	out = Format(in)
	if !bytes.Contains(out, []byte("xxE\nEmore")) {
		t.Errorf("embedded marker text was treated as a closing line: %q", out)
	}

	// a marker followed by trailing spaces or more text is not a closing
	// line; the heredoc stays open until a clean marker line
	in = []byte("block {\n\tx <<E\nbody\n\tE \nmore\n\tE\n}\n")
	out = Format(in)
	if !bytes.Contains(out, []byte("\tE \nmore")) {
		t.Errorf("marker with trailing whitespace wrongly closed the heredoc: %q", out)
	}

	// body indentation and the closing marker indentation must be copied
	// verbatim so their correspondence (and the heredoc value) survives;
	// the historical formatter wrongly de-indented these lines
	in = []byte("block {\n\th <<E\n\t\tEE\n\t\tfoo\n\t\tE\n}\n")
	mustEqual(t, Format(in), in, "heredoc padding correspondence")
	if toks, err := Tokenize(Format(in), "t"); err != nil || len(toks) == 0 {
		t.Fatalf("padded heredoc must stay parseable: err=%v toks=%d", err, len(toks))
	}
}

func TestFormatUnterminatedHeredoc(t *testing.T) {
	in := []byte("block {\n\theredoc <<E\nbody without closing marker\n")
	mustEqual(t, Format(in), in, "unterminated heredoc")
}

func TestFormatMalformedReturnsInputUnchanged(t *testing.T) {
	for name, in := range map[string][]byte{
		"unterminated double quote": []byte("respond \"oops\n"),
		"unterminated backtick":     []byte("respond `oops\n"),
		"dangling escape at EOF":    []byte("foo \\"),
		"too many open braces":      []byte("a {\n\tb\n"),
		"too many close braces":     []byte("a\n}\n"),
		"unterminated heredoc":      []byte("x <<E\nbody\n"),
		"truncated leading BOM":     {0xEF, 0xBB},
		"open quote no newline":     []byte("x \"abc"),
	} {
		out := Format(in)
		mustEqual(t, out, in, name)
		// even a missing trailing newline must not be synthesized
		if len(out) != len(in) {
			t.Errorf("%s: length changed (no partial formatting may leak)", name)
		}
	}
}

func TestFormatQuotedAndTokenBoundaries(t *testing.T) {
	// escaped quotes, empty args, adjacent braces, and import paths keep
	// their byte boundaries; imports/snippets/globs are not expanded
	cases := [][2]string{
		{`"a \"b\" c"`, `"a \"b\" c"` + "\n"},
		{`respond "" 200`, "respond \"\" 200\n"},
		{`a {}`, "a {}\n"},
		{`foo{bar} foo{bar}baz`, "foo{bar} foo{bar}baz\n"},
		{`import ./conf.d/*.caddy`, "import ./conf.d/*.caddy\n"},
		{`import snippet-name`, "import snippet-name\n"},
	}
	for _, c := range cases {
		mustEqual(t, Format([]byte(c[0])), []byte(c[1]), c[0])
	}
}

func TestFormatDoesNotSplitOrMergeDirectives(t *testing.T) {
	// comments and blank lines keep their relative positions; one
	// directive is never split in two or joined with another
	in := []byte("a 1\n\n# comment\nb 2\n")
	want := []byte("a 1\n\n# comment\nb 2\n")
	mustEqual(t, Format(in), want, "directive boundaries")
}

func TestFormatDeterminismAndIdempotency(t *testing.T) {
	inputs := [][]byte{
		[]byte("a {\n\tb\n}\n"),
		append(append([]byte{}, bom...), []byte("x {\r\ny\r\n}\r\n")...),
		[]byte("block {\n\th <<E\r\n  body \r\n\tE\n}\n"),
		{0xEF, 0xBB}, // malformed: stays as-is
		[]byte("respond \"oops\n"),
	}
	for _, in := range inputs {
		first := Format(in)
		// repeated calls over the same input return identical bytes
		for i := 0; i < 5; i++ {
			mustEqual(t, Format(in), first, "determinism")
		}
		// formatting the output is idempotent
		mustEqual(t, Format(first), first, "idempotency")
	}
}

func TestFormatResultDecoupledFromInputBuffer(t *testing.T) {
	in := []byte("abc {\n\tdef\n}\n")
	out := Format(in)

	// mutating the input buffer after the call must not change the result
	want := append([]byte{}, out...)
	for i := range in {
		in[i] = 'X'
	}
	mustEqual(t, out, want, "result after input mutation")

	// reusing the same backing slice across calls stays independent
	buf := make([]byte, 64)
	copy(buf, []byte("a {\n\tb\n}\n"))
	r1 := Format(buf[:len("a {\n\tb\n}\n")])
	for i := range buf {
		buf[i] = 0
	}
	copy(buf, []byte("other {\n\tz\n}\n"))
	r2 := Format(buf[:len("other {\n\tz\n}\n")])
	if bytes.Equal(r1, r2) {
		t.Errorf("results of distinct calls leaked shared state: %q", r1)
	}
}

func TestFormatParallel(t *testing.T) {
	inputs := [][]byte{
		[]byte("a {\n\tb\n}\n"),
		append(append([]byte{}, bom...), []byte("x {\r\ny\r\n}\r\n")...),
		{0xEF, 0xBB},
		[]byte("block {\n\th <<E\nbody\n\tE\n}\n"),
	}
	expected := make([][]byte, len(inputs))
	for i, in := range inputs {
		expected[i] = Format(in)
	}

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 50; round++ {
				for i, in := range inputs {
					mustEqual(t, Format(in), expected[i], "parallel")
				}
			}
		}()
	}
	wg.Wait()
}

// TestFormatTokenizeRoundTrip ensures the formatted bytes remain safe to
// parse after a round trip, including heredoc bodies that carry CR bytes
// and indentation that matches the closing marker.
func TestFormatTokenizeRoundTrip(t *testing.T) {
	inputs := [][]byte{
		[]byte("block {\n\theredoc <<E\n\tbody line\r\n\tE\n\trespond 200\n}\n"),
		append(append([]byte{}, bom...), []byte("block {\n\theredoc <<E\n\tx\r\n\tE\n}\n")...),
		[]byte("a {\r\n\tb\r\n}\r\n"),
	}
	for _, in := range inputs {
		out := Format(in)

		toksBefore, errBefore := Tokenize(in, "Caddyfile")
		toksAfter, errAfter := Tokenize(out, "Caddyfile")
		if errBefore != nil || errAfter != nil {
			t.Fatalf("tokenize error: before=%v after=%v for %q", errBefore, errAfter, out)
		}
		if len(toksBefore) != len(toksAfter) {
			t.Fatalf("token count changed: %d -> %d for %q", len(toksBefore), len(toksAfter), out)
		}
		for i := range toksBefore {
			if toksBefore[i].Text != toksAfter[i].Text {
				t.Errorf("token %d text changed:\nbefore %q\nafter  %q", i, toksBefore[i].Text, toksAfter[i].Text)
			}
		}

		// the formatted output must itself be idempotent and re-parseable
		mustEqual(t, Format(out), out, "round-trip idempotency")
	}
}

func TestFormatBOMDoesNotIntroduceCarriageReturnsInStructuredParts(t *testing.T) {
	out := Format([]byte("a {\n\tb\r\n\tc\r}\n"))
	if strings.Contains(string(out), "\r") {
		t.Errorf("structured output still contains CR: %q", out)
	}
}
