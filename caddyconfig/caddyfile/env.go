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
	"os"
	"strings"
)

// envSnapshot is the environment captured once, at the beginning of a
// Parse call. The top-level Caddyfile and every file it imports are
// expanded against the same snapshot, so a variable cannot resolve to
// two different values within a single adaptation, and values cannot
// change while imports are being resolved.
//
// A name present with an empty string value is distinct from a name
// that is absent.
type envSnapshot map[string]string

// newEnvSnapshot captures the current process environment.
func newEnvSnapshot() envSnapshot {
	snap := make(envSnapshot)
	for _, kv := range os.Environ() {
		if key, val, found := strings.Cut(kv, "="); found {
			snap[key] = val
		}
	}
	return snap
}

// expandEnvTokens expands environment variable notations within each
// token's text. Expansion happens after lexing, on a per-token basis,
// so the bytes substituted for a variable can never be re-tokenized
// into additional arguments, quotes, braces, or new placeholders:
// whitespace, quotes, newlines, and braces in a value stay inside the
// token that contained the notation. Expansion never fails partially:
// the first invalid notation aborts the whole call with an error that
// identifies the originating file and line.
func expandEnvTokens(tokens []Token, env envSnapshot) ([]Token, error) {
	for i := range tokens {
		expanded, err := expandTokenText(tokens[i].Text, env, &tokens[i])
		if err != nil {
			return nil, err
		}
		tokens[i].Text = expanded
	}
	return tokens, nil
}

// expandTokenText expands the {$NAME} and {$NAME:default} notations in
// s. A notation whose braces are both escaped (\{$NAME\}) is preserved
// literally as {$NAME} without consulting the environment. token
// supplies the source location used in error messages.
func expandTokenText(s string, env envSnapshot, token *Token) (string, error) {
	var out strings.Builder
	out.Grow(len(s))

	for i := 0; i < len(s); {
		switch {
		// Escaped notation: both delimiters are backslash-escaped,
		// e.g. \{$NAME\}. The first closing brace after the opening
		// must itself be escaped for the pair to count as escaped;
		// the enclosed text is emitted verbatim (minus the escaping
		// backslashes) and the environment is never consulted.
		case strings.HasPrefix(s[i:], escapedEnvOpen):
			closeIdx := strings.IndexByte(s[i+len(escapedEnvOpen):], envCloseByte)
			if closeIdx >= 0 {
				closeBrace := i + len(escapedEnvOpen) + closeIdx
				if closeBrace > i+len(escapedEnvOpen)-1 && s[closeBrace-1] == envEscapeByte {
					out.WriteString(envOpen)
					out.WriteString(s[i+len(escapedEnvOpen) : closeBrace-1])
					out.WriteByte(envCloseByte)
					i = closeBrace + 1
					continue
				}
			}
			// No escaped closing brace to match the escaped opening;
			// the backslash is an ordinary byte, so emit it and let
			// the following bytes be interpreted normally.
			out.WriteByte(s[i])
			i++

		// Environment variable notation: {$NAME} or {$NAME:default}.
		case strings.HasPrefix(s[i:], envOpen):
			closeIdx := strings.IndexByte(s[i+len(envOpen):], envCloseByte)
			if closeIdx < 0 {
				return "", envExpandErr(token,
					"environment variable notation is not closed: %q",
					s[i:])
			}
			closeBrace := i + len(envOpen) + closeIdx
			body := s[i+len(envOpen) : closeBrace]

			name, hasDefault, defaultVal := parseEnvNotation(body)
			if name == "" {
				return "", envExpandErr(token,
					"environment variable name must not be empty: {$%s}",
					body)
			}
			if hasDefault && defaultVal == "" {
				return "", envExpandErr(token,
					"environment variable %q has a default separator but no default value: {%s}",
					name, body)
			}

			val, found := env[name]
			if !found {
				if !hasDefault {
					return "", envExpandErr(token,
						"environment variable %q is not set; use {%s:default} to provide a default",
						name, name)
				}
				val = defaultVal
			}
			// The value is spliced into the current token as raw
			// bytes; it is never scanned for further notations.
			out.WriteString(val)
			i = closeBrace + 1

		default:
			out.WriteByte(s[i])
			i++
		}
	}

	return out.String(), nil
}

// parseEnvNotation splits the body of a {$...} notation into the
// variable name and an optional default value separated by the first
// default delimiter.
func parseEnvNotation(body string) (name string, hasDefault bool, defaultVal string) {
	name, defaultVal, hasDefault = strings.Cut(body, envVarDefaultDelimiter)
	return
}

// envExpandErr formats an expansion error with the token's source
// location, mirroring the file:line style of Dispenser.WrapErr.
func envExpandErr(token *Token, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if len(token.imports) > 0 {
		return fmt.Errorf("%s, at %s:%d import chain ['%s']",
			msg, token.File, token.Line, strings.Join(token.imports, "','"))
	}
	return fmt.Errorf("%s, at %s:%d", msg, token.File, token.Line)
}

var (
	// envOpen opens and envCloseByte closes an environment variable
	// notation: {$NAME}.
	envOpen       = "{$"
	envCloseByte  = byte('}')
	envEscapeByte = byte('\\')

	// escapedEnvOpen opens an escaped notation; an escaped closing
	// brace (\}) completes it. The notation is preserved literally.
	escapedEnvOpen = `\{$`

	// envVarDefaultDelimiter separates a variable name from its default.
	envVarDefaultDelimiter = ":"
)
