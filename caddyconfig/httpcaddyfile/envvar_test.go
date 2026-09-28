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

package httpcaddyfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// adaptForEnvTest adapts a Caddyfile through the public entry point.
func adaptForEnvTest(t *testing.T, filename, input string) ([]byte, []caddyconfig.Warning, error) {
	t.Helper()
	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	out, warnings, err := adapter.Adapt([]byte(input), map[string]any{"filename": filename})
	return out, warnings, err
}

// staticBodies collects every static_response body from adapted JSON.
func staticBodies(t *testing.T, out []byte) []string {
	t.Helper()
	var generic any
	if err := json.Unmarshal(out, &generic); err != nil {
		t.Fatalf("unmarshaling adapted config: %v\n%s", err, out)
	}
	var bodies []string
	var walk func(any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if val["handler"] == "static_response" {
				if body, ok := val["body"].(string); ok {
					bodies = append(bodies, body)
				}
			}
			for _, vv := range val {
				walk(vv)
			}
		case []any:
			for _, vv := range val {
				walk(vv)
			}
		}
	}
	walk(generic)
	return bodies
}

func TestEnvVarAdaptSubstitution(t *testing.T) {
	t.Setenv("CADDY_ENV_SITE", "example.com")

	input := `{$CADDY_ENV_SITE} {
	respond hello-{$CADDY_ENV_SITE}-{$CADDY_ENV_SITE}
}
`
	out, warnings, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("expected successful adaptation, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
	if !strings.Contains(string(out), `"host":["example.com"]`) {
		t.Errorf("expected substituted host, got %s", out)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 1 || bodies[0] != "hello-example.com-example.com" {
		t.Errorf("expected repeated placeholders to share one snapshot value, got %v", bodies)
	}
}

func TestEnvVarAdaptValueCannotInjectStructure(t *testing.T) {
	// spaces, quotes, newlines and braces must remain raw bytes of a
	// single argument rather than becoming extra tokens or structure
	t.Setenv("CADDY_ENV_RAW", "a b {\"\n}\n")
	t.Setenv("CADDY_ENV_NESTED", "{$CADDY_ENV_OTHER}")

	input := `example.test {
	respond {$CADDY_ENV_RAW}
	respond {$CADDY_ENV_NESTED}
}
`
	out, _, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("a value containing structural bytes must not break parsing, got %v", err)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 2 {
		t.Fatalf("expected exactly 2 respond handlers (one per line), got %d: %v", len(bodies), bodies)
	}
	if bodies[0] != "a b {\"\n}\n" {
		t.Errorf("raw value must survive verbatim in one argument, got %q", bodies[0])
	}
	if bodies[1] != "{$CADDY_ENV_OTHER}" {
		t.Errorf("a value containing another placeholder must not be re-expanded, got %q", bodies[1])
	}
}

func TestEnvVarAdaptDefaults(t *testing.T) {
	t.Setenv("CADDY_ENV_SET_EMPTY", "")

	input := `example.test {
	respond setempty=[{$CADDY_ENV_SET_EMPTY:fallback}]
	respond unset=[{$CADDY_ENV_UNSET:fall back text}]
}
`
	out, _, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("expected successful adaptation, got %v", err)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 2 {
		t.Fatalf("expected 2 bodies, got %v", bodies)
	}
	if bodies[0] != "setempty=[]" {
		t.Errorf("an explicitly set empty value must not fall back to the default, got %q", bodies[0])
	}
	if bodies[1] != "unset=[fall back text]" {
		t.Errorf("default text containing spaces must stay one argument, got %q", bodies[1])
	}
}

func TestEnvVarAdaptEscapedNotation(t *testing.T) {
	// the escaped notation must be literal even though the variable is
	// not set and the site block braces must remain structural
	input := `example.test {
	respond pre-\{$CADDY_ENV_LITERAL\}-post
}
`
	out, _, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("escaped notation must not require the variable, got %v", err)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 1 || bodies[0] != "pre-{$CADDY_ENV_LITERAL}-post" {
		t.Errorf("expected literal notation with adjacent text intact, got %v", bodies)
	}
	// the escaped variable name must not have been read from the
	// environment (there is no variable by that name, so a lookup that
	// failed would have aborted adaptation instead)
	if strings.Contains(string(out), `\{$`) || strings.Contains(string(out), `\}`) {
		t.Errorf("escaping backslashes must be consumed, got %s", out)
	}
}

func TestEnvVarAdaptFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setEnv    map[string]string
		input     string
		errSubstr string
		fileLine  string
	}{
		{
			name:      "unset without default",
			input:     "example.test {\n\trespond {$CADDY_ENV_MISSING}\n}\n",
			errSubstr: "is not set",
			fileLine:  "Caddyfile:2",
		},
		{
			name:      "empty name",
			input:     "example.test {\n\trespond {$}\n}\n",
			errSubstr: "name must not be empty",
			fileLine:  "Caddyfile:2",
		},
		{
			name:      "missing default content",
			input:     "example.test {\n\trespond {$CADDY_ENV_X:}\n}\n",
			errSubstr: "no default value",
			fileLine:  "Caddyfile:2",
		},
		{
			name:      "unclosed notation",
			input:     "example.test {\n\trespond {$CADDY_ENV_X\n}\n",
			errSubstr: "not closed",
			fileLine:  "Caddyfile:2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.setEnv {
				t.Setenv(k, v)
			}
			out, warnings, err := adaptForEnvTest(t, "Caddyfile", tc.input)
			if err == nil {
				t.Fatalf("expected adaptation to fail, got output %s", out)
			}
			if out != nil {
				t.Errorf("failure must not return partial config, got %s", out)
			}
			if len(warnings) != 0 {
				t.Errorf("failure must not return warnings, got %v", warnings)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q must contain %q", err, tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.fileLine) {
				t.Errorf("error %q must identify location %q", err, tc.fileLine)
			}
		})
	}
}

