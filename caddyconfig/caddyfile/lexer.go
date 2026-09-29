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
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// utf8BOM is the UTF-8 encoding of U+FEFF, the byte order mark.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

type (
	// lexer is a utility which can get values, token by
	// token, from the normalized input. A token is a word,
	// and tokens are separated by whitespace. A word can be
	// enclosed in quotes if it contains whitespace.
	lexer struct {
		// src is the input decoded into runes with all line
		// endings normalized to LF (and a leading BOM stripped),
		// so the resulting tokens are independent of the line
		// ending encoding of the source file.
		src []rune
		pos int

		file string

		// line is the logical source line, maintained with the
		// historical semantics (escaped newlines and heredoc
		// bodies are only folded in once their token closes);
		// physLine/physCol instead count every consumed rune.
		line         int
		physLine     int
		physCol      int
		skippedLines int

		token Token
	}

	// Token represents a single parsable unit.
	Token struct {
		File string
		// Line is the 1-based logical line of the token, with
		// line continuations and heredoc bodies folded in the
		// same way as the source authoring experience.
		Line int
		// Column is the 1-based column of the token on its
		// line, counting runes and measured against the
		// normalized input (CR LF and lone CR both count as
		// a single line end), so it is stable regardless of
		// line ending encoding.
		Column        int
		imports       []string
		Text          string
		wasQuoted     rune // enclosing quote character, if any
		heredocMarker string
		snippetName   string
	}
)

