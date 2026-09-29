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
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestLexer(t *testing.T) {
	testCases := []struct {
		input        []byte
		expected     []Token
		expectErr    bool
		errorMessage string
	}{
		{
			input: []byte(`host:123`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
			},
		},
		{
			input: []byte(`host:123

					directive`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
				{Line: 3, Text: "directive"},
			},
		},
		{
			input: []byte(`host:123 {
						directive
					}`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
				{Line: 1, Text: "{"},
				{Line: 2, Text: "directive"},
				{Line: 3, Text: "}"},
			},
		},
		{
			input: []byte(`host:123 { directive }`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
				{Line: 1, Text: "{"},
				{Line: 1, Text: "directive"},
				{Line: 1, Text: "}"},
			},
		},
		{
			input: []byte(`host:123 {
						#comment
						directive
						# comment
						foobar # another comment
					}`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
				{Line: 1, Text: "{"},
				{Line: 3, Text: "directive"},
				{Line: 5, Text: "foobar"},
				{Line: 6, Text: "}"},
			},
		},
		{
			input: []byte(`host:123 {
						# hash inside string is not a comment
						redir / /some/#/path
					}`),
			expected: []Token{
				{Line: 1, Text: "host:123"},
				{Line: 1, Text: "{"},
				{Line: 3, Text: "redir"},
				{Line: 3, Text: "/"},
				{Line: 3, Text: "/some/#/path"},
				{Line: 4, Text: "}"},
			},
		},
		{
			input: []byte("# comment at beginning of file\n# comment at beginning of line\nhost:123"),
			expected: []Token{
				{Line: 3, Text: "host:123"},
			},
		},
		{
			input: []byte(`a "quoted value" b
					foobar`),
			expected: []Token{
				{Line: 1, Text: "a"},
				{Line: 1, Text: "quoted value"},
				{Line: 1, Text: "b"},
				{Line: 2, Text: "foobar"},
			},
		},
		{
			input: []byte(`A "quoted \"value\" inside" B`),
			expected: []Token{
				{Line: 1, Text: "A"},
				{Line: 1, Text: `quoted "value" inside`},
				{Line: 1, Text: "B"},
			},
		},
		{
			input: []byte("An escaped \"newline\\\ninside\" quotes"),
			expected: []Token{
				{Line: 1, Text: "An"},
				{Line: 1, Text: "escaped"},
				{Line: 1, Text: "newline\\\ninside"},
				{Line: 2, Text: "quotes"},
			},
		},
		{
			input: []byte("An escaped newline\\\noutside quotes"),
			expected: []Token{
				{Line: 1, Text: "An"},
				{Line: 1, Text: "escaped"},
				{Line: 1, Text: "newline"},
				{Line: 1, Text: "outside"},
				{Line: 1, Text: "quotes"},
			},
		},
		{
			input: []byte("line1\\\nescaped\nline2\nline3"),
			expected: []Token{
				{Line: 1, Text: "line1"},
				{Line: 1, Text: "escaped"},
				{Line: 3, Text: "line2"},
				{Line: 4, Text: "line3"},
			},
		},
		{
			input: []byte("line1\\\nescaped1\\\nescaped2\nline4\nline5"),
			expected: []Token{
				{Line: 1, Text: "line1"},
				{Line: 1, Text: "escaped1"},
				{Line: 1, Text: "escaped2"},
				{Line: 4, Text: "line4"},
				{Line: 5, Text: "line5"},
			},
		},
		{
			input: []byte(`"unescapable\ in quotes"`),
			expected: []Token{
				{Line: 1, Text: `unescapable\ in quotes`},
			},
		},
		{
			input: []byte(`"don't\escape"`),
			expected: []Token{
				{Line: 1, Text: `don't\escape`},
			},
		},
		{
			input: []byte(`"don't\\escape"`),
			expected: []Token{
				{Line: 1, Text: `don't\\escape`},
			},
		},
		{
			input: []byte(`un\escapable`),
			expected: []Token{
				{Line: 1, Text: `un\escapable`},
			},
		},
		{
			input: []byte(`A "quoted value with line
					break inside" {
						foobar
					}`),
			expected: []Token{
				{Line: 1, Text: "A"},
				{Line: 1, Text: "quoted value with line\n\t\t\t\t\tbreak inside"},
				{Line: 2, Text: "{"},
				{Line: 3, Text: "foobar"},
				{Line: 4, Text: "}"},
			},
		},
		{
			input: []byte(`"C:\php\php-cgi.exe"`),
			expected: []Token{
				{Line: 1, Text: `C:\php\php-cgi.exe`},
			},
		},
		{
			input: []byte(`empty "" string`),
			expected: []Token{
				{Line: 1, Text: `empty`},
				{Line: 1, Text: ``},
				{Line: 1, Text: `string`},
			},
		},
		{
			input: []byte("skip those\r\nCR characters"),
			expected: []Token{
				{Line: 1, Text: "skip"},
				{Line: 1, Text: "those"},
				{Line: 2, Text: "CR"},
				{Line: 2, Text: "characters"},
			},
		},
		{
			input: []byte("\xEF\xBB\xBF:8080"), // test with leading byte order mark
			expected: []Token{
				{Line: 1, Column: 1, Text: ":8080"},
			},
		},
		{
			input: []byte("ab\xEF\xBB\xBFcd"), // a BOM anywhere else is ordinary content
			expected: []Token{
				{Line: 1, Column: 1, Text: "ab\uFEFFcd"},
			},
		},
		{
			input: []byte("a \xEF\xBB\xBF b"),
			expected: []Token{
				{Line: 1, Column: 1, Text: "a"},
				{Line: 1, Column: 3, Text: "\uFEFF"},
				{Line: 1, Column: 5, Text: "b"},
			},
		}, {
			input: []byte("\xEF\xBB\xBF"), // a BOM alone is consumed, yielding no tokens
		},
		{
			input:    []byte{}, // empty input yields an empty token sequence, no error
			expected: []Token{},
		},
		{
			input:    []byte("# a trailing backslash inside a comment is harmless\\\n"),
			expected: []Token{},
		},
		{
			input:        []byte("\xEF"), // truncated leading BOM: first byte only
			expectErr:    true,
			errorMessage: "Caddyfile:1:1: incomplete UTF-8 byte order mark at beginning of file",
		},
		{
			input:        []byte("\xEF\xBB"), // truncated leading BOM: first two bytes
			expectErr:    true,
			errorMessage: "Caddyfile:1:1: incomplete UTF-8 byte order mark at beginning of file",
		},
		{
			input: []byte("a\rb"), // lone CR is a line separator
			expected: []Token{
				{Line: 1, Column: 1, Text: "a"},
				{Line: 2, Column: 1, Text: "b"},
			},
		},
		{
			input: []byte("host:8080 {\r\n\tdirective\r\n}"), // CRLF, just like the LF variant
			expected: []Token{
				{Line: 1, Column: 1, Text: "host:8080"},
				{Line: 1, Column: 11, Text: "{"},
				{Line: 2, Column: 2, Text: "directive"},
				{Line: 3, Column: 1, Text: "}"},
			},
		},
		{
			input: []byte("ab cd\nef ghi"),
			expected: []Token{
				{Line: 1, Column: 1, Text: "ab"},
				{Line: 1, Column: 4, Text: "cd"},
				{Line: 2, Column: 1, Text: "ef"},
				{Line: 2, Column: 4, Text: "ghi"},
			},
		},
		{
			input: []byte(`foo{}`), // adjacent braces stay part of the one unquoted token
			expected: []Token{
				{Line: 1, Column: 1, Text: "foo{}"},
			},
		},
		{
			input: []byte(`"a # b" # real comment`), // '#' inside quotes is not a comment
			expected: []Token{
				{Line: 1, Column: 1, Text: "a # b"},
			},
		},
		{
			input: []byte(`a \#b`), // an escaped '#' is literal text, not a comment
			expected: []Token{
				{Line: 1, Column: 1, Text: "a"},
				{Line: 1, Column: 3, Text: `\#b`},
			},
		},
		{
			input:        []byte(`"abc`), // unclosed double-quoted string
			expectErr:    true,
			errorMessage: `Caddyfile:1:1: unclosed quoted string: missing closing "`,
		},
		{
			input:        []byte("ab\n\"unclosed"), // error points at the opening quote
			expectErr:    true,
			errorMessage: "Caddyfile:2:1: unclosed quoted string: missing closing \"",
		},
		{
			input:        []byte("`unclosed"), // unclosed backtick-quoted string
			expectErr:    true,
			errorMessage: "Caddyfile:1:1: unclosed quoted string: missing closing `",
		},
		{
			input:        []byte(`abc\`), // dangling escape at end of file
			expectErr:    true,
			errorMessage: "Caddyfile:1:4: dangling backslash escape: no character follows",
		},
		{
			input:        []byte("ab\r\n\\"), // dangling escape after a CRLF line ending
			expectErr:    true,
			errorMessage: "Caddyfile:2:1: dangling backslash escape: no character follows",
		},
		{
			input: []byte("simple `backtick quoted` string"),
			expected: []Token{
				{Line: 1, Text: `simple`},
				{Line: 1, Text: `backtick quoted`},
				{Line: 1, Text: `string`},
			},
		},
		{
			input: []byte("multiline `backtick\nquoted\n` string"),
			expected: []Token{
				{Line: 1, Text: `multiline`},
				{Line: 1, Text: "backtick\nquoted\n"},
				{Line: 3, Text: `string`},
			},
		},
		{
			input: []byte("nested `\"quotes inside\" backticks` string"),
			expected: []Token{
				{Line: 1, Text: `nested`},
				{Line: 1, Text: `"quotes inside" backticks`},
				{Line: 1, Text: `string`},
			},
		},
		{
			input: []byte("reverse-nested \"`backticks` inside\" quotes"),
			expected: []Token{
				{Line: 1, Text: `reverse-nested`},
				{Line: 1, Text: "`backticks` inside"},
				{Line: 1, Text: `quotes`},
			},
		},
		{
			input: []byte(`heredoc <<EOF
content
EOF same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: "content"},
				{Line: 3, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<VERY-LONG-MARKER
content
VERY-LONG-MARKER same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: "content"},
				{Line: 3, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<EOF
extra-newline

EOF same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: "extra-newline\n"},
				{Line: 4, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<EOF
EOF
	HERE same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: ``},
				{Line: 3, Text: `HERE`},
				{Line: 3, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<EOF
		EOF same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: ""},
				{Line: 2, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<EOF
	content
	EOF same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: "content"},
				{Line: 3, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`prev-line
	heredoc <<EOF
		multi
		line
		content
	EOF same-line-arg
	next-line
	`),
			expected: []Token{
				{Line: 1, Text: `prev-line`},
				{Line: 2, Text: `heredoc`},
				{Line: 2, Text: "\tmulti\n\tline\n\tcontent"},
				{Line: 6, Text: `same-line-arg`},
				{Line: 7, Text: `next-line`},
			},
		},
		{
			input: []byte(`escaped-heredoc \<< >>`),
			expected: []Token{
				{Line: 1, Text: `escaped-heredoc`},
				{Line: 1, Text: `<<`},
				{Line: 1, Text: `>>`},
			},
		},
		{
			input: []byte(`not-a-heredoc <EOF
	content
	`),
			expected: []Token{
				{Line: 1, Text: `not-a-heredoc`},
				{Line: 1, Text: `<EOF`},
				{Line: 2, Text: `content`},
			},
		},
		{
			input: []byte(`not-a-heredoc <<<EOF content`),
			expected: []Token{
				{Line: 1, Text: `not-a-heredoc`},
				{Line: 1, Text: `<<<EOF`},
				{Line: 1, Text: `content`},
			},
		},
		{
			input: []byte(`not-a-heredoc "<<" ">>"`),
			expected: []Token{
				{Line: 1, Text: `not-a-heredoc`},
				{Line: 1, Text: `<<`},
				{Line: 1, Text: `>>`},
			},
		},
		{
			input: []byte(`not-a-heredoc << >>`),
			expected: []Token{
				{Line: 1, Text: `not-a-heredoc`},
				{Line: 1, Text: `<<`},
				{Line: 1, Text: `>>`},
			},
		},
		{
			input: []byte(`not-a-heredoc <<HERE SAME LINE
	content
	HERE same-line-arg
	`),
			expected: []Token{
				{Line: 1, Text: `not-a-heredoc`},
				{Line: 1, Text: `<<HERE`},
				{Line: 1, Text: `SAME`},
				{Line: 1, Text: `LINE`},
				{Line: 2, Text: `content`},
				{Line: 3, Text: `HERE`},
				{Line: 3, Text: `same-line-arg`},
			},
		},
		{
			input: []byte(`heredoc <<s
			�
			s
	`),
			expected: []Token{
				{Line: 1, Text: `heredoc`},
				{Line: 1, Text: "�"},
			},
		},
		{
			input: []byte("\u000Aheredoc \u003C\u003C\u0073\u0073\u000A\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F\u000A\u0073\u0073\u000A\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F\u000A\u00BF\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F"),
			expected: []Token{
				{
					Line: 2,
					Text: "heredoc",
				},
				{
					Line: 2,
					Text: "\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F",
				},
				{
					Line: 5,
					Text: "\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F",
				},
				{
					Line: 6,
					Text: "\u00BF\u00BF\u0057\u0001\u0000\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u00FF\u003D\u001F",
				},
			},
		},
		{
			input:        []byte("not-a-heredoc <<\n"),
			expectErr:    true,
			errorMessage: "Caddyfile:1:15: missing opening heredoc marker on line #1; must contain only alphanumeric characters, dashes and underscores; got empty string",
		},
		{
			input: []byte(`heredoc <<<EOF
	content
	EOF same-line-arg
	`),
			expectErr:    true,
			errorMessage: "Caddyfile:1:9: too many '<' for heredoc on line #1; only use two, for example <<END",
		},
		{
			input: []byte(`heredoc <<EOF
	content
	`),
			expectErr:    true,
			errorMessage: "Caddyfile:1:9: incomplete heredoc <<EOF on line #3, expected ending marker EOF",
		},
		{
			input: []byte(`heredoc <<EOF
	content
		EOF
	`),
			expectErr:    true,
			errorMessage: "Caddyfile:2:1: mismatched leading whitespace in heredoc <<EOF on line #2 [\tcontent], expected whitespace [\t\t] to match the closing marker",
		},
		{
			input: []byte(`heredoc <<EOF
        content
		EOF
	`),
			expectErr:    true,
			errorMessage: "Caddyfile:2:1: mismatched leading whitespace in heredoc <<EOF on line #2 [        content], expected whitespace [\t\t] to match the closing marker",
		},
		{
			input: []byte(`heredoc <<EOF
The next line is a blank line

The previous line is a blank line
EOF`),
			expected: []Token{
				{Line: 1, Text: "heredoc"},
				{Line: 1, Text: "The next line is a blank line\n\nThe previous line is a blank line"},
			},
		},
		{
			input: []byte(`heredoc <<EOF
	One tab indented heredoc with blank next line

	One tab indented heredoc with blank previous line
	EOF`),
			expected: []Token{
				{Line: 1, Text: "heredoc"},
				{Line: 1, Text: "One tab indented heredoc with blank next line\n\nOne tab indented heredoc with blank previous line"},
			},
		},
		{
			input: []byte(`heredoc <<EOF
The next line is a blank line with one tab
	
The previous line is a blank line with one tab
EOF`),
			expected: []Token{
				{Line: 1, Text: "heredoc"},
				{Line: 1, Text: "The next line is a blank line with one tab\n\t\nThe previous line is a blank line with one tab"},
			},
		},
		{
			input: []byte(`heredoc <<EOF
		The next line is a blank line with one tab less than the correct indentation
	
		The previous line is a blank line with one tab less than the correct indentation
		EOF`),
			expectErr:    true,
			errorMessage: "Caddyfile:3:1: mismatched leading whitespace in heredoc <<EOF on line #3 [\t], expected whitespace [\t\t] to match the closing marker",
		},
	}

	for i, testCase := range testCases {
		actual, err := Tokenize(testCase.input, "Caddyfile")
		if testCase.expectErr {
			if err == nil {
				t.Fatalf("expected error, got actual: %v", actual)
				continue
			}
			if err.Error() != testCase.errorMessage {
				t.Fatalf("expected error '%v', got: %v", testCase.errorMessage, err)
			}
			continue
		}

		if err != nil {
			t.Fatalf("%v", err)
		}
		lexerCompare(t, i, testCase.expected, actual)
	}
}

func lexerCompare(t *testing.T, n int, expected, actual []Token) {
	if len(expected) != len(actual) {
		t.Fatalf("Test case %d: expected %d token(s) but got %d", n, len(expected), len(actual))
	}

	for i := 0; i < len(actual) && i < len(expected); i++ {
		if actual[i].Line != expected[i].Line {
			t.Fatalf("Test case %d token %d ('%s'): expected line %d but was line %d",
				n, i, expected[i].Text, expected[i].Line, actual[i].Line)
			break
		}
		if expected[i].Column != 0 && actual[i].Column != expected[i].Column {
			t.Fatalf("Test case %d token %d ('%s'): expected column %d but was column %d",
				n, i, expected[i].Text, expected[i].Column, actual[i].Column)
			break
		}
		if actual[i].Text != expected[i].Text {
			t.Fatalf("Test case %d token %d: expected text '%s' but was '%s'",
				n, i, expected[i].Text, actual[i].Text)
			break
		}
	}
}

// encodeNewlines returns in with every LF replaced by the given
// line ending, so the same logical text can be lexed under the
// different encodings files acquire across platforms and editors.
func encodeNewlines(in, eol []byte) []byte {
	return bytes.ReplaceAll(in, []byte{'\n'}, eol)
}

// tokenSignature is the observable result of Tokenize: the token
// text, order and positions only; the File is verified separately.
type tokenSignature struct {
	Line   int
	Column int
	Text   string
}

func signatures(tokens []Token) []tokenSignature {
	sig := make([]tokenSignature, len(tokens))
	for i, tok := range tokens {
		sig[i] = tokenSignature{Line: tok.Line, Column: tok.Column, Text: tok.Text}
	}
	return sig
}

// TestLexerLineEndingEquivalence verifies that the same logical
// configuration yields identical token text, order and positions
// whether it uses LF, CRLF, or lone CR line endings, including in
// comments, quoted strings, escaped line continuations and heredocs.
func TestLexerLineEndingEquivalence(t *testing.T) {
	logical := []byte("host:8080 {\n" +
		"  # a comment should be ignored\n" +
		"  directive arg \\\n" +
		"    continued\n" +
		"  quoted \"a b\nc\"\n" +
		"  heredoc <<EOF\n" +
		"  body\n" +
		"  EOF tail\n" +
		"}\n")

	encodings := map[string][]byte{
		"LF":   {'\n'},
		"CRLF": {'\r', '\n'},
		"CR":   {'\r'},
	}

	var reference []tokenSignature
	for name, eol := range encodings {
		input := encodeNewlines(logical, eol)
		tokens, err := Tokenize(input, "Caddyfile")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		for _, tok := range tokens {
			if strings.ContainsRune(tok.Text, '\r') {
				t.Fatalf("%s: token %q retains a carriage return", name, tok.Text)
			}
		}
		sig := signatures(tokens)
		if reference == nil {
			reference = sig
			continue
		}
		if !reflect.DeepEqual(reference, sig) {
			t.Fatalf("%s encoding produced different tokens:\nLF:  %v\n%s: %v", name, reference, name, sig)
		}
	}
}

// TestLexerLeadingBOMNormalized ensures a leading BOM is consumed
// silently under every line ending encoding and never becomes the
// first token, while BOMs elsewhere are preserved verbatim.
func TestLexerLeadingBOMNormalized(t *testing.T) {
	for _, eol := range [][]byte{{'\n'}, {'\r', '\n'}, {'\r'}} {
		input := append([]byte{0xEF, 0xBB, 0xBF}, encodeNewlines([]byte(":8080\nroot"), eol)...)
		tokens, err := Tokenize(input, "Caddyfile")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tokens) != 2 || tokens[0].Text != ":8080" {
			t.Fatalf("leading BOM should be consumed, got %v", tokens)
		}
	}

	// a BOM after other content is ordinary text, even when quoted
	for _, in := range [][]byte{
		[]byte("a \xEF\xBB\xBF b"),
		[]byte("\"\xEF\xBB\xBF\""),
	} {
		tokens, err := Tokenize(in, "Caddyfile")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var found bool
		for _, tok := range tokens {
			if strings.ContainsRune(tok.Text, '\uFEFF') {
				found = true
			}
		}
		if !found {
			t.Fatalf("non-leading BOM must be preserved as content, got %v", tokens)
		}
	}
}

// TestLexerErrorsCarryPositionAndFile checks that lexical failures
// report the filename exactly as passed and the first definite line
// and column, independent of the working directory, and that no
// partial token sequence is returned alongside an error.
func TestLexerErrorsCarryPositionAndFile(t *testing.T) {
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		input   []byte
		file    string
		wantErr string
	}{
		{[]byte(`ab "x`), "relative/Caddyfile", "relative/Caddyfile:1:4: unclosed quoted string: missing closing \""},
		{[]byte("x\n `q"), "/abs/path/Caddyfile", "/abs/path/Caddyfile:2:2: unclosed quoted string: missing closing `"},
		{[]byte("a\\"), "Caddyfile", "Caddyfile:1:2: dangling backslash escape: no character follows"},
		{[]byte("\xEF\xBB"), "Caddyfile", "Caddyfile:1:1: incomplete UTF-8 byte order mark at beginning of file"},
	} {
		tokens, err := Tokenize(tc.input, tc.file)
		if err == nil {
			t.Fatalf("expected error for %q, got tokens %v", tc.input, tokens)
		}
		if err.Error() != tc.wantErr {
			t.Fatalf("expected error %q, got %q", tc.wantErr, err.Error())
		}
		if tokens != nil {
			t.Fatalf("error result must not yield traversable tokens, got %v", tokens)
		}
	}
}

// TestTokenizeBufferIndependence verifies a successful result does
// not alias the caller's input buffer: mutating the input after the
// call leaves the returned tokens untouched.
func TestTokenizeBufferIndependence(t *testing.T) {
	input := []byte("host:8080 {\n\tdirective\n}")
	tokens, err := Tokenize(input, "Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	want := signatures(tokens)

	for i := range input {
		input[i] = 'X'
	}

	tokens2, err := Tokenize([]byte("host:8080 {\n\tdirective\n}"), "Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, signatures(tokens)) {
		t.Fatalf("mutating the input buffer changed earlier results:\n%v", signatures(tokens))
	}
	if !reflect.DeepEqual(want, signatures(tokens2)) {
		t.Fatalf("results are not repeatable across calls:\n%v\n%v", want, signatures(tokens2))
	}
}

// TestTokenizeCallIsolation checks that a failed call leaves no
// residue: immediately tokenizing valid text (even reusing the same
// backing array), then failing again, each produces its own result.
func TestTokenizeCallIsolation(t *testing.T) {
	bad := []byte(`"never closed`)
	if _, err := Tokenize(bad, "Caddyfile"); err == nil {
		t.Fatal("expected an error for unclosed quote")
	}

	// a fresh, independent buffer holding valid input right after
	// the failure must not inherit any state from the failed call
	good := []byte("good input")
	tokens, err := Tokenize(good, "Caddyfile")
	if err != nil {
		t.Fatalf("valid input after a failure should succeed, got: %v", err)
	}
	if len(tokens) != 2 || tokens[0].Text != "good" || tokens[1].Text != "input" {
		t.Fatalf("unexpected tokens after prior failure: %v", tokens)
	}

	// reusing the original failing input repeats the same failure
	if _, err := Tokenize(bad, "Caddyfile"); err == nil {
		t.Fatal("expected the same failure to repeat")
	}

	// and valid input tokenized twice yields identical results
	again, err := Tokenize(good, "Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tokens, again) {
		t.Fatalf("repeated valid calls differ:\n%v\n%v", tokens, again)
	}
}

// TestTokenizeConcurrent runs many goroutines over one shared input
// and asserts every call returns byte-identical tokens or the same
// error, with no shared mutable state leaking between calls.
func TestTokenizeConcurrent(t *testing.T) {
	cases := [][]byte{
		[]byte("host:8080 {\n\tdirective arg\n}\n"),
		[]byte("\"unclosed"),
		[]byte("\xEF\xBB"),
	}
	const n = 64

	for _, input := range cases {
		var wg sync.WaitGroup
		results := make([][]Token, n)
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = Tokenize(input, "Caddyfile")
			}(i)
		}
		close(start)
		wg.Wait()

		for i := 1; i < n; i++ {
			if (errs[0] == nil) != (errs[i] == nil) {
				t.Fatalf("case %q: call 0 erred=%v, call %d erred=%v", input, errs[0], i, errs[i])
			}
			if errs[0] != nil && errs[0].Error() != errs[i].Error() {
				t.Fatalf("case %q: errors differ: %q vs %q", input, errs[0], errs[i])
			}
			if !reflect.DeepEqual(results[0], results[i]) {
				t.Fatalf("case %q: token results differ between concurrent calls", input)
			}
		}
	}
}
