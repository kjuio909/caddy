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
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type (
	// lexer is a utility which can get values, token by
	// token, from an input byte slice. A token is a word,
	// and tokens are separated by whitespace. A word can
	// be enclosed in quotes if it contains whitespace.
	lexer struct {
		// src is scanned positionally and never mutated;
		// every invocation of Tokenize gets a fresh lexer.
		src []byte
		// pos is the byte offset of the next rune to read.
		pos int

		// line is the logical line counter used for token
		// Line values; it preserves the historical behavior
		// where escaped line continuations do not advance it.
		line         int
		skippedLines int

		// physLine and physCol are the physical 1-based
		// coordinates of the next rune in src, counting
		// CRLF, a bare CR, and LF each as a single line
		// break. They are used to report error positions.
		physLine int
		physCol  int

		file  string
		token Token
	}

	// Token represents a single parsable unit.
	Token struct {
		File          string
		imports       []string
		Line          int
		Text          string
		wasQuoted     rune // enclosing quote character, if any
		heredocMarker string
		snippetName   string
	}
)

// utf8BOM is the UTF-8 encoding of U+FEFF, the byte order mark.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Tokenize takes bytes as input and lexes it into
// a list of tokens that can be parsed as a Caddyfile.
// Also takes a filename to fill the token's File as
// the source of the tokens, which is important to
// determine relative paths for `import` directives.
//
// To produce stable results across platforms and
// editors, a single leading UTF-8 byte order mark is
// consumed as a file marker, CRLF and bare CR line
// endings are treated exactly like LF, and lexical
// errors are reported with the given filename and a
// definite line and column. The returned tokens do not
// alias input, so mutating the input slice afterwards
// has no effect on them.
func Tokenize(input []byte, filename string) ([]Token, error) {
	src := input

	// A byte order mark is only special in the leading
	// position. A complete one is consumed silently; one
	// cut off at end of input is malformed input; anywhere
	// else in the file U+FEFF (or an unrelated EF-prefixed
	// byte sequence) is ordinary content.
	if len(src) > 0 && src[0] == utf8BOM[0] {
		switch {
		case len(src) >= len(utf8BOM) && src[1] == utf8BOM[1] && src[2] == utf8BOM[2]:
			src = src[len(utf8BOM):]
		case (len(src) == 1) || (len(src) == 2 && src[1] == utf8BOM[1]):
			l := &lexer{file: filename}
			return nil, l.errorf(1, 1, "incomplete UTF-8 byte order mark at beginning of file")
		}
	}

	l := &lexer{
		src:      src,
		line:     1,
		physLine: 1,
		physCol:  1,
		file:     filename,
	}

	var tokens []Token
	for {
		found, err := l.next()
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		l.token.File = filename
		tokens = append(tokens, l.token)
	}
	return tokens, nil
}

// read decodes the next rune from the input and returns
// it along with its physical 1-based line and column.
// CRLF, a bare CR, and LF are all normalized to a single
// '\n', so no carriage return ever reaches token text or
// position accounting. Invalid bytes decode to
// utf8.RuneError, matching utf8.DecodeRune. The boolean
// result is false at end of input.
func (l *lexer) read() (rune, int, int, bool) {
	if l.pos >= len(l.src) {
		return 0, l.physLine, l.physCol, false
	}
	line, col := l.physLine, l.physCol

	if l.src[l.pos] == '\r' {
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '\n' {
			l.pos++
		}
		l.physLine++
		l.physCol = 1
		return '\n', line, col, true
	}

	ch, size := utf8.DecodeRune(l.src[l.pos:])
	l.pos += size
	if ch == '\n' {
		l.physLine++
		l.physCol = 1
	} else {
		l.physCol++
	}
	return ch, line, col, true
}

// errorf reports a lexical error at the given physical
// position, prefixed with the filename as provided by the
// caller (never the working directory).
func (l *lexer) errorf(line, col int, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if l.file != "" {
		return fmt.Errorf("%s:%d:%d: %s", l.file, line, col, message)
	}
	return fmt.Errorf("line %d, column %d: %s", line, col, message)
}

