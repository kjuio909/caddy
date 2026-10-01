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

// utf8BOM is the Unicode byte order mark that may prefix a UTF-8 file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Format formats the input Caddyfile to a standard, nice-looking
// appearance. It works by reading each rune of the input and taking
// control over all the bracing and whitespace that is written; otherwise,
// words, comments, placeholders, and escaped characters are all treated
// literally and written as they appear in the input.
//
// A single complete UTF-8 byte order mark at the very start of the
// input is preserved as a file marker and appears only once; BOM bytes
// anywhere else are kept verbatim. Line endings (CRLF, bare CR, and
// LF) on structural lines are normalized to LF, while every byte of a
// heredoc body is copied unchanged.
//
// If the input cannot be formatted safely — an unclosed quote, a
// dangling escape, mismatched structural braces, an unterminated
// heredoc, or a truncated leading byte order mark — the input is
// returned byte-for-byte; no partially formatted result is exposed.
// The returned slice never aliases the input buffer and carries no
// state from any other call.
func Format(input []byte) []byte {
	// A truncated byte order mark at the start of the input is not a
	// valid file marker; leave the bytes untouched.
	if hasTruncatedBOM(input) {
		return copyBytes(input)
	}

	hasBOM := bytes.HasPrefix(input, utf8BOM)
	content := input
	if hasBOM {
		content = input[len(utf8BOM):]
	}

	result := formatContent(content)
	if result == nil {
		return copyBytes(input)
	}

	// Idempotency backstop: never expose a result that formatting again
	// would change. Pathological inputs the structural scan accepts but
	// cannot stabilize must come back byte-for-byte instead of leaking a
	// partial, non-idempotent transformation. Normal configurations are
	// already fixed points and are unaffected.
	if again := formatContent(result); again == nil || !bytes.Equal(again, result) {
		return copyBytes(input)
	}

	// Allocate a fresh slice so the result never aliases the input and
	// the BOM file marker, when present, appears exactly once.
	out := make([]byte, 0, len(utf8BOM)+len(result))
	if hasBOM {
		out = append(out, utf8BOM...)
	}
	out = append(out, result...)
	return out
}

// formatContent runs the whitespace-and-brace formatter over content
// (without any leading byte order mark). It returns the normalized
// result ending in a single LF, or nil if the content is malformed
// (unterminated heredoc, quote, or escape; mismatched structural
// braces; invalid adjacent braces).
func formatContent(content []byte) []byte {
	out := new(bytes.Buffer)

	type heredocState int

	const (
		heredocClosed  heredocState = 0
		heredocOpening heredocState = 1
	)

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

		heredoc        heredocState // whether we're reading a heredoc opening marker
		heredocEscaped bool         // whether a heredoc was escaped
		heredocMarker  []rune
		// heredocLookahead is true after a '<' written at the start
		// of a token; the next rune can complete the '<<' heredoc
		// prefix. It does not survive whitespace or a line boundary.
		heredocLookahead bool

		nesting int // indentation level
		depth   int // structural brace depth (uncounted placeholder braces excluded)

		invalid bool // the input is malformed; the result must be discarded

		currentToken                  strings.Builder
		currentLineFirstToken         string
		previousLineWasTopLevelImport bool
		openBraceOwnLine              bool
	)

	// rawByte carries the original byte of the current iteration to
	// write verbatim when decoding produced RuneError, so invalid
	// UTF-8 is preserved instead of being replaced
	var rawByte byte
	write := func(ch rune) {
		if ch == utf8.RuneError {
			out.WriteByte(rawByte)
		} else {
			out.WriteRune(ch)
		}
		last = ch
	}

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

	indent := func() {
		for tabs := nesting; tabs > 0; tabs-- {
			write('\t')
		}
	}

	nextLine := func() {
		write('\n')
		beginningOfLine = true
	}

	// flushPendingBrace emits a structural opening brace that was
	// buffered while the formatter waited to see whether it started a
	// block or was attached to a token. It must run before the first
	// body token of the block, including tokens that begin with an
	// escape or a quote, which take early paths in the main loop.
	flushPendingBrace := func(spacePrior bool) bool {
		if !openBrace || !spacePrior || openBraceWritten {
			return false
		}
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
			write(' ')
		}
		write('{')
		openBraceWritten = true
		openBraceOwnLine = false
		nextLine()
		newLines = 0
		space = false
		// prevent infinite nesting from ridiculous inputs (issue #4169)
		if nesting < 10 {
			nesting++
		}
		depth++
		return true
	}

	// startToken writes any pending newlines, indentation, or a single
	// separating space before a structural token, first flushing a
	// pending open brace. Early token paths (escapes, opening quotes)
	// route through it so a block body beginning with such a token is
	// formatted exactly like any other.
	startToken := func(spacePrior bool) {
		flushPendingBrace(spacePrior)
		if newLines > 2 {
			newLines = 2
		}
		for n := 0; n < newLines; n++ {
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
			write(' ')
		}
	}

