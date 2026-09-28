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

// Argument placeholder shapes. A placeholder is only replaced when
// it is a syntactically complete {args[...]} mark: ordinary braces,
// backslash-escaped braces and adjacent text are preserved literally.
// A variadic placeholder must occupy a token on its own.
const (
	argsPrefix    = "{args["
	argsSuffix    = "]}"
	argsDepPrefix = "{args."
	argsDepSuffix = "}"
)

type argsPlaceholderKind int

const (
	argsNotPlaceholder argsPlaceholderKind = iota
	argsScalar                             // {args[N]} or the deprecated {args.N}
	argsVariadic                           // {args[:]}, {args[N:]}, {args[:M]}, {args[N:M]}
)

// matchArgsPlaceholder reports whether text[start:] begins with a
// complete argument placeholder mark and returns the kind, inner spec
// and the position just past the match. Backslash escaping is handled
// by the caller before invoking this function.
func matchArgsPlaceholder(text string, start int) (kind argsPlaceholderKind, spec string, next int, ok bool) {
	if rest := text[start:]; strings.HasPrefix(rest, argsPrefix) {
		endRel := strings.Index(rest[len(argsPrefix):], argsSuffix)
		if endRel < 0 {
			return argsNotPlaceholder, "", start, false
		}
		innerEnd := len(argsPrefix) + endRel
		spec = rest[len(argsPrefix):innerEnd]
		if spec == "" {
			return argsNotPlaceholder, "", start, false
		}
		kind = argsScalar
		if strings.Contains(spec, ":") {
			kind = argsVariadic
		}
		return kind, spec, start + innerEnd + len(argsSuffix), true
	}
	// deprecated {args.N} form: a single non-negative index with no
	// braces or variadic range inside
	if rest := text[start:]; strings.HasPrefix(rest, argsDepPrefix) {
		endRel := strings.Index(rest[len(argsDepPrefix):], argsDepSuffix)
		if endRel < 0 {
			return argsNotPlaceholder, "", start, false
		}
		spec = rest[len(argsDepPrefix) : len(argsDepPrefix)+endRel]
		if spec == "" || strings.ContainsAny(spec, "{}:") {
			return argsNotPlaceholder, "", start, false
		}
		return argsScalar, spec, start + len(argsDepPrefix) + endRel + len(argsDepSuffix), true
	}
	return argsNotPlaceholder, "", start, false
}

// substituteArgsToken applies the import statement's positional
// arguments to one imported token. A complete placeholder mark is
// replaced even when adjacent to other text; ordinary braces, escaped
// ("doubled") braces and the adjacent text itself stay literal. A
// variadic placeholder is only valid as the entire token and expands,
// in declaration order, into zero or more tokens sharing the metadata
// of token. Empty arguments or arguments containing whitespace or
// quotes remain a single token: the replacement text is never re-lexed.
// Undeclared, negative or out-of-bounds references are reported at the
// placeholder token.
func substituteArgsToken(token Token, args []string) ([]Token, error) {
	// a variadic placeholder is only valid as a token on its own
	if kind, spec, next, ok := matchArgsPlaceholder(token.Text, 0); ok && kind == argsVariadic && next == len(token.Text) {
		start, end, err := parseVariadicBounds(spec, len(args))
		if err != nil {
			return nil, tokenErrf(token, "import argument placeholder %s: %v", token.Text, err)
		}
		out := make([]Token, 0, end-start)
		for _, arg := range args[start:end] {
			t := token
			t.Text = arg
			out = append(out, t)
		}
		return out, nil
	}

	var b strings.Builder
	b.Grow(len(token.Text))
	for i := 0; i < len(token.Text); {
		// a backslash escapes the following brace, mirroring the
		// runtime replacer: "\{" and "\}" stay literal (with the
		// backslash consumed); any other escaped character is kept
		// together with its backslash.
		if token.Text[i] == '\\' && i+1 < len(token.Text) &&
			(token.Text[i+1] == '{' || token.Text[i+1] == '}') {
			b.WriteByte(token.Text[i+1])
			i += 2
			continue
		}
		if kind, spec, next, ok := matchArgsPlaceholder(token.Text, i); ok {
			if kind == argsVariadic {
				// a variadic mark only expands when it occupies the whole
				// token; inline it is indistinguishable from ordinary text
				// and stays literal
				b.WriteString(token.Text[i:next])
				i = next
				continue
			}
			value, err := scalarArgValue(token, token.Text[i:next], spec, args)
			if err != nil {
				return nil, err
			}
			b.WriteString(value)
			i = next
			continue
		}
		// ordinary braces and any other adjacent text stay literal
		b.WriteByte(token.Text[i])
		i++
	}

	out := token
	out.Text = b.String()
	return []Token{out}, nil
}

