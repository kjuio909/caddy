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

// tokenView is the observable part of a token: callers of
// Tokenize can only see its file, line, and text.
type tokenView struct {
	File string
	Line int
	Text string
}

func viewTokens(tokens []Token) []tokenView {
	views := make([]tokenView, len(tokens))
	for i, t := range tokens {
		views[i] = tokenView{File: t.File, Line: t.Line, Text: t.Text}
	}
	return views
}

// reencode rewrites every LF line ending in s to the given ending.
func reencode(s, lineEnding string) []byte {
	return []byte(strings.ReplaceAll(s, "\n", lineEnding))
}

// TestTokenizeLineEndings ensures LF, CRLF, and bare CR inputs
// tokenize to the exact same tokens in the same order, with no
// carriage return surviving into token text or positions.
func TestTokenizeLineEndings(t *testing.T) {
	lfInputs := []string{
		"host:123 {\n\tdirective\n}\n",
		"a # trailing comment\nb # another\nc",
		"line1\\\nescaped\nline2\n",
		"A \"quoted value with line\nbreak inside\" {\n\tfoobar\n}\n",
		"multiline `backtick\nquoted\n` string\n",
		"heredoc <<EOF\ncontent\nEOF arg\n",
		"skip those\r\nCR characters", // legacy CR already present: mix is still just a line break
	}

	for i, lf := range lfInputs {
		expected := viewTokens(mustTokenize(t, []byte(lf), ""))

		for _, ending := range []string{"\r\n", "\r"} {
			in := reencode(strings.ReplaceAll(lf, "\r\n", "\n"), ending)
			actual := viewTokens(mustTokenize(t, in, ""))
			if len(actual) != len(expected) {
				t.Fatalf("case %d ending %q: expected %d tokens, got %d (%v)",
					i, ending, len(expected), len(actual), actual)
			}
			for j := range expected {
				if actual[j] != expected[j] {
					t.Fatalf("case %d ending %q token %d: expected %+v, got %+v",
						i, ending, j, expected[j], actual[j])
				}
				if strings.ContainsRune(actual[j].Text, '\r') {
					t.Fatalf("case %d ending %q token %d: carriage return left in text %q",
						i, ending, j, actual[j].Text)
				}
			}
		}
	}
}