scan:
	for i := 0; i < len(content); {
		ch, size := utf8.DecodeRune(content[i:])
		next := i + size
		if ch == utf8.RuneError && size == 1 {
			rawByte = content[i]
		}

		// Normalize CR and CRLF line endings on structural lines
		// (including a heredoc opening marker line). Quoted arguments
		// and escaped characters keep their bytes verbatim, and heredoc
		// bodies are copied without ever reaching this fold.
		if ch == '\r' && quotes == "" && !escaped {
			ch = '\n'
			if next < len(content) && content[next] == '\n' {
				next++ // swallow the LF of a CRLF pair
			}
		}

		// detect whether we have the start of a heredoc: the second
		// '<' must immediately follow the first within the same token,
		// never across whitespace or a line boundary
		if quotes == "" && (heredoc == heredocClosed && !heredocEscaped) &&
			heredocLookahead && ch == '<' {
			heredocLookahead = false
			write(ch)
			heredoc = heredocOpening
			space = false
			i = next
			continue
		}

		if heredoc == heredocOpening {
			if ch == '\n' {
				if len(heredocMarker) > 0 && heredocMarkerRegexp.MatchString(string(heredocMarker)) {
					// The opening marker line is structural, so its
					// line ending is emitted as a normalized LF.
					write('\n')

					// Locate the closing marker and copy the body,
					// the marker line's indentation padding, and the
					// marker itself byte-for-byte.
					bodyStart := next
					markerStart, markerEnd, found := findHeredocClose(
						content, bodyStart, heredocMarker)
					if !found {
						invalid = true
						break
					}
					out.Write(content[bodyStart:markerEnd])

					heredocMarker = nil
					heredoc = heredocClosed
					// The whitespace rune that ends the marker line was
					// not written (as in the original loop); scanning
					// resumes there so it is handled structurally,
					// finishing the marker line and normalizing its
					// line ending.
					last, _ = utf8.DecodeLastRune(content[markerStart:markerEnd])
					space = false
					beginningOfLine = false
					newLines = 0

					i = markerEnd
					continue
				}
				// a newline with no marker, or a marker the grammar
				// does not accept, is an invalid heredoc opening; the
				// lexer rejects it ("missing/invalid opening heredoc
				// marker"), so the input must be returned unchanged
				invalid = true
				break
			}
			if unicode.IsSpace(ch) {
				// a space means it's just a regular token and not a heredoc
				heredocMarker = nil
				heredoc = heredocClosed
			} else {
				heredocMarker = append(heredocMarker, ch)
				write(ch)
				i = next
				continue
			}
		}

		// Resolve a pending '<<' candidate. The first '<' was written
		// at a token start, so the next rune is adjacent to it: if it
		// is not the second '<' nor whitespace, treat it as part of the
		// same token (no separating space). Whitespace cancels the
		// candidate entirely so a later '<' cannot start a heredoc
		// across a line or token boundary.
		if heredocLookahead && ch != '<' {
			heredocLookahead = false
			if !unicode.IsSpace(ch) {
				space = false
			}
		}

		if comment {
			if ch == '\n' {
				comment = false
				space = true
				nextLine()
				i = next
				continue
			}
			write(ch)
			i = next
			continue
		}

		if !escaped && ch == '\\' {
			// an escape can be the first token of a block body; route
			// through startToken so newlines, indentation, and any
			// pending open brace are handled like any other token
			startToken(space)
			space = false
			write(ch)
			escaped = true
			beginningOfLine = false
			i = next
			continue
		}

		if escaped {
			if ch == '<' {
				heredocEscaped = true
			}
			write(ch)
			escaped = false
			i = next
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
				// an opening backtick can be the first token of a
				// block body; route through startToken so newlines,
				// indentation, and any pending brace are handled
				startToken(space)
				space = false
				beginningOfLine = false
				quotes = "`"
			}
		}

		if quotes == "\"" {
			if ch == '"' {
				quotes = ""
			}
			write(ch)
			i = next
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
				write(' ')
			}
			write(ch)
			space = false
			i = next
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
			i = next
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

		// Two structural open braces with no token boundary between
		// them (e.g. "{{") would be collapsed or rewritten by the
		// brace machinery, which cannot preserve the original byte
		// boundaries; treat such input as malformed.
		if ch == '{' && openBrace && !openBraceWritten && !spacePrior {
			invalid = true
			break scan
		}

		flushPendingBrace(spacePrior)

		switch {
		case ch == '{':
			finishToken()
			openBrace = true
			openBraceSpace = spacePrior && !beginningOfLine
			openBraceOwnLine = newLines > 0
			if openBraceSpace && newLines == 0 {
				write(' ')
			}
			openBraceWritten = false
			if quotes == "`" {
				write('{')
				openBraceWritten = true
				openBraceOwnLine = false
				i = next
				continue
			}
			i = next
			continue

		case ch == '}' && (spacePrior || !openBrace):
			finishToken()
			if quotes == "`" {
				write('}')
				i = next
				continue
			}
			if last != '\n' {
				nextLine()
			}
			if nesting > 0 {
				nesting--
			}
			depth--
			indent()
			write('}')
			newLines = 0
			i = next
			continue
		}

		if newLines > 2 {
			newLines = 2
		}
		for n := 0; n < newLines; n++ {
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
			write(' ')
		}

		if openBrace && !openBraceWritten {
			// A brace attached directly to a token (as in a
			// placeholder like {$FOO}) is not a structural brace;
			// it is not counted toward brace balance.
			write('{')
			openBraceWritten = true
		}

		// A '<' beginning a token is left in a "space-adjacent" state
		// (space stays true): the following rune either completes the
		// '<<' heredoc prefix or is joined to it. heredocLookahead
		// additionally prevents the prefix from spanning a boundary.
		if spacePrior && ch == '<' && quotes == "" && !escaped {
			space = true
			heredocLookahead = true
		}

		currentToken.WriteRune(ch)
		write(ch)

		beginningOfLine = false
		i = next
	}

	// malformed content yields no result:
	// unterminated heredoc, unclosed quote, dangling escape, or
	// mismatched structural braces (including a dangling open brace)
	if invalid || quotes != "" || escaped || depth != 0 || (openBrace && !openBraceWritten) {
		return nil
	}

	// the Caddyfile does not need any leading or trailing spaces, but...
	trimmedResult := bytes.TrimSpace(out.Bytes())

	// ...Caddyfiles should, however, end with a newline because
	// newlines are significant to the syntax of the file.
	return append(trimmedResult, '\n')
}

