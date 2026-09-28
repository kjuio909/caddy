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
	"strconv"
	"strings"
)

// argsPlaceholder kinds recognized by the import expansion.
type argsPlaceholderKind int

const (
	argsSingle   argsPlaceholderKind = iota // {args[N]} (or deprecated {args.N})
	argsVariadic                            // {args[a:b]}
)

// argsPlaceholder describes a parsed argument placeholder.
type argsPlaceholder struct {
	kind       argsPlaceholderKind
	start      int // inclusive start index (variadic and single)
	end        int // exclusive end index (variadic only)
	deprecated bool
}

// parseArgsPlaceholder parses body, the text between the braces of a
// placeholder (for example "args[0]" or "args[1:3]"), and reports
// whether it is a well-formed argument placeholder. A body that is
// clearly meant to be one but has an illegal index is reported with
// ok == true and a non-nil err, so callers can reject the parse
// instead of silently leaving the text untouched.
func parseArgsPlaceholder(body string) (argsPlaceholder, bool, error) {
	if strings.HasPrefix(body, "args[") && strings.HasSuffix(body, "]") {
		inner := body[len("args[") : len(body)-len("]")]
		if inner == "" {
			return argsPlaceholder{}, true, fmt.Errorf("placeholder {args[]} cannot have an empty index")
		}

		rawStart, rawEnd, hasRange := strings.Cut(inner, ":")
		if !hasRange {
			index, err := parsePlaceholderIndex(rawStart)
			if err != nil {
				return argsPlaceholder{kind: argsSingle}, true, err
			}
			return argsPlaceholder{kind: argsSingle, start: index, end: index + 1}, true, nil
		}

		// more than one colon is never a valid range
		if strings.Count(inner, ":") > 1 {
			return argsPlaceholder{kind: argsVariadic}, true, fmt.Errorf("variadic placeholder {%s} has an invalid index range", body)
		}

		ph := argsPlaceholder{kind: argsVariadic}
		if rawStart == "" {
			ph.start = 0
		} else {
			index, err := parsePlaceholderIndex(rawStart)
			if err != nil {
				return argsPlaceholder{kind: argsVariadic}, true, err
			}
			ph.start = index
		}
		if rawEnd == "" {
			// open-ended ranges are bounded by the argument count later
			ph.end = -1
		} else {
			index, err := parsePlaceholderIndex(rawEnd)
			if err != nil {
				return argsPlaceholder{kind: argsVariadic}, true, err
			}
			ph.end = index
		}
		return ph, true, nil
	}

	// TODO: Remove the deprecated {args.*} placeholder support at
	// some point in the future.
	if strings.HasPrefix(body, "args.") && body != "args." {
		index, err := parsePlaceholderIndex(strings.TrimPrefix(body, "args."))
		if err != nil {
			return argsPlaceholder{deprecated: true}, true, err
		}
		return argsPlaceholder{kind: argsSingle, start: index, end: index + 1, deprecated: true}, true, nil
	}

	return argsPlaceholder{}, false, nil
}

// parsePlaceholderIndex parses a single non-negative argument index.
func parsePlaceholderIndex(raw string) (int, error) {
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("invalid argument index %q", raw)
	}
	return index, nil
}

// looksLikeArgsPlaceholder reports whether body, the contents of a
// brace pair, was clearly intended as an argument placeholder but is
// malformed. Such text is an error instead of being kept verbatim.
func looksLikeArgsPlaceholder(body string) bool {
	return body == "args" || body == "args." ||
		strings.HasPrefix(body, "args[") || strings.HasPrefix(body, "args.")
}

// wholeTokenPlaceholder parses text expecting it to contain exactly
// one argument placeholder and nothing else. A token made of several
// adjacent (or malformed) placeholders is not a whole-token match; the
// slow-path scanner decides its fate instead.
func wholeTokenPlaceholder(text string) (argsPlaceholder, bool, error) {
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return argsPlaceholder{}, false, nil
	}
	body := text[1 : len(text)-1]
	// one placeholder cannot itself contain braces, so a token
	// holding several adjacent placeholders is never a whole match
	// and is left for the scanner
	if strings.ContainsAny(body, "{}") {
		return argsPlaceholder{}, false, nil
	}
	ph, ok, err := parseArgsPlaceholder(body)
	if !ok {
		return argsPlaceholder{}, false, nil
	}
	return ph, true, err
}