// TestTokenizeBOM covers the three leading BOM situations and
// interior byte order marks.
func TestTokenizeBOM(t *testing.T) {
	bom := string(utf8BOM)

	t.Run("leading BOM consumed", func(t *testing.T) {
		tokens := mustTokenize(t, []byte(bom+":8080"), "Caddyfile")
		got := viewTokens(tokens)
		want := []tokenView{{File: "Caddyfile", Line: 1, Text: ":8080"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	})

	t.Run("truncated leading BOM is an error", func(t *testing.T) {
		for _, in := range [][]byte{{0xEF}, {0xEF, 0xBB}} {
			tokens, err := Tokenize(in, "Caddyfile")
			if err == nil {
				t.Fatalf("expected error for % X, got tokens %v", in, tokens)
			}
			if tokens != nil {
				t.Fatalf("expected nil tokens with error, got %v", tokens)
			}
			if want := "Caddyfile:1:1: incomplete UTF-8 byte order mark at beginning of file"; err.Error() != want {
				t.Fatalf("expected %q, got %q", want, err)
			}
		}
	})

	t.Run("EF BB not followed by BF is ordinary content", func(t *testing.T) {
		// 0xEF 0xBB 0xBD cannot be a BOM, so the bytes are
		// read as ordinary (invalid) content, not rejected.
		tokens := mustTokenize(t, []byte{0xEF, 0xBB, 0xBD}, "")
		if len(tokens) != 1 || tokens[0].Text != string([]byte{0xEF, 0xBB, 0xBD}) {
			t.Fatalf("expected bytes preserved as content, got %q", tokens)
		}
	})

	t.Run("interior BOM preserved", func(t *testing.T) {
		// U+FEFF is not whitespace, so adjacent bytes are
		// simply part of the surrounding token
		tokens := mustTokenize(t, []byte("a"+bom+"b"), "")
		if len(tokens) != 1 || tokens[0].Text != "a"+bom+"b" {
			t.Fatalf("expected interior BOM kept in token, got %q", tokens)
		}

		// surrounded by spaces it forms its own token
		tokens = mustTokenize(t, []byte("x "+bom+" y"), "")
		got := viewTokens(tokens)
		want := []tokenView{
			{Line: 1, Text: "x"},
			{Line: 1, Text: bom},
			{Line: 1, Text: "y"},
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 tokens, got %v", got)
		}
		for i := range want {
			if got[i].Line != want[i].Line || got[i].Text != want[i].Text {
				t.Fatalf("token %d: expected %q line %d, got %q line %d",
					i, want[i].Text, want[i].Line, got[i].Text, got[i].Line)
			}
		}
	})
}

// TestTokenizeTokenBoundaries pins the word-level rules that
// newline normalization must not change: quoting keeps spaces,
// escapes, adjacent braces, and empty args together, and '#'
// only starts a comment at a lexical boundary.
func TestTokenizeTokenBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		wantText []string
	}{
		{
			name:     "adjacent braces stay attached",
			input:    "host:123 { directive }",
			wantText: []string{"host:123", "{", "directive", "}"},
		},
		{
			name:     "no space around braces keeps one token",
			input:    "a{b}c",
			wantText: []string{"a{b}c"},
		},
		{
			name:     "empty quoted argument",
			input:    "empty \"\" and `` string",
			wantText: []string{"empty", "", "and", "", "string"},
		},
		{
			name:     "spaces and escapes inside quotes",
			input:    `A "quoted \"value\" inside" B`,
			wantText: []string{"A", `quoted "value" inside`, "B"},
		},
		{
			name:     "hash in token is not a comment",
			input:    "redir / /some/#/path",
			wantText: []string{"redir", "/", "/some/#/path"},
		},
		{
			name:     "hash in quotes is not a comment",
			input:    `x "# not a comment" y`,
			wantText: []string{"x", "# not a comment", "y"},
		},
		{
			name:     "escaped hash is not a comment",
			input:    `foo \#bar # real comment`,
			wantText: []string{"foo", `\#bar`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n", "\r"} {
				in := reencode(tc.input, ending)
				tokens := mustTokenize(t, in, "")
				if len(tokens) != len(tc.wantText) {
					t.Fatalf("ending %q: expected %v, got %v", ending, tc.wantText, tokens)
				}
				for i, want := range tc.wantText {
					if tokens[i].Text != want {
						t.Fatalf("ending %q token %d: expected %q, got %q",
							ending, i, want, tokens[i].Text)
					}
				}
			}
		})
	}
}

// TestTokenizeErrors verifies structural errors carry the
// caller-supplied filename and a definite line and column,
// return no traversable tokens, and position identically
// regardless of line ending encoding.
func TestTokenizeErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "unclosed double quote",
			input:   "ab \"x",
			wantErr: "test/Caddyfile:1:4: unclosed quoted string: missing closing '\"'",
		},
		{
			name:    "unclosed double quote on later line",
			input:   "ab\n \"x",
			wantErr: "test/Caddyfile:2:2: unclosed quoted string: missing closing '\"'",
		},
		{
			name:    "unclosed quote followed by dangling escape",
			input:   "\"\\",
			wantErr: "test/Caddyfile:1:1: unclosed quoted string: missing closing '\"'",
		},
		{
			name:    "unclosed backtick",
			input:   "x `abc",
			wantErr: "test/Caddyfile:1:3: unclosed quoted string: missing closing '`'",
		},
		{
			name:    "dangling backslash",
			input:   "foo bar\\",
			wantErr: "test/Caddyfile:1:8: incomplete escape: dangling backslash at end of input",
		},
		{
			name:    "dangling backslash on later line",
			input:   "a\nb\\",
			wantErr: "test/Caddyfile:2:2: incomplete escape: dangling backslash at end of input",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n", "\r"} {
				in := reencode(tc.input, ending)
				tokens, err := Tokenize(in, "test/Caddyfile")
				if err == nil {
					t.Fatalf("ending %q: expected error, got tokens %v", ending, tokens)
				}
				if tokens != nil {
					t.Fatalf("ending %q: error must not return tokens, got %v", ending, tokens)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("ending %q: expected error %q, got %q", ending, tc.wantErr, err)
				}
			}
		})
	}

	t.Run("no filename", func(t *testing.T) {
		_, err := Tokenize([]byte("ab\n \"x"), "")
		want := "line 2, column 2: unclosed quoted string: missing closing '\"'"
		if err == nil || err.Error() != want {
			t.Fatalf("expected %q, got %v", want, err)
		}
	})
}