// findHeredocClose scans a heredoc body starting at bodyStart and
// locates the closing marker, mirroring the character-at-a-time rule:
// the marker runes must be the runes immediately preceding a
// whitespace rune; that whitespace rune ends the heredoc and is
// processed structurally when scanning resumes (a line ending there
// is normalized, a space separates tokens). Everything before the
// marker is raw body, including the indentation padding on its line.
// It returns:
//   - markerStart: offset of the marker's first rune
//   - markerEnd:   offset just past the marker (the whitespace rune)
//
// ok is false when the heredoc is not closed before end of input; a
// marker on a final line without any following whitespace does not
// close it.
func findHeredocClose(content []byte, bodyStart int, marker []rune) (markerStart, markerEnd int, ok bool) {
	// the last len(marker)+1 (rune, offset) pairs
	type runePos struct {
		r rune
		p int
	}
	var window []runePos

	for i := bodyStart; i < len(content); {
		r, size := utf8.DecodeRune(content[i:])
		window = append(window, runePos{r, i})
		if len(window) > len(marker)+1 {
			window = window[1:]
		}

		if unicode.IsSpace(r) && len(window) == len(marker)+1 {
			match := true
			for k, want := range marker {
				if window[k].r != want {
					match = false
					break
				}
			}
			if match {
				return window[0].p, i, true
			}
		}

		// a newline starts a fresh marker candidate line
		if r == '\n' {
			window = window[:0]
		}

		i += size
	}
	return 0, 0, false
}

// hasTruncatedBOM reports whether input begins with a byte order
// mark fragment that does not form the complete, valid BOM: one or
// two matching leading bytes cut short, or matching leading bytes
// followed by a byte that breaks the BOM sequence. Such bytes are
// ambiguous file markers and must be left untouched.
func hasTruncatedBOM(input []byte) bool {
	if len(input) == 0 || input[0] != utf8BOM[0] {
		return false
	}
	if bytes.HasPrefix(input, utf8BOM) {
		return false
	}
	return true
}

// copyBytes returns a slice that does not alias b.
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