// Tokenize takes bytes as input and lexes it into
// a list of tokens that can be parsed as a Caddyfile.
// Also takes a filename to fill the token's File as
// the source of the tokens, which is important to
// determine relative paths for `import` directives.
//
// The input is treated as raw file bytes: a leading UTF-8
// byte order mark is consumed as a file marker (it does not
// become a token), and CR LF, lone CR and LF are all treated
// as line separators. After this normalization, token text
// and order are identical no matter which line ending encoding
// was used, and positions are computed from the normalized
// input. The caller's buffer is never retained, so mutating
// it after the call does not affect the returned tokens, and
// concurrent calls share no state.
func Tokenize(input []byte, filename string) ([]Token, error) {
	l := lexer{}
	if err := l.init(input, filename); err != nil {
		return nil, err
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

// init prepares the lexer to scan input for tokens. It strips
// a leading byte order mark, normalizes all line endings to LF,
// and decodes the input into an independent rune slice.
func (l *lexer) init(input []byte, filename string) error {
	l.file = filename
	l.line = 1
	l.physLine = 1
	l.physCol = 1

	// A byte order mark is only special as the very first code
	// point of the file. A full mark there is consumed without
	// becoming a token; a truncated prefix of one is an input
	// error. Anywhere else, U+FEFF is ordinary content.
	switch {
	case len(input) >= 3 && bytes.Equal(input[:3], utf8BOM):
		input = input[3:]
	case len(input) == 2 && input[0] == utf8BOM[0] && input[1] == utf8BOM[1]:
		return l.errorfAt(1, 1, "incomplete UTF-8 byte order mark at beginning of file")
	case len(input) == 1 && input[0] == utf8BOM[0]:
		return l.errorfAt(1, 1, "incomplete UTF-8 byte order mark at beginning of file")
	}

	// Decode up front so the scan never holds onto the caller's
	// buffer, and normalize CR LF and lone CR to LF. Invalid
	// bytes become RuneError, one byte at a time, matching the
	// behavior of reading runes directly from the bytes.
	l.src = make([]rune, 0, len(input))
	for i := 0; i < len(input); {
		r, size := utf8.DecodeRune(input[i:])
		if r == '\r' {
			if next, _ := utf8.DecodeRune(input[i+size:]); next == '\n' {
				// CR before LF is part of the line separator; drop
				// it and let the LF be read on the next iteration.
				i += size
				continue
			}
			// a lone CR is itself a line separator
			r = '\n'
		}
		l.src = append(l.src, r)
		i += size
	}
	return nil
}

// errorfAt formats an error and prefixes it with the source
// filename and the first definite line and column (both
// 1-based) where the problem was detected. The filename is
// reported exactly as passed to Tokenize.
func (l *lexer) errorfAt(line, col int, format string, args ...any) error {
	return fmt.Errorf("%s:%d:%d: %s", l.file, line, col, fmt.Sprintf(format, args...))
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
	// started records whether the token's source position was
	// captured already; a leading backslash counts as the start
	tokenStarted := false

	// logical line and physical column where the token starts
	tokenLine, tokenCol := 0, 0
	// physical position of the opening quote and of the latest
	// backslash escape, for definite error locations
	quoteLine, quoteCol := 0, 0
	escLine, escCol := 0, 0

	makeToken := func(quoted rune) bool {
		l.token.Text = string(val)
		l.token.Line = tokenLine
		l.token.Column = tokenCol
		l.token.wasQuoted = quoted
		l.token.heredocMarker = heredocMarker
		return true
	}

	for {
		// No more input: finalize an open token, or report
		// unterminated constructs at a definite position.
		if l.pos >= len(l.src) {
			if inHeredoc && len(val) > 0 {
				line := l.line + l.skippedLines
				return false, l.errorfAt(tokenLine, tokenCol, "incomplete heredoc <<%s on line #%d, expected ending marker %s", heredocMarker, line, heredocMarker)
			}
			if quoted || btQuoted {
				quote := `"`
				if btQuoted {
					quote = "`"
				}
				return false, l.errorfAt(quoteLine, quoteCol, "unclosed quoted string: missing closing %s", quote)
			}
			if escaped && !comment {
				return false, l.errorfAt(escLine, escCol, "dangling backslash escape: no character follows")
			}
			if len(val) > 0 {
				return makeToken(0), nil
			}
			return false, nil
		}

		ch := l.src[l.pos]
		l.pos++

		// physical position of this rune, then advance over it
		curLine, curCol := l.physLine, l.physCol
		if ch == '\n' {
			l.physLine++
			l.physCol = 1
		} else {
			l.physCol++
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
			if ch == '\n' {
				if len(val) == 2 {
					return false, l.errorfAt(tokenLine, tokenCol, "missing opening heredoc marker on line #%d; must contain only alphanumeric characters, dashes and underscores; got empty string", curLine)
				}

				// check if there's too many <
				if string(val[:3]) == "<<<" {
					return false, l.errorfAt(tokenLine, tokenCol, "too many '<' for heredoc on line #%d; only use two, for example <<END", curLine)
				}

				heredocMarker = string(val[2:])
				if !heredocMarkerRegexp.Match([]byte(heredocMarker)) {
					return false, l.errorfAt(tokenLine, tokenCol, "heredoc marker on line #%d must contain only alphanumeric characters, dashes and underscores; got '%s'", curLine, heredocMarker)
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
			escaped = true
			escLine, escCol = curLine, curCol
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
			// allow quoted text to continue on multiple lines
			if ch == '\n' {
				l.line += 1 + l.skippedLines
				l.skippedLines = 0
			}
			// collect this character as part of the quoted token
			val = append(val, ch)
			continue
		}

		if unicode.IsSpace(ch) {
			// end of the line
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
		// a quoted or backslash-escaped '#' is ordinary text
		if ch == '#' && len(val) == 0 && !escaped {
			comment = true
		}
		if comment {
			continue
		}

		if len(val) == 0 {
			if !tokenStarted {
				// a leading escape that contributes to the token
				// starts at the backslash; a backslash that merely
				// continues a line never reaches this point
				startCol := curCol
				if escaped {
					startCol = escCol
				}
				tokenLine, tokenCol = l.line, startCol
				tokenStarted = true
				l.token = Token{Line: l.line}
			}
			if ch == '"' {
				quoted = true
				quoteLine, quoteCol = curLine, curCol
				continue
			}
			if ch == '`' {
				btQuoted = true
				quoteLine, quoteCol = curLine, curCol
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

	// figure out how much whitespace we need to strip from the front of every line
	// by getting the string that precedes the marker, on the last line
	paddingToStrip := stringVal[lastNewline+1 : len(stringVal)-len(marker)]

	// iterate over each line and strip the whitespace from the front
	var out string
	for lineNum, lineText := range lines[:len(lines)-1] {
		if lineText == "" {
			out += "\n"
			continue
		}

		// find an exact match for the padding
		index := strings.Index(lineText, paddingToStrip)

		// if the padding doesn't match exactly at the start then we can't safely strip
		if index != 0 {
			return nil, l.errorfAt(l.line+lineNum+1, 1, "mismatched leading whitespace in heredoc <<%s on line #%d [%s], expected whitespace [%s] to match the closing marker", marker, l.line+lineNum+1, lineText, paddingToStrip)
		}

		// strip, then append the line, with the newline, to the output
		out += lineText[len(paddingToStrip):] + "\n"
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
		Column:        t.Column,
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
