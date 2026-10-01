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
	"unicode"
	"unicode/utf8"
)

// bomRune is the Unicode byte order mark U+FEFF, whose UTF-8 encoding
// is bomBytes.
const bomRune rune = 0xFEFF

var bomBytes = []byte{0xEF, 0xBB, 0xBF}

// Format formats the input Caddyfile to a standard, nice-looking
// appearance. It works by reading each rune of the input and taking
// control over all the bracing and whitespace that is written; otherwise,
// words, comments, placeholders, and escaped characters are all treated
// literally and written as they appear in the input.
//
// A single, complete UTF-8 BOM at the very beginning of the input is
// preserved verbatim as a file marker and is emitted exactly once;
// a BOM anywhere else (ordinary content, quoted arguments, heredoc
// bodies) keeps its original bytes. A truncated leading BOM, an
// unterminated quote, a dangling escape, mismatched structural braces,
// or an unterminated heredoc cause the input to be returned byte for
// byte unchanged — no partial formatting is ever emitted for those.
// CRLF, bare CR, and LF line endings in structured parts collapse to
// LF; heredoc bodies are copied byte for byte. The result is always a
// fixed point of Format (formatting the output changes nothing); an
// input the formatter cannot normalize to a stable form is returned
// byte for byte unchanged. The returned slice is always freshly
// allocated and never aliases the input buffer, so mutating or reusing
// the input after the call cannot change the result, and the function
// carries no state between calls.
func Format(input []byte) []byte {
	out, ok := formatPass(input)
	if !ok {
		// malformed input: no partial formatting may leak out
		return bytes.Clone(input)
	}

	// Idempotence is a hard guarantee. Run a second pass over the
	// formatted bytes; if they would move at all, the input hits a
	// corner that cannot be normalized deterministically, so leave the
	// original untouched rather than leaking an unstable result.
	out2, ok2 := formatPass(out)
	if !ok2 || !bytes.Equal(out, out2) {
		return bytes.Clone(input)
	}

	return out
}

// formatPass performs one formatting pass over input, handling the
// optional leading BOM. It returns ok=false for a truncated leading
// BOM or any malformed input that Format must leave untouched.
func formatPass(input []byte) (out []byte, ok bool) {
	bomKind, body := splitLeadingBOM(input)
	if bomKind < 0 {
		// truncated leading BOM: do not stitch, drop, or rewrite it
		return nil, false
	}

	// the Caddyfile does not need any leading or trailing spaces, but
	// a BOM in the content is not whitespace and must be preserved
	data := trimSpaceKeepBOM(body)

	return formatBytes(data, bomKind == 1)
}

// splitLeadingBOM inspects the first bytes of input for a UTF-8 BOM.
// It returns (1, rest) for a complete BOM, (0, input) when there is no
// BOM, and (-1, input) when the input ends inside a leading BOM prefix.
func splitLeadingBOM(input []byte) (int, []byte) {
	switch {
	case len(input) >= 3 && input[0] == bomBytes[0] && input[1] == bomBytes[1] && input[2] == bomBytes[2]:
		return 1, input[3:]
	case len(input) == 1 && input[0] == bomBytes[0]:
		return -1, input
	case len(input) == 2 && input[0] == bomBytes[0] && input[1] == bomBytes[1]:
		return -1, input
	default:
		return 0, input
	}
}

