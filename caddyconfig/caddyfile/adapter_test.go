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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// testServerType accepts any parsed server blocks without emitting
// warnings of its own, so tests can observe only formatting warnings.
type testServerType struct{}

func (testServerType) Setup([]ServerBlock, map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	return new(caddy.Config), nil, nil
}

// adaptFile runs the public Adapter entry point against body, using
// path as the main file's filename.
func adaptFile(t *testing.T, path, body string) ([]caddyconfig.Warning, error) {
	t.Helper()
	adapter := Adapter{ServerType: testServerType{}}
	_, warnings, err := adapter.Adapt([]byte(body), map[string]any{"filename": path})
	return warnings, err
}

// fmtSrc formats a Caddyfile source string the same way the formatter
// would, so test fixtures are themselves formatted.
func fmtSrc(src string) string {
	return string(Format([]byte(src)))
}

func writeAdaptFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

// unformattedBlock is a valid Caddyfile whose body directive is not
// indented; its first formatting difference is on line 2.
const unformattedBlock = ":8081 {\nrespond \"ok\"\n}\n"

func TestAdaptFormattedMainFile(t *testing.T) {
	dir := t.TempDir()
	main := fmtSrc("import sub.caddy\n:8080 {\n\tdir1\n}\n")
	writeAdaptFile(t, dir, "sub.caddy", unformattedBlock)

	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning, got %d: %+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.File != filepath.Join(dir, "sub.caddy") {
		t.Errorf("expected warning for imported file, got %q", w.File)
	}
	if w.Line != 2 {
		t.Errorf("expected first difference on line 2, got %d", w.Line)
	}
	if w.Message != formattingWarningMessage {
		t.Errorf("unexpected warning message: %q", w.Message)
	}
}

func TestAdaptNestedImports(t *testing.T) {
	dir := t.TempDir()
	// outer file imports the inner one before its own unindented block,
	// so the first difference for the outer file is on line 3.
	outer := "import inner.caddy\n:8082 {\ndir2\n}\n"
	inner := ":8083 {\ndir3\n}\n"
	outerPath := writeAdaptFile(t, dir, "outer.caddy", outer)
	innerPath := writeAdaptFile(t, dir, "inner.caddy", inner)

	main := fmtSrc("import outer.caddy\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings for nested imports, got %d: %+v", len(warnings), warnings)
	}
	// first-expansion order: the outer file is recorded before the
	// import it contains gets expanded
	if warnings[0].File != outerPath || warnings[0].Line != 3 {
		t.Errorf("expected outer file warning first at line 3, got %q:%d", warnings[0].File, warnings[0].Line)
	}
	if warnings[1].File != innerPath || warnings[1].Line != 2 {
		t.Errorf("expected inner file warning second at line 2, got %q:%d", warnings[1].File, warnings[1].Line)
	}
}

func TestAdaptGlobImports(t *testing.T) {
	dir := t.TempDir()
	first := writeAdaptFile(t, dir, "sites/01-first.caddy", unformattedBlock)
	second := writeAdaptFile(t, dir, "sites/02-second.caddy", unformattedBlock)

	main := fmtSrc("import sites/*\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings for glob matches, got %d: %+v", len(warnings), warnings)
	}
	if warnings[0].File != first || warnings[1].File != second {
		t.Errorf("expected warnings in glob order %q then %q, got %q then %q",
			first, second, warnings[0].File, warnings[1].File)
	}
}

func TestAdaptDuplicateImportWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	writeAdaptFile(t, dir, "sub.caddy", unformattedBlock)

	// same file imported twice, once with arguments: it must only
	// produce a single formatting warning
	main := fmtSrc("import sub.caddy\nimport sub.caddy arg\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for duplicate import, got %d: %+v", len(warnings), warnings)
	}
	if warnings[0].File != filepath.Join(dir, "sub.caddy") {
		t.Errorf("expected warning for the imported file, got %q", warnings[0].File)
	}
}

func TestAdaptImportSnippetIsNotFile(t *testing.T) {
	dir := t.TempDir()
	main := fmtSrc(`(common) {
	dir2
}
:8080 {
	import common
}
`)
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings with formatted main file and snippet import, got %+v", warnings)
	}
}