// scalarArgValue resolves one scalar placeholder mark against args,
// rejecting an invalid or missing index.
func scalarArgValue(token Token, mark string, spec string, args []string) (string, error) {
	index, err := strconv.Atoi(spec)
	if err != nil {
		return "", tokenErrf(token, "import argument placeholder %s has an invalid index %q", mark, spec)
	}
	if index < 0 {
		return "", tokenErrf(token, "import argument placeholder %s has a negative index", mark)
	}
	if index >= len(args) {
		return "", tokenErrf(token, "import argument placeholder %s index is out of bounds, only %d argument(s) were given to the import", mark, len(args))
	}
	return args[index], nil
}

// parseVariadic determines if the token is a variadic placeholder,
// and if so, determines the index range (start/end) of args to use.
// Returns a boolean signaling whether a variadic placeholder was found,
// and the start and end indices. It is a non-erroring view used by the
// lower-level tests; substituteArgsToken performs the validated,
// erroring substitution.
func parseVariadic(token Token, argCount int) (bool, int, int) {
	kind, spec, next, ok := matchArgsPlaceholder(token.Text, 0)
	if !ok || kind != argsVariadic || next != len(token.Text) {
		return false, 0, 0
	}
	start, end, err := parseVariadicBounds(spec, argCount)
	if err != nil {
		return false, 0, 0
	}
	return true, start, end
}

// parseVariadicBounds parses the "start:end" spec of a variadic
// placeholder, defaulting an empty start to 0 and an empty end to the
// number of available arguments.
func parseVariadicBounds(spec string, argCount int) (int, int, error) {
	startSpec, endSpec, found := strings.Cut(spec, ":")
	if !found {
		return 0, 0, errInvalidArgsIndex{value: spec}
	}

	start := 0
	end := argCount
	var err error
	if startSpec != "" {
		start, err = strconv.Atoi(startSpec)
		if err != nil {
			return 0, 0, errInvalidArgsIndex{value: startSpec}
		}
	}
	if endSpec != "" {
		end, err = strconv.Atoi(endSpec)
		if err != nil {
			return 0, 0, errInvalidArgsIndex{value: endSpec}
		}
	}
	if start < 0 {
		return 0, 0, errNegativeArgsIndex{index: start}
	}
	if end < 0 {
		return 0, 0, errNegativeArgsIndex{index: end}
	}
	if start > end {
		return 0, 0, errArgsIndexReversed{start: start, end: end}
	}
	if end > argCount {
		return 0, 0, errArgsIndexOutOfBounds{index: end, argCount: argCount}
	}
	return start, end, nil
}

// tokenErrf reports an error located at token with a stable, readable
// location and its active import chain.
func tokenErrf(token Token, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if len(token.imports) > 0 {
		return fmt.Errorf("%s, at %s:%d import chain ['%s']", msg, token.File, token.Line, strings.Join(token.imports, "','"))
	}
	return fmt.Errorf("%s, at %s:%d", msg, token.File, token.Line)
}

type errInvalidArgsIndex struct{ value string }

func (e errInvalidArgsIndex) Error() string { return "invalid index " + strconv.Quote(e.value) }

type errNegativeArgsIndex struct{ index int }

func (e errNegativeArgsIndex) Error() string {
	return "negative index " + strconv.Itoa(e.index)
}

type errArgsIndexReversed struct{ start, end int }

func (e errArgsIndexReversed) Error() string {
	return "start index " + strconv.Itoa(e.start) + " is greater than end index " + strconv.Itoa(e.end)
}

type errArgsIndexOutOfBounds struct{ index, argCount int }

func (e errArgsIndexOutOfBounds) Error() string {
	return "index " + strconv.Itoa(e.index) + " is out of bounds, only " + strconv.Itoa(e.argCount) + " argument(s) were given to the import"
}