func TestEnvVarAdaptImportSharesSnapshot(t *testing.T) {
	t.Setenv("CADDY_ENV_SITE", "imported.example")

	dir := t.TempDir()
	incFile := filepath.Join(dir, "inc.caddy")
	if err := os.WriteFile(incFile, []byte("respond body-{$CADDY_ENV_SITE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	input := "{$CADDY_ENV_SITE} {\n\timport " + incFile + "\n}\n"
	out, _, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("expected successful adaptation, got %v", err)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 1 || bodies[0] != "body-imported.example" {
		t.Errorf("imported file must resolve against the call snapshot, got %v", bodies)
	}
	if !strings.Contains(string(out), `"host":["imported.example"]`) {
		t.Errorf("top-level block must resolve against the same snapshot, got %s", out)
	}
}

func TestEnvVarAdaptImportFailureReportsImportedFile(t *testing.T) {
	dir := t.TempDir()
	incFile := filepath.Join(dir, "inc.caddy")
	if err := os.WriteFile(incFile, []byte("respond {$CADDY_ENV_MISSING_IN_IMPORT}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	input := "example.test {\n\timport " + incFile + "\n}\n"
	out, warnings, err := adaptForEnvTest(t, "Caddyfile", input)
	if err == nil {
		t.Fatalf("expected failure from the imported file, got %s", out)
	}
	if out != nil || len(warnings) != 0 {
		t.Errorf("failure must return no config or warnings, got %s / %v", out, warnings)
	}
	if !strings.Contains(err.Error(), "is not set") {
		t.Errorf("expected unset-variable error, got %v", err)
	}
	if !strings.Contains(err.Error(), incFile) {
		t.Errorf("error %q must identify the imported file %s", err, incFile)
	}
}

func TestEnvVarAdaptCallIsolationAndDeterminism(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	input := `{$CADDY_ENV_VERSION} {
	respond {$CADDY_ENV_VERSION}
}
`
	t.Setenv("CADDY_ENV_VERSION", "v1")
	out1, warn1, err1 := adapter.Adapt([]byte(input), nil)
	if err1 != nil {
		t.Fatalf("first adaptation failed: %v", err1)
	}
	if !strings.Contains(string(out1), `"body":"v1"`) {
		t.Errorf("first adaptation must use v1, got %s", out1)
	}

	// changing the environment between calls must be reflected, with no
	// shared expansion cache on the reused adapter
	t.Setenv("CADDY_ENV_VERSION", "v2")
	out2, warn2, err2 := adapter.Adapt([]byte(input), nil)
	if err2 != nil {
		t.Fatalf("second adaptation failed: %v", err2)
	}
	if !strings.Contains(string(out2), `"body":"v2"`) {
		t.Errorf("second adaptation must reflect the changed environment, got %s", out2)
	}
	if strings.Contains(string(out2), `"body":"v1"`) {
		t.Errorf("second adaptation must not reuse the first call's value, got %s", out2)
	}

	// identical environment and input must yield identical bytes and warnings
	out3, warn3, err3 := adapter.Adapt([]byte(input), nil)
	if err3 != nil {
		t.Fatalf("third adaptation failed: %v", err3)
	}
	if string(out2) != string(out3) {
		t.Errorf("identical input+snapshot must yield identical bytes")
	}
	if len(warn1) != len(warn2) || len(warn2) != len(warn3) {
		t.Errorf("warning behavior must be stable across calls: %v %v %v", warn1, warn2, warn3)
	}

	// identical failure must yield identical error text across calls
	bad := "example.test {\n\trespond {$CADDY_ENV_NOPE}\n}\n"
	_, _, e1 := adapter.Adapt([]byte(bad), nil)
	_, _, e2 := adapter.Adapt([]byte(bad), nil)
	if e1 == nil || e2 == nil {
		t.Fatalf("expected failures, got %v %v", e1, e2)
	}
	if e1.Error() != e2.Error() {
		t.Errorf("identical failures must yield identical errors:\n%q\n%q", e1, e2)
	}

	// a valid submission after a failed one must fully succeed
	out4, _, err4 := adapter.Adapt([]byte(input), nil)
	if err4 != nil {
		t.Fatalf("valid adaptation after failure must succeed, got %v", err4)
	}
	if string(out4) != string(out2) {
		t.Errorf("error state must not leak between calls")
	}
}

func TestEnvVarAdaptNoPlaceholdersUnaffected(t *testing.T) {
	input := "a.example {\n\trespond hello\n}\nb.example {\n\trespond world\n}\n"
	out, _, err := adaptForEnvTest(t, "Caddyfile", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bodies := staticBodies(t, out)
	if len(bodies) != 2 || bodies[0] != "hello" || bodies[1] != "world" {
		t.Errorf("ordinary site directives must be unaffected, got %v", bodies)
	}
}