// TestTokenizeIsolation ensures successful results are independent
// of the caller's buffer, and that a previous failure, buffer reuse,
// concurrent use, or repeated calls cannot leak state.
func TestTokenizeIsolation(t *testing.T) {
	input := []byte("host:123 {\n\tdirective arg\n}\n")
	want := viewTokens(mustTokenize(t, input, "isolated/Caddyfile"))

	t.Run("mutating input after success", func(t *testing.T) {
		buf := bytes.Clone(input)
		tokens, err := Tokenize(buf, "isolated/Caddyfile")
		if err != nil {
			t.Fatal(err)
		}
		for i := range buf {
			buf[i] = 'X'
		}
		if got := viewTokens(tokens); !equalViews(got, want) {
			t.Fatalf("tokens changed after input mutation: %v", got)
		}
	})

	t.Run("valid text immediately after failure on reused buffer", func(t *testing.T) {
		buf := make([]byte, 64)
		bad := []byte("a \"unclosed")
		copy(buf, bad)
		if tokens, err := Tokenize(buf[:len(bad)], "reused"); err == nil || tokens != nil {
			t.Fatalf("expected failure first, got tokens=%v err=%v", tokens, err)
		}

		// overwrite the same backing array with valid bytes
		for i := range buf {
			buf[i] = 0
		}
		good := []byte("ok now")
		copy(buf, good)
		tokens, err := Tokenize(buf[:len(good)], "reused")
		if err != nil {
			t.Fatalf("expected success after prior failure, got %v", err)
		}
		gotTexts := []string{"ok", "now"}
		if len(tokens) != len(gotTexts) {
			t.Fatalf("expected %v, got %v", gotTexts, tokens)
		}
		for i := range gotTexts {
			if tokens[i].Text != gotTexts[i] {
				t.Fatalf("token %d: expected %q, got %q", i, gotTexts[i], tokens[i].Text)
			}
		}
	})

	t.Run("repeated and parallel calls on the same input", func(t *testing.T) {
		snapshot := func() []tokenView {
			return viewTokens(mustTokenize(t, input, "isolated/Caddyfile"))
		}
		first := snapshot()
		if !equalViews(first, want) {
			t.Fatalf("first call mismatch: %v", first)
		}
		for i := 0; i < 10; i++ {
			if got := snapshot(); !equalViews(got, first) {
				t.Fatalf("repeat %d mismatch: %v", i, got)
			}
		}

		const n = 32
		var wg sync.WaitGroup
		results := make([][]tokenView, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				results[i] = viewTokens(mustTokenize(t, input, "isolated/Caddyfile"))
			}(i)
		}
		wg.Wait()
		for i, got := range results {
			if !equalViews(got, first) {
				t.Fatalf("parallel call %d mismatch: %v", i, got)
			}
		}
	})

	t.Run("parallel failures do not leak tokens", func(t *testing.T) {
		const n = 32
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				tokens, err := Tokenize([]byte("a \"oops"), "parallel")
				if err == nil {
					t.Errorf("expected error")
				}
				if tokens != nil {
					t.Errorf("expected nil tokens, got %v", tokens)
				}
			}()
		}
		wg.Wait()
	})
}

// TestTokenizeEmptyInput confirms empty input yields no tokens
// and no error (the historical io.EOF surface is normalized away).
func TestTokenizeEmptyInput(t *testing.T) {
	for _, in := range [][]byte{nil, {}} {
		tokens, err := Tokenize(in, "empty")
		if err != nil {
			t.Fatalf("empty input: expected no error, got %v", err)
		}
		if len(tokens) != 0 {
			t.Fatalf("empty input: expected no tokens, got %v", tokens)
		}
	}
}

func mustTokenize(t *testing.T, input []byte, filename string) []Token {
	t.Helper()
	tokens, err := Tokenize(input, filename)
	if err != nil {
		t.Fatalf("unexpected tokenize error for %q: %v", input, err)
	}
	return tokens
}

func equalViews(a, b []tokenView) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