// next loads the next token into the lexer.
// A token is delimited by whitespace, unless
// the token starts with a quotes character (")
// in which case the token goes until the closing
// quotes (the enclosing quotes are not included).
// Inside quoted strings, quotes may be escaped
// with a preceding \ character. No other chars
// may be escaped. The rest of the line is skipped
// if a "#" character is read in. Returns true if
// a token was loaded; false otherwise.
func (l *lexer) next() (bool, error) {
	var val []rune
	var comment, quoted, btQuoted, inHeredoc, heredocEscaped, escaped bool
	var heredocMarker string

	// startLine is the logical line where the current
	// token began; tokLine/tokCol are that position in
	// physical coordinates, for unclosed-quote errors.
	// escLine/escCol locate a pending backslash.
	var startLine, tokLine, tokCol, escLine, escCol int

	makeToken := func(quoted rune) bool {
		l.token.Text = string(val)
		l.token.Line = startLine
		l.token.wasQuoted = quoted
		l.token.heredocMarker = heredocMarker
		return true
	}

	for {
		// Read a character in; if err then if we had
		// read some characters, make a token. If we
		// reached EOF, then no more tokens to read.
		// If no EOF, then we had a problem.
		ch, line, col, ok := l.read()
		if !ok {
			// structural errors are reported at the position
			// where they began, and no partial token is
			// returned alongside the error
			if quoted {
				return false, l.errorf(tokLine, tokCol, "unclosed quoted string: missing closing '\"'")
			}
			if btQuoted {
				return false, l.errorf(tokLine, tokCol, "unclosed quoted string: missing closing '`'")
			}
			if escaped {
				return false, l.errorf(escLine, escCol, "incomplete escape: dangling backslash at end of input")
			}
			if inHeredoc {
				return false, fmt.Errorf("incomplete heredoc <<%s on line #%d, expected ending marker %s", heredocMarker, l.line+l.skippedLines, heredocMarker)
			}

			if len(val) > 0 {
				return makeToken(0), nil
			}
			return false, nil
		}

		// detect whether we have the start of a heredoc
		if (!quoted && !btQuoted) && (!inHeredoc && !heredocEscaped) &&
			len(val) > 1 && string(val[:2]) == "<<" {
			// a space means it's just a regular token and not a heredoc
			if ch == ' ' {
				return makeToken(0), nil
			}

			// after hitting a newline, we know that the heredoc marker
			// is the characters after the two << and the newline.
			// we reset the val because the heredoc is syntax we don't
			// want to keep.
			// (CR and CRLF have already been normalized to '\n'.)
			if ch == '\n' {
				if len(val) == 2 {
					return false, fmt.Errorf("missing opening heredoc marker on line #%d; must contain only alphanumeric characters, dashes and underscores; got empty string", l.line)
				}

				// check if there's too many <
				if string(val[:3]) == "<<<" {
					return false, fmt.Errorf("too many '<' for heredoc on line #%d; only use two, for example <<END", l.line)
				}

				heredocMarker = string(val[2:])
				if !heredocMarkerRegexp.MatchString(heredocMarker) {
					return false, fmt.Errorf("heredoc marker on line #%d must contain only alphanumeric characters, dashes and underscores; got '%s'", l.line, heredocMarker)
				}

				inHeredoc = true
				l.skippedLines++
				val = nil
				continue
			}
			val = append(val, ch)
			continue
		}

		// if we're in a heredoc, all characters are read as-is
		if inHeredoc {
			val = append(val, ch)

			if ch == '\n' {
				l.skippedLines++
			}

			// check if we're done, i.e. that the last few characters are the marker
			if len(val) >= len(heredocMarker) && heredocMarker == string(val[len(val)-len(heredocMarker):]) {
				// set the final value
				finalVal, err := l.finalizeHeredoc(val, heredocMarker)
				if err != nil {
					return false, err
				}
				val = finalVal

				// set the line counter, and make the token
				l.line += l.skippedLines
				l.skippedLines = 0
				return makeToken('<'), nil
			}

			// stay in the heredoc until we find the ending marker
			continue
		}

		// track whether we found an escape '\' for the next
		// iteration to be contextually aware
		if !escaped && !btQuoted && ch == '\\' {
			if len(val) == 0 && !quoted {
				startLine = l.line
				tokLine, tokCol = line, col
			}
			escaped = true
			escLine, escCol = line, col
			continue
		}

		if quoted || btQuoted {
			if quoted && escaped {
				// all is literal in quoted area,
				// so only escape quotes
				if ch != '"' {
					val = append(val, '\\')
				}
				escaped = false
			} else {
				if (quoted && ch == '"') || (btQuoted && ch == '`') {
					return makeToken(ch), nil
				}
			}
			// allow quoted text to wrap continue on multiple lines
			if ch == '\n' {
				l.line += 1 + l.skippedLines
				l.skippedLines = 0
			}
			// collect this character as part of the quoted token;
			// line endings are already normalized, so quoted text
			// is identical across LF, CRLF, and bare CR inputs
			val = append(val, ch)
			continue
		}

		if unicode.IsSpace(ch) {
			// end of the line (carriage returns are normalized
			// away at read time, so every line break is '\n')
			if ch == '\n' {
				// newlines can be escaped to chain arguments
				// onto multiple lines; else, increment the line count
				if escaped {
					l.skippedLines++
					escaped = false
				} else {
					l.line += 1 + l.skippedLines
					l.skippedLines = 0
				}
				// comments (#) are single-line only
				comment = false
			}
			// any kind of space means we're at the end of this token
			if len(val) > 0 {
				return makeToken(0), nil
			}
			continue
		}

		// comments must be at the start of a token,
		// in other words, preceded by space or newline;
		// a quoted or backslash-escaped '#' is content
		if ch == '#' && len(val) == 0 && !escaped {
			comment = true
		}
		if comment {
			continue
		}

		if len(val) == 0 {
			startLine = l.line
			tokLine, tokCol = line, col
			if ch == '"' {
				quoted = true
				continue
			}
			if ch == '`' {
				btQuoted = true
				continue
			}
		}

		if escaped {
			// allow escaping the first < to skip the heredoc syntax
			if ch == '<' {
				heredocEscaped = true
			} else {
				val = append(val, '\\')
			}
			escaped = false
		}

		val = append(val, ch)
	}
}

