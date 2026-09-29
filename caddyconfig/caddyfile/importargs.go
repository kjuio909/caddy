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

	"go.uber.org/zap"
)

// argWarnFunc logs a warning while expanding import args.
type argWarnFunc func(msg string, fields ...zap.Field)

// expandArgsToken substitutes every complete {args...} placeholder
// in token using the positional args of one import statement.
//
// Only complete placeholders are substituted: ordinary braces
// (e.g. {host} or {env.FOO}) and escaped braces (\{ \}) are kept
// verbatim, along with any literal text adjacent to a placeholder.
// Substituted values never pass through expansion again, even if
// they contain braces. A normal substitution always yields exactly
// one token, so values containing spaces, quotes, or nothing at all
// preserve the token's original boundaries. A variadic placeholder
// ({args[start:end]}) must occupy a token on its own and expands to
// one token per referenced argument, in declaration order.
//
// Malformed placeholders, negative, empty, or out-of-range indices,
// and references despite no declared args are reported as errors
// instead of being silently left in place.
func expandArgsToken(token Token, args []string, warn argWarnFunc) ([]string, error) {
	if warn == nil {
		warn = func(string, ...zap.Field) {}
	}

	text := token.Text
	var sb strings.Builder
	sb.Grow(len(text))

	// a variadic placeholder occupies the whole token, so when one
	// is found its expansion replaces the token entirely
	var variadicValues []string

	for i := 0; i < len(text); {
		// escaped braces are literal braces, never placeholders
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

		closeIdx := findUnescapedCloseBrace(text, i)
		if closeIdx < 0 {
			// no closing brace: this is not a complete placeholder,
			// so the rest of the token is preserved verbatim
			sb.WriteString(text[i:])
			break
		}

		key := text[i+1 : closeIdx]
		group := text[i : closeIdx+1]
		sole := i == 0 && closeIdx == len(text)-1

		switch {
		case strings.HasPrefix(key, argsBracketPrefix) && strings.HasSuffix(key, "]"):
			indexSpec := key[len(argsBracketPrefix) : len(key)-1]
			if strings.Count(indexSpec, ":") == 1 {
				start, end, err := parseVariadicIndices(group, indexSpec, len(args))
				if err != nil {
					return nil, err
				}
				if !sole {
					return nil, fmt.Errorf("variadic placeholder %s must be a token on its own", group)
				}
				values := make([]string, end-start)
				copy(values, args[start:end])
				variadicValues = values
			} else {
				index, err := parseArgIndex(group, indexSpec, len(args))
				if err != nil {
					return nil, err
				}
				sb.WriteString(args[index])
			}

		case strings.HasPrefix(key, argsDeprecatedPrefix):
			indexSpec := key[len(argsDeprecatedPrefix):]
			index, err := parseArgIndex(group, indexSpec, len(args))
			if err != nil {
				return nil, err
			}
			warn("Placeholder "+group+" deprecated, use {args["+indexSpec+"]} instead",
				zap.String("file", token.File+":"+strconv.Itoa(token.Line)),
				zap.Strings("import_chain", token.imports))
			sb.WriteString(args[index])

		default:
			// ordinary braces ({host}, {block}, {env.FOO}, ...)
			// stay; write just the opening brace and keep scanning so
			// that placeholders nested inside other braces (as in
			// JSON tokens) are still expanded
			sb.WriteByte('{')
			i++
			continue
		}

		i = closeIdx + 1
	}

	if variadicValues != nil {
		return variadicValues, nil
	}
	return []string{sb.String()}, nil
}

// findUnescapedCloseBrace returns the index of the first } at or
// after open that is not preceded by a backslash, or -1.
func findUnescapedCloseBrace(text string, open int) int {
	for i := open + 1; i < len(text); i++ {
		if text[i] == '}' && (i == open+1 || text[i-1] != '\\') {
			return i
		}
	}
	return -1
}

// parseArgIndex parses a non-negative index and bounds it against
// the number of declared args.
func parseArgIndex(group, indexSpec string, argCount int) (int, error) {
	if indexSpec == "" {
		return 0, fmt.Errorf("placeholder %s cannot have an empty index", group)
	}
	if strings.Contains(indexSpec, ":") {
		return 0, fmt.Errorf("variadic placeholder %s must be a token on its own", group)
	}
	index, err := strconv.Atoi(indexSpec)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("placeholder %s has an invalid index", group)
	}
	if index >= argCount {
		return 0, fmt.Errorf("placeholder %s index is out of bounds, only %d argument(s) exist",
			group, argCount)
	}
	return index, nil
}

// parseVariadicIndices resolves the start/end of a {args[s:e]}
// reference against the number of declared args.
func parseVariadicIndices(group, indexSpec string, argCount int) (int, int, error) {
	startSpec, endSpec, _ := strings.Cut(indexSpec, ":")
	if strings.Contains(endSpec, ":") {
		return 0, 0, fmt.Errorf("variadic placeholder %s has an invalid index range", group)
	}

	startIndex := 0
	endIndex := argCount
	var err error
	if startSpec != "" {
		startIndex, err = strconv.Atoi(startSpec)
		if err != nil {
			return 0, 0, fmt.Errorf("variadic placeholder %s has an invalid start index", group)
		}
	}
	if endSpec != "" {
		endIndex, err = strconv.Atoi(endSpec)
		if err != nil {
			return 0, 0, fmt.Errorf("variadic placeholder %s has an invalid end index", group)
		}
	}
	if startIndex < 0 || endIndex < 0 || startIndex > endIndex ||
		startIndex > argCount || endIndex > argCount {
		return 0, 0, fmt.Errorf("variadic placeholder %s indices are out of bounds, only %d argument(s) exist",
			group, argCount)
	}
	return startIndex, endIndex, nil
}

const (
	argsBracketPrefix    = "args["
	argsDeprecatedPrefix = "args."
)