// substituteImportArgs expands argument placeholders in one imported
// token. A whole-token placeholder is replaced with the argument
// token itself (so arguments containing spaces or quotes stay a
// single token, with their quoting intact); a placeholder embedded
// in surrounding text is substituted in place and can never cause
// re-tokenization.
//
// Ordinary braces, adjacent literal text, and braces escaped with a
// backslash are preserved verbatim. Malformed placeholders, indices
// out of range, and variadic placeholders embedded in other text are
// reported as errors, located at the imported token.
func substituteImportArgs(token Token, argTokens []Token) ([]Token, error) {
	text := token.Text

	// fast path: the token is a single placeholder on its own
	if ph, ok, err := wholeTokenPlaceholder(text); ok {
		if err != nil {
			return nil, importArgsErr(token, err.Error())
		}
		if ph.kind == argsSingle {
			if ph.start >= len(argTokens) {
				return nil, importArgsErr(token,
					fmt.Sprintf("placeholder {args[%d]} is out of range: only %d argument(s) were given to the import", ph.start, len(argTokens)))
			}
			return []Token{positionArgToken(argTokens[ph.start], token)}, nil
		}

		// variadic: validate the range against the available args
		start, end, err := boundVariadic(ph, len(argTokens), text)
		if err != nil {
			return nil, importArgsErr(token, err.Error())
		}
		out := make([]Token, 0, end-start)
		for _, arg := range argTokens[start:end] {
			out = append(out, positionArgToken(arg, token))
		}
		return out, nil
	}

	// slow path: scan the token for embedded placeholders
	var sb strings.Builder
	sb.Grow(len(text))
	for i := 0; i < len(text); {
		// escaped braces are literal braces; the escaping backslash
		// is consumed so the brace is never re-expanded
		if text[i] == '\\' && i+1 < len(text) && (text[i+1] == '{' || text[i+1] == '}') {
			sb.WriteByte(text[i+1])
			i += 2
			continue
		}

		if text[i] != '{' {
			sb.WriteByte(text[i])
			i++
			continue
		}

		closeRel := strings.IndexByte(text[i+1:], '}')
		if closeRel < 0 {
			// unmatched opening brace: the rest is ordinary text
			sb.WriteString(text[i:])
			break
		}
		end := i + 1 + closeRel
		body := text[i+1 : end]

		ph, isArgs, parseErr := parseArgsPlaceholder(body)
		if !isArgs {
			if looksLikeArgsPlaceholder(body) {
				return nil, importArgsErr(token, fmt.Sprintf("invalid argument placeholder {%s}", body))
			}
			// this brace pair is not an args placeholder (for
			// example a JSON object), but an args placeholder may
			// still be nested inside it; emit just the opening
			// brace and rescan its contents
			sb.WriteByte('{')
			i++
			continue
		}
		if parseErr != nil {
			return nil, importArgsErr(token, parseErr.Error())
		}
		if ph.kind == argsVariadic {
			return nil, importArgsErr(token, fmt.Sprintf("variadic placeholder {%s} must be a token on its own", body))
		}
		if ph.start >= len(argTokens) {
			indexText := strconv.Itoa(ph.start)
			if ph.deprecated {
				return nil, importArgsErr(token,
					fmt.Sprintf("placeholder {args.%s} is out of range: only %d argument(s) were given to the import", indexText, len(argTokens)))
			}
			return nil, importArgsErr(token,
				fmt.Sprintf("placeholder {args[%s]} is out of range: only %d argument(s) were given to the import", indexText, len(argTokens)))
		}

		// substitute only the argument's text, in place; quoting
		// belongs to the surrounding token and is left untouched
		sb.WriteString(argTokens[ph.start].Text)
		i = end + 1
	}

	out := token
	out.Text = sb.String()
	return []Token{out}, nil
}

// boundVariadic resolves an open-ended variadic range and validates
// it against the number of available arguments.
func boundVariadic(ph argsPlaceholder, argCount int, text string) (int, int, error) {
	start := ph.start
	end := ph.end
	if end == -1 {
		end = argCount
	}
	if start > end {
		return 0, 0, fmt.Errorf("variadic placeholder %s has a start index greater than its end index", text)
	}
	if end > argCount {
		return 0, 0, fmt.Errorf("variadic placeholder %s is out of range: only %d argument(s) were given to the import", text, argCount)
	}
	return start, end, nil
}

// positionArgToken returns a copy of a placeholder token carrying an
// argument's value. Location and import provenance stay attached to
// where the placeholder was written (so line adjacency and error
// locations are stable through nested imports), while the argument's
// exact text and quoting are preserved without re-tokenization.
func positionArgToken(arg, placeholder Token) Token {
	t := placeholder.Clone()
	t.Text = arg.Text
	t.wasQuoted = arg.wasQuoted
	t.quoteClosed = arg.quoteClosed
	t.heredocMarker = arg.heredocMarker
	return t
}

// importArgsErr builds a parse error for a placeholder problem,
// pointing at the file and line where the placeholder was written.
func importArgsErr(token Token, msg string) error {
	if len(token.imports) > 0 {
		return fmt.Errorf("%s, at %s:%d import chain ['%s']",
			msg, token.File, token.Line, strings.Join(token.imports, "','"))
	}
	return fmt.Errorf("%s, at %s:%d", msg, token.File, token.Line)
}

// parseVariadic determines if the token is a variadic placeholder,
// and if so, determines the index range (start/end) of args to use.
// Returns a boolean signaling whether a variadic placeholder was found,
// and the start and end indices.
//
// Deprecated: retained for the unit-level predicate checks; expansion
// goes through substituteImportArgs, which also rejects malformed
// placeholders and out-of-range indices as hard parse errors.
func parseVariadic(token Token, argCount int) (bool, int, int) {
	ph, ok, err := wholeTokenPlaceholder(token.Text)
	if err != nil || !ok || ph.kind != argsVariadic {
		return false, 0, 0
	}
	start, end, err := boundVariadic(ph, argCount, token.Text)
	if err != nil {
		return false, 0, 0
	}
	return true, start, end
}