// trimSpaceKeepBOM is like bytes.TrimSpace except that U+FEFF is treated
// as an ordinary character, so a BOM embedded in the content survives.
func trimSpaceKeepBOM(b []byte) []byte {
	start := 0
	for start < len(b) {
		r, size := utf8.DecodeRune(b[start:])
		if r == bomRune || r == utf8.RuneError || !unicode.IsSpace(r) {
			break
		}
		start += size
	}
	end := len(b)
	for end > start {
		r, size := utf8.DecodeLastRune(b[:end])
		if r == bomRune || r == utf8.RuneError || !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	return b[start:end]
}

// byteCursor is a simple rune reader over a byte slice. It offers two
// points of view: raw reads hand back the exact bytes of the next rune,
// while structured reads collapse CRLF and bare CR line endings into a
// single LF (the byte span is nil in that case, so the caller can tell
// synthesized line feeds from original ones).
type byteCursor struct {
	b   []byte
	pos int
}

// nextRaw decodes the next rune without any normalization. lit is the
// original byte span of the rune, even for invalid UTF-8.
func (c *byteCursor) nextRaw() (r rune, lit []byte, ok bool) {
	if c.pos >= len(c.b) {
		return 0, nil, false
	}
	r, size := utf8.DecodeRune(c.b[c.pos:])
	lit = c.b[c.pos : c.pos+size]
	c.pos += size
	return r, lit, true
}

// nextStructured decodes the next rune, collapsing CRLF and bare CR
// into a single synthesized LF (lit is nil).
func (c *byteCursor) nextStructured() (r rune, lit []byte, ok bool) {
	if c.pos >= len(c.b) {
		return 0, nil, false
	}
	if c.b[c.pos] == '\r' {
		if c.pos+1 < len(c.b) && c.b[c.pos+1] == '\n' {
			c.pos += 2
		} else {
			c.pos++
		}
		return '\n', nil, true
	}
	return c.nextRaw()
}

func formatBytes(data []byte, emitBOM bool) (result []byte, ok bool) {
	type heredocState int

	const (
		heredocClosed  heredocState = 0
		heredocOpening heredocState = 1
		heredocOpened  heredocState = 2
	)

	cur := &byteCursor{b: data}
	out := new(bytes.Buffer)
	if emitBOM {
		out.Write(bomBytes)
	}

	var (
		last rune // the last character that was written to the result

		space           = true // whether current/previous character was whitespace (beginning of input counts as space)
		beginningOfLine = true // whether we are at beginning of line

		openBrace        bool // whether current word/token is or started with open curly brace
		openBraceWritten bool // if openBrace, whether that brace was written or not
		openBraceSpace   bool // whether there was a non-newline space before open brace

		newLines int // count of newlines consumed

		comment bool   // whether we're in a comment
		quotes  string // encountered quotes ('', '`', '"', '"`', '`"')
		escaped bool   // whether current char is escaped

		heredoc        heredocState // whether we're in a heredoc
		heredocEscaped bool         // whether heredoc is escaped
		heredocMarker  []rune

		nesting int // indentation level

		// structural brace balance, used only to reject malformed input
		braceDepth int
		minDepth   int

		currentToken                  strings.Builder
		currentLineFirstToken         string
		previousLineWasTopLevelImport bool
		openBraceOwnLine              bool
	)

	finishToken := func() {
		if currentToken.Len() == 0 {
			return
		}
		if currentLineFirstToken == "" {
			currentLineFirstToken = currentToken.String()
		}
		currentToken.Reset()
	}

	finishLine := func() {
		finishToken()
		if currentLineFirstToken != "" {
			previousLineWasTopLevelImport = nesting == 0 && currentLineFirstToken == "import"
		} else if !openBrace || !openBraceOwnLine || openBraceWritten {
			previousLineWasTopLevelImport = false
		}
		currentLineFirstToken = ""
	}

	// writeSynth writes a character produced by the formatter itself.
	writeSynth := func(ch rune) {
		out.WriteRune(ch)
		last = ch
	}

	// writeLiteral writes a character together with its original byte
	// span. Literal spans are copied untouched so that multi-byte
	// sequences and invalid UTF-8 keep their original byte boundaries;
	// a nil span denotes a character synthesized by the reader (a line
	// ending collapsed to LF).
	writeLiteral := func(ch rune, lit []byte) {
		if lit == nil {
			out.WriteRune(ch)
		} else {
			out.Write(lit)
		}
		last = ch
	}

	indent := func() {
		for tabs := nesting; tabs > 0; tabs-- {
			writeSynth('\t')
		}
	}

	nextLine := func() {
		writeSynth('\n')
		beginningOfLine = true
	}

	// readHeredocLine consumes one raw line while a heredoc is open.
	// It returns the raw bytes of the whole line (content plus its
	// line ending, if any), the content without the ending, and whether
	// the line ended before EOF.
	readHeredocLine := func() (raw, content []byte, terminated bool) {
		start := cur.pos
		for cur.pos < len(cur.b) && cur.b[cur.pos] != '\n' && cur.b[cur.pos] != '\r' {
			_, size := utf8.DecodeRune(cur.b[cur.pos:])
			cur.pos += size
		}
		contentEnd := cur.pos
		if cur.pos < len(cur.b) {
			terminated = true
			if cur.b[cur.pos] == '\r' && cur.pos+1 < len(cur.b) && cur.b[cur.pos+1] == '\n' {
				cur.pos += 2
			} else {
				cur.pos++
			}
		}
		return cur.b[start:cur.pos], cur.b[start:contentEnd], terminated
	}

	for {
		// While a heredoc is open, consume whole lines verbatim. A line
		// closes the heredoc only when it is the marker on its own line
		// boundary (preceded only by spaces or tabs, with nothing after
		// it); marker text appearing anywhere else is ordinary body.
		if heredoc == heredocOpened {
			raw, content, terminated := readHeredocLine()
			marker := []byte(string(heredocMarker))
			left := bytes.TrimLeft(content, " \t")
			// the marker closes the heredoc only when it is alone on a
			// line boundary (after a line break or at EOF); trailing
			// spaces or more text keep the heredoc open
			isClose := bytes.Equal(left, marker)
			if isClose {
				// Copy the closing line content verbatim: its leading
				// whitespace is the padding the lexer strips from every
				// body line, so changing it would break the
				// correspondence between the body and the marker. The
				// line ending itself is normalized like any structural
				// line ending.
				out.Write(content)
				if r, _ := utf8.DecodeLastRune(marker); r != utf8.RuneError {
					last = r
				}
				heredocMarker = nil
				heredoc = heredocClosed
				space = true
				heredocEscaped = false
				finishLine()
				if terminated {
					newLines++
				} else {
					beginningOfLine = false
				}
				continue
			}

			// anything else is body: spaces, tabs, CRs, braces, and
			// comment bytes pass through completely untouched
			out.Write(raw)
			if !terminated {
				// ran out of input without a closing marker
				return nil, false
			}
			continue
		}

		// Quoted spans are read raw so their bytes never move; every
		// other part of the file gets normalized line endings.
		var ch rune
		var lit []byte
		var readOK bool
		if quotes != "" {
			ch, lit, readOK = cur.nextRaw()
		} else {
			ch, lit, readOK = cur.nextStructured()
		}
		if !readOK {
			break
		}

		// detect whether we have the start of a heredoc
		if quotes == "" && (heredoc == heredocClosed && !heredocEscaped) &&
			space && last == '<' && ch == '<' {
			writeLiteral(ch, lit)
			heredoc = heredocOpening
			space = false
			continue
		}

		if heredoc == heredocOpening {
			if ch == '\n' {
				if len(heredocMarker) > 0 && heredocMarkerRegexp.MatchString(string(heredocMarker)) {
					heredoc = heredocOpened
				} else {
					heredocMarker = nil
					heredoc = heredocClosed
					nextLine()
					continue
				}
				writeSynth('\n')
				continue
			}
			if unicode.IsSpace(ch) {
				// a space means it's just a regular token and not a heredoc
				heredocMarker = nil
				heredoc = heredocClosed
			} else {
				heredocMarker = append(heredocMarker, ch)
				writeLiteral(ch, lit)
				continue
			}
		}

		if last == '<' && space {
			space = false
		}

		if comment {
			if ch == '\n' {
				comment = false
				space = true
				nextLine()
				continue
			}
			writeLiteral(ch, lit)
			continue
		}

		if !escaped && ch == '\\' {
			if space {
				writeSynth(' ')
				space = false
			}
			writeLiteral(ch, lit)
			escaped = true
			continue
		}

		if escaped {
			if ch == '<' {
				heredocEscaped = true
			}
			writeLiteral(ch, lit)
			escaped = false
			continue
		}

		if ch == '`' {
			switch quotes {
			case "\"`":
				quotes = "\""
			case "`":
				quotes = ""
			case "\"":
				quotes = "\"`"
			default:
				quotes = "`"
			}
		}

		if quotes == "\"" {
			if ch == '"' {
				quotes = ""
			}
			writeLiteral(ch, lit)
			continue
		}

		if ch == '"' {
			switch quotes {
			case "":
				if space {
					quotes = "\""
				}
			case "`\"":
				quotes = "`"
			case "\"`":
				quotes = ""
			}
		}

		if strings.Contains(quotes, "`") {
			if ch == '`' && space && !beginningOfLine {
				writeSynth(' ')
			}
			writeLiteral(ch, lit)
			space = false
			continue
		}

		if unicode.IsSpace(ch) {
			finishToken()
			space = true
			heredocEscaped = false
			if ch == '\n' {
				finishLine()
				newLines++
			}
			continue
		}
		spacePrior := space
		space = false

		//////////////////////////////////////////////////////////
		// I find it helpful to think of the formatting loop in two
		// main sections; by the time we reach this point, we
		// know we are in a "regular" part of the file: we know
		// the character is not a space, not in a literal segment
		// like a comment or quoted, it's not escaped, etc.
		//////////////////////////////////////////////////////////

		if ch == '#' {
			comment = true
		}

		if openBrace && spacePrior && !openBraceWritten {
			if nesting == 0 && last == '}' {
				nextLine()
				nextLine()
			}

			openBrace = false
			if openBraceOwnLine && previousLineWasTopLevelImport {
				if last != '\n' {
					nextLine()
				}
				indent()
			} else if beginningOfLine {
				indent()
			} else if !openBraceSpace || !unicode.IsSpace(last) {
				writeSynth(' ')
			}
			writeSynth('{')
			openBraceWritten = true
			openBraceOwnLine = false
			nextLine()
			newLines = 0
			braceDepth++
			// prevent infinite nesting from ridiculous inputs (issue #4169)
			if nesting < 10 {
				nesting++
			}
		}

		switch {
		case ch == '{':
			finishToken()
			openBrace = true
			openBraceSpace = spacePrior && !beginningOfLine
			openBraceOwnLine = newLines > 0
			if openBraceSpace && newLines == 0 {
				writeSynth(' ')
			}
			openBraceWritten = false
			if quotes == "`" {
				writeLiteral(ch, lit)
				openBraceWritten = true
				openBraceOwnLine = false
				continue
			}
			continue

		case ch == '}' && (spacePrior || !openBrace):
			finishToken()
			if quotes == "`" {
				writeLiteral(ch, lit)
				continue
			}
			if last != '\n' {
				nextLine()
			}
			braceDepth--
			if braceDepth < minDepth {
				minDepth = braceDepth
			}
			if nesting > 0 {
				nesting--
			}
			indent()
			writeSynth('}')
			newLines = 0
			continue
		}

		if newLines > 2 {
			newLines = 2
		}
		for i := 0; i < newLines; i++ {
			nextLine()
		}
		newLines = 0
		if beginningOfLine {
			indent()
		}
		if nesting == 0 && last == '}' && beginningOfLine {
			nextLine()
			nextLine()
		}

		if !beginningOfLine && spacePrior {
			writeSynth(' ')
		}

		if openBrace && !openBraceWritten {
			writeSynth('{')
			openBraceWritten = true
		}

		if spacePrior && ch == '<' {
			space = true
		}

		currentToken.WriteRune(ch)
		writeLiteral(ch, lit)

		beginningOfLine = false
	}

	// reject malformed inputs: open quote, dangling escape, open heredoc,
	// an open brace still pending at EOF, or unbalanced structural braces
	if quotes != "" || escaped || heredoc == heredocOpened ||
		(openBrace && !openBraceWritten) ||
		braceDepth != 0 || minDepth < 0 {
		return nil, false
	}

	// ...Caddyfiles should, however, end with a newline because
	// newlines are significant to the syntax of the file
	trimmedResult := trimSpaceKeepBOM(out.Bytes())
	result = make([]byte, 0, len(trimmedResult)+1)
	result = append(result, trimmedResult...)
	result = append(result, '\n')

	return result, true
}