func TestAdaptCRLFImports(t *testing.T) {
	dir := t.TempDir()
	// CRLF newlines, otherwise properly formatted: normalization makes
	// this identical to the formatter's output, so no warning
	good := writeAdaptFile(t, dir, "good.caddy", ":8082 {\r\n\tdir1\r\n}\r\n")
	// CRLF newlines plus missing indentation: after CRLF normalization,
	// the first difference lands on line 2
	bad := writeAdaptFile(t, dir, "bad.caddy", ":8083 {\r\ndir1\r\n}\r\n")

	main := fmtSrc("import good.caddy\nimport bad.caddy\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected only the unindented CRLF file to warn, got %+v", warnings)
	}
	if warnings[0].File != bad {
		t.Errorf("expected warning for %q, got %q", bad, warnings[0].File)
	}
	if warnings[0].Line != 2 {
		t.Errorf("expected first difference on line 2 after CRLF normalization, got %d", warnings[0].Line)
	}
	if good == warnings[0].File {
		t.Errorf("properly formatted CRLF file should not warn")
	}
}

func TestAdaptImportChecksRawFileWithEnvVars(t *testing.T) {
	dir := t.TempDir()
	// formatted file whose token text comes from an environment
	// variable; lexing rewrites that span in-place, so the collected
	// contents must be the raw file rather than the expanded bytes
	t.Setenv("CADDYFILE_IMPORT_TEST_ADDR", ":8084")
	sub := ":{$CADDYFILE_IMPORT_TEST_ADDR} {\n\tdir1\n}\n"
	writeAdaptFile(t, dir, "sub.caddy", sub)

	main := fmtSrc("import sub.caddy\n:8080 {\n\tdir0\n}\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warning against the raw, formatted file, got %+v", warnings)
	}

	// the same expansion must not mask a genuine formatting difference
	writeAdaptFile(t, dir, "bad.caddy", ":{$CADDYFILE_IMPORT_TEST_ADDR} {\ndir1\n}\n")
	main = fmtSrc("import bad.caddy\n:8080 {\n\tdir0\n}\n")
	warnings, err = adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for unformatted file, got %+v", warnings)
	}
	if warnings[0].File != filepath.Join(dir, "bad.caddy") || warnings[0].Line != 2 {
		t.Errorf("expected unformatted file warning at line 2, got %q:%d", warnings[0].File, warnings[0].Line)
	}
}

func TestAdaptAllFormatted(t *testing.T) {
	dir := t.TempDir()
	main := fmtSrc("import sub.caddy\n:8080 {\n\tdir1\n}\n")
	sub := fmtSrc(unformattedBlock)
	writeAdaptFile(t, dir, "sub.caddy", string(sub))

	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("adapt failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no formatting warnings, got %+v", warnings)
	}
}

func TestAdaptParseErrorMasksNoFormattingWarning(t *testing.T) {
	dir := t.TempDir()
	// a token on the same line as an opening brace is a parse error,
	// and the extra spaces are also a formatting difference: the parse
	// error must surface with no formatting warnings
	writeAdaptFile(t, dir, "bad.caddy", ":8081 {\nfoo { a b }\n}\n")

	main := fmtSrc("import bad.caddy\n:8080 {\n\tdir1\n}\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err == nil {
		t.Fatal("expected parse error, got none")
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings when parsing fails, got %+v", warnings)
	}
}

func TestAdaptEmptyAndMissingGlobImports(t *testing.T) {
	dir := t.TempDir()
	writeAdaptFile(t, dir, "empty.caddy", "")

	main := fmtSrc("import empty.caddy\nimport missing/*\n")
	warnings, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main))
	if err != nil {
		t.Fatalf("empty file and unmatched glob should not error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty file or unmatched glob, got %+v", warnings)
	}

	// a missing non-glob import remains a hard error
	main = fmtSrc("import does-not-exist.caddy\n")
	if _, err := adaptFile(t, filepath.Join(dir, "Caddyfile"), string(main)); err == nil {
		t.Error("expected error for missing imported file, got none")
	}
}

func TestAdaptImportCollectionIsolatedPerCall(t *testing.T) {
	const goroutines = 16

	type fixture struct {
		mainPath string
		subPath  string
	}
	fixtures := make([]fixture, goroutines)
	for i := range fixtures {
		dir := t.TempDir()
		fixtures[i] = fixture{
			mainPath: filepath.Join(dir, "Caddyfile"),
			subPath:  writeAdaptFile(t, dir, "sub.caddy", unformattedBlock),
		}
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(fx fixture) {
			defer wg.Done()
			main := fmtSrc("import sub.caddy\n")
			warnings, err := adaptFile(t, fx.mainPath, string(main))
			if err != nil {
				t.Errorf("adapt failed: %v", err)
				return
			}
			if len(warnings) != 1 || warnings[0].File != fx.subPath {
				t.Errorf("expected only this call's imported file to warn, got %+v", warnings)
			}
		}(fixtures[i])
	}
	wg.Wait()
}