// finalizeHeredoc takes the runes read as the heredoc text and the marker,
// and processes the text to strip leading whitespace, returning the final
// value without the leading whitespace.
func (l *lexer) finalizeHeredoc(val []rune, marker string) ([]rune, error) {
	stringVal := string(val)

	// find the last newline of the heredoc, which is where the contents end
	lastNewline := strings.LastIndex(stringVal, "\n")

	// collapse the content, then split into separate lines
	lines := strings.Split(stringVal[:lastNewline+1], "\n")

	// figure out how much whitespace we need to strip from the front of every
	// line by getting the string that precedes the marker, on the last line
	paddingToStrip := stringVal[lastNewline+1 : len(stringVal)-len(marker)]

	// iterate over each line and strip the whitespace from the front
	var out string
	for lineNum, lineText := range lines[:len(lines)-1] {
		if lineText == "" || lineText == "\r" {
			out += "\n"
			continue
		}

		// find an exact match for the padding
		index := strings.Index(lineText, paddingToStrip)

		// if the padding doesn't match exactly at the start then we can't safely strip
		if index != 0 {
			cleanLineText := strings.TrimRight(lineText, "\r\n")
			return nil, fmt.Errorf("mismatched leading whitespace in heredoc <<%s on line #%d [%s], expected whitespace [%s] to match the closing marker", marker, l.line+lineNum+1, cleanLineText, paddingToStrip)
		}

		// strip, then append the line, with the newline, to the output.
		// also removes all "\r" because Windows.
		out += strings.ReplaceAll(lineText[len(paddingToStrip):]+"\n", "\r", "")
	}

	// Remove the trailing newline from the loop
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}

	// return the final value
	return []rune(out), nil
}

// Quoted returns true if the token was enclosed in quotes
// (i.e. double quotes, backticks, or heredoc).
func (t Token) Quoted() bool {
	return t.wasQuoted > 0
}

// NumLineBreaks counts how many line breaks are in the token text.
func (t Token) NumLineBreaks() int {
	lineBreaks := strings.Count(t.Text, "\n")
	if t.wasQuoted == '<' {
		// heredocs have an extra linebreak because the opening
		// delimiter is on its own line and is not included in the
		// token Text itself, and the trailing newline is removed.
		lineBreaks += 2
	}
	return lineBreaks
}

// Clone returns a deep copy of the token.
func (t Token) Clone() Token {
	return Token{
		File:          t.File,
		imports:       append([]string{}, t.imports...),
		Line:          t.Line,
		Text:          t.Text,
		wasQuoted:     t.wasQuoted,
		heredocMarker: t.heredocMarker,
		snippetName:   t.snippetName,
	}
}

var heredocMarkerRegexp = regexp.MustCompile("^[A-Za-z0-9_-]+$")

// isNextOnNewLine tests whether t2 is on a different line from t1
func isNextOnNewLine(t1, t2 Token) bool {
	// If the second token is from a different file,
	// we can assume it's from a different line
	if t1.File != t2.File {
		return true
	}

	// If the second token is from a different import chain,
	// we can assume it's from a different line
	if len(t1.imports) != len(t2.imports) {
		return true
	}
	for i, im := range t1.imports {
		if im != t2.imports[i] {
			return true
		}
	}

	// If the first token (incl line breaks) ends
	// on a line earlier than the next token,
	// then the second token is on a new line
	return t1.Line+t1.NumLineBreaks() < t2.Line
}
