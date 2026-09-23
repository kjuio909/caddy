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
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// stubServerType is a ServerType that does not inspect the parsed
// blocks, so Adapt's parse and formatting-lint behavior can be tested
// in isolation. It may be seeded with warnings to verify ordering.
type stubServerType struct {
	warnings []caddyconfig.Warning
}

func (s stubServerType) Setup(_ []ServerBlock, _ map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	return new(caddy.Config), s.warnings, nil
}

const notFormattedMessage = "Caddyfile input is not formatted; run 'caddy fmt --overwrite' to fix inconsistencies"

// writeImportFile creates name in dir with the given contents.
func writeImportFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// adaptInDir adapts body with filename set to dir/Caddyfile.
func adaptInDir(t *testing.T, dir, body string, st stubServerType) ([]byte, []caddyconfig.Warning, error) {
	t.Helper()
	return Adapter{ServerType: st}.Adapt([]byte(body), map[string]any{
		"filename": filepath.Join(dir, "Caddyfile"),
	})
}

func TestAdaptImportEnvVarsDoNotAffectFormattingCheck(t *testing.T) {
	// the parser expands {$ENV} spans in place before lexing; the
	// formatting check must run on the raw file contents, not on the
	// expanded bytes. The raw contents below are formatted, but once
	// the variable is expanded they are not.
	t.Setenv("IMPORT_BODY", "respond  \"hi\"")

	dir := t.TempDir()
	writeImportFile(t, dir, "env.caddy", "{$IMPORT_BODY}\n")
	body := ":8080 {\n\timport env.caddy\n}\n"

	_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("formatting check must use raw file contents, got warnings: %v", warnings)
	}
}

func TestAdaptFormattedMainAndImport(t *testing.T) {
	dir := t.TempDir()
	importPath := writeImportFile(t, dir, "good.caddy", "respond \"ok\"\n")
	mainBody := ":8080 {\n\timport good.caddy\n}\n"

	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for formatted input, got %v", warnings)
	}

	// sanity check: the warning would reference the absolute path the
	// parser uses, and nothing if formatted
	if _, diff := FormattingDifference(importPath, []byte("respond \"ok\"\n")); diff {
		t.Errorf("formatted file reported as different")
	}
}

func TestAdaptUnformattedSingleImport(t *testing.T) {
	dir := t.TempDir()
	importPath := writeImportFile(t, dir, "good.caddy", "respond  \"ok\"\n")
	mainBody := ":8080 {\n\timport good.caddy\n}\n"

	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning, got %d: %v", len(warnings), warnings)
	}
	want := caddyconfig.Warning{
		File:    importPath,
		Line:    1,
		Message: notFormattedMessage,
	}
	if warnings[0] != want {
		t.Errorf("expected warning %+v, got %+v", want, warnings[0])
	}
}

func TestAdaptUnformattedNestedImports(t *testing.T) {
	dir := t.TempDir()

	// mid imports leaf; both are unformatted
	writeImportFile(t, dir, "leaf.caddy", "respond  \"leaf\"\n")
	writeImportFile(t, dir, "mid.caddy", "respond  \"mid\"\nimport leaf.caddy\n")
	mainBody := ":8080 {\n\timport mid.caddy\n}\n"

	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}

	gotFiles := make([]string, len(warnings))
	for i, w := range warnings {
		gotFiles[i] = w.File
		if w.Message != notFormattedMessage {
			t.Errorf("warning %d has unexpected message: %q", i, w.Message)
		}
	}
	wantFiles := []string{
		filepath.Join(dir, "mid.caddy"),
		filepath.Join(dir, "leaf.caddy"),
	}
	if len(gotFiles) != len(wantFiles) {
		t.Fatalf("expected %d warnings, got %d: %v", len(wantFiles), len(gotFiles), gotFiles)
	}
	for i := range wantFiles {
		if gotFiles[i] != wantFiles[i] {
			t.Errorf("warning %d: expected file %s, got %s (full order: %v)", i, wantFiles[i], gotFiles[i], gotFiles)
		}
	}

	// a formatted mid still surfaces the nested leaf warning
	writeImportFile(t, dir, "mid.caddy", "respond \"mid\"\nimport leaf.caddy\n")
	_, warnings, err = adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error on second run: %v", err)
	}
	if len(warnings) != 1 || warnings[0].File != filepath.Join(dir, "leaf.caddy") {
		t.Errorf("expected only leaf.caddy warning, got %v", warnings)
	}
}

func TestAdaptUnformattedGlobImports(t *testing.T) {
	dir := t.TempDir()

	// glob matches are sorted; only b is unformatted
	writeImportFile(t, dir, "a.caddy", "respond \"a\"\n")
	bPath := writeImportFile(t, dir, "b.caddy", "respond  \"b\"\n")
	writeImportFile(t, dir, "c.caddy", "respond \"c\"\n")
	// dotfiles are skipped for patterns whose last segment is "*"
	writeImportFile(t, dir, ".hidden.caddy", "respond  \"hidden\"\n")

	mainBody := ":8080 {\n\timport *.caddy\n}\n"
	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning (b.caddy), got %v", warnings)
	}
	if warnings[0].File != bPath || warnings[0].Line != 1 {
		t.Errorf("expected b.caddy:1 warning, got %s:%d", warnings[0].File, warnings[0].Line)
	}
}

func TestAdaptImportOrderingAndDedup(t *testing.T) {
	dir := t.TempDir()

	// all four are unformatted; z_first is expanded explicitly first,
	// then the glob (sorted: a, b, z_last, z_first [dup]), then z_last
	// explicitly [dup]
	firstPath := writeImportFile(t, dir, "z_first.caddy", "respond  \"first\"\n")
	aPath := writeImportFile(t, dir, "a.caddy", "respond  \"a\"\n")
	bPath := writeImportFile(t, dir, "b.caddy", "respond  \"b\"\n")
	lastPath := writeImportFile(t, dir, "z_last.caddy", "respond  \"last\"\n")

	mainBody := `:8080 {
	import z_first.caddy
	import *.caddy
	import z_last.caddy
}
`
	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}

	want := []string{firstPath, aPath, bPath, lastPath}
	if len(warnings) != len(want) {
		t.Fatalf("expected %d warnings, got %d: %v", len(want), len(warnings), warnings)
	}
	for i, w := range warnings {
		if w.File != want[i] {
			t.Errorf("warning %d: expected %s, got %s (full order: %v)", i, want[i], w.File, warnings)
		}
	}
}

func TestAdaptRepeatedImportWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	importPath := writeImportFile(t, dir, "same.caddy", "respond  \"{args[0]}\"\n")

	// same file imported twice, with different arguments; it must
	// produce exactly one formatting warning
	mainBody := `:8080 {
	import same.caddy foo
	import same.caddy bar
}
`
	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for duplicated import, got %v", warnings)
	}
	if warnings[0].File != importPath {
		t.Errorf("expected warning for %s, got %s", importPath, warnings[0].File)
	}
}

func TestAdaptImportCRLFNormalization(t *testing.T) {
	dir := t.TempDir()

	mainBody := ":8080 {\n\timport crlf_good.caddy\n\timport crlf_bad.caddy\n\timport crlf_multiline.caddy\n}\n"
	writeImportFile(t, dir, "crlf_good.caddy", "respond \"ok\"\r\n")
	badPath := writeImportFile(t, dir, "crlf_bad.caddy", "respond  \"ok\"\r\n")
	multiPath := writeImportFile(t, dir, "crlf_multiline.caddy", "respond \"ok\"\r\nbad  line\r\n")

	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (crlf_bad, crlf_multiline), got %v", warnings)
	}
	if warnings[0].File != badPath || warnings[0].Line != 1 {
		t.Errorf("expected %s:1, got %s:%d", badPath, warnings[0].File, warnings[0].Line)
	}
	if warnings[1].File != multiPath || warnings[1].Line != 2 {
		t.Errorf("expected %s:2, got %s:%d", multiPath, warnings[1].File, warnings[1].Line)
	}
}

func TestAdaptSnippetIsNotSeparateFile(t *testing.T) {
	dir := t.TempDir()

	// file defines a snippet; main invokes the snippet by name, which
	// must not produce a warning keyed to anything other than the file
	writeImportFile(t, dir, "snippets.caddy", "(mysnip) {\n\trespond  \"{args[0]}\"\n}\n")
	snippetsPath := filepath.Join(dir, "snippets.caddy")
	mainBody := `import snippets.caddy
:8080 {
	import mysnip hi
}
`
	_, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for snippets.caddy, got %v", warnings)
	}
	if warnings[0].File != snippetsPath {
		t.Errorf("expected warning for the defining file %s, got %s", snippetsPath, warnings[0].File)
	}

	// formatted file + snippet invocation: no warnings
	writeImportFile(t, dir, "snippets.caddy", "(mysnip) {\n\trespond \"{args[0]}\"\n}\n")
	_, warnings, err = adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error on second run: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings with formatted snippet file, got %v", warnings)
	}
}

func TestAdaptWarningOrderMainThenImports(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, dir, "imp.caddy", "respond  \"imp\"\n")

	// main file's first difference is on line 3
	mainBody := ":8080 {\n\trespond \"x\"\n\tbad  y\n\timport imp.caddy\n}\n"
	mainPath := filepath.Join(dir, "Caddyfile")

	st := stubServerType{warnings: []caddyconfig.Warning{
		{File: "stub", Line: 1, Message: "from server type"},
	}}
	_, warnings, err := adaptInDir(t, dir, mainBody, st)
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %v", warnings)
	}
	if warnings[0].Message != "from server type" {
		t.Errorf("ServerType warning must come first, got %+v", warnings[0])
	}
	if warnings[1].File != mainPath || warnings[1].Line != 3 {
		t.Errorf("expected main file warning at %s:3, got %s:%d", mainPath, warnings[1].File, warnings[1].Line)
	}
	if warnings[2].File != filepath.Join(dir, "imp.caddy") || warnings[2].Line != 1 {
		t.Errorf("expected import warning last, got %s:%d", warnings[2].File, warnings[2].Line)
	}
}

func TestAdaptFormattingDoesNotChangeJSON(t *testing.T) {
	dir := t.TempDir()
	mainBody := ":8080 {\n\timport imp.caddy\n}\n"

	writeImportFile(t, dir, "imp.caddy", "respond  \"ok\"\n")
	jsonUnformatted, _, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}

	writeImportFile(t, dir, "imp.caddy", "respond \"ok\"\n")
	jsonFormatted, warnings, err := adaptInDir(t, dir, mainBody, stubServerType{})
	if err != nil {
		t.Fatalf("Adapt returned error after formatting: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings with formatted file, got %v", warnings)
	}
	if string(jsonFormatted) != string(jsonUnformatted) {
		t.Errorf("generated JSON changed with formatting:\nunformatted: %s\nformatted:   %s", jsonUnformatted, jsonFormatted)
	}
}

func TestAdaptParseFailureNotMasked(t *testing.T) {
	t.Run("syntax error in main file", func(t *testing.T) {
		dir := t.TempDir()
		writeImportFile(t, dir, "imp.caddy", "respond  \"ok\"\n")
		body := ":8080 {\n\timport imp.caddy\n" // unclosed brace
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if len(warnings) != 0 {
			t.Errorf("parse errors must not be accompanied by formatting warnings, got %v", warnings)
		}
	})

	t.Run("lex error in imported file", func(t *testing.T) {
		dir := t.TempDir()
		badPath := writeImportFile(t, dir, "bad.caddy", "<<EOF\nunterminated heredoc")
		body := ":8080 {\n\timport bad.caddy\n}\n"
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if len(warnings) != 0 {
			t.Errorf("parse errors must not be accompanied by formatting warnings, got %v", warnings)
		}
		// existing error semantics preserved
		if !strings.Contains(err.Error(), badPath) {
			t.Errorf("expected error to mention %s, got %v", badPath, err)
		}
	})

	t.Run("syntax error in imported file", func(t *testing.T) {
		dir := t.TempDir()
		writeImportFile(t, dir, "bad.caddy", ":8081 {\n:8082 {\nrespond \"x\"\n")
		body := ":8080 {\n\timport bad.caddy\n}\n"
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if len(warnings) != 0 {
			t.Errorf("parse errors must not be accompanied by formatting warnings, got %v", warnings)
		}
	})
}

func TestAdaptImportExistingSemanticsPreserved(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		writeImportFile(t, dir, "empty.caddy", "")
		writeImportFile(t, dir, "spaces.caddy", "  \n\t\n")
		body := ":8080 {\n\timport empty.caddy\n\timport spaces.caddy\n}\n"
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err != nil {
			t.Fatalf("empty imports should not error, got %v", err)
		}
		if len(warnings) != 0 {
			t.Errorf("empty files must not produce formatting warnings, got %v", warnings)
		}
	})

	t.Run("glob matches nothing", func(t *testing.T) {
		dir := t.TempDir()
		body := ":8080 {\n\timport nomatch-*.caddy\n}\n"
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err != nil {
			t.Fatalf("unmatched glob should not error, got %v", err)
		}
		if len(warnings) != 0 {
			t.Errorf("unmatched glob must not produce warnings, got %v", warnings)
		}
	})

	t.Run("missing non-glob file", func(t *testing.T) {
		dir := t.TempDir()
		body := ":8080 {\n\timport doesnotexist.caddy\n}\n"
		_, _, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil || !strings.Contains(err.Error(), "File to import not found") {
			t.Fatalf("expected 'File to import not found' error, got %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := ":8080 {\n\timport subdir\n}\n"
		_, _, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("expected 'is a directory' error, got %v", err)
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission bits are not enforced on Windows")
		}
		dir := t.TempDir()
		path := writeImportFile(t, dir, "noperm.caddy", "respond \"x\"\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

		body := ":8080 {\n\timport noperm.caddy\n}\n"
		_, _, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil {
			t.Skip("unreadable import file was readable; skipping")
		}
		if !strings.Contains(err.Error(), "Could not import") {
			t.Errorf("expected 'Could not import' error, got %v", err)
		}
	})

	t.Run("import cycle", func(t *testing.T) {
		dir := t.TempDir()
		writeImportFile(t, dir, "a.caddy", "import b.caddy\n")
		writeImportFile(t, dir, "b.caddy", "import a.caddy\n")
		body := ":8080 {\n\timport a.caddy\n}\n"
		_, warnings, err := adaptInDir(t, dir, body, stubServerType{})
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("expected import cycle error, got %v", err)
		}
		if len(warnings) != 0 {
			t.Errorf("cycle errors must not be accompanied by formatting warnings, got %v", warnings)
		}
	})
}

func TestAdaptImportCollectionIsolation(t *testing.T) {
	// per-call isolation: a previous Adapt must not leak collected files
	badDir := t.TempDir()
	writeImportFile(t, badDir, "imp.caddy", "respond  \"bad\"\n")
	if _, warnings, err := adaptInDir(t, badDir, ":8080 {\n\timport imp.caddy\n}\n", stubServerType{}); err != nil {
		t.Fatalf("first Adapt: %v", err)
	} else if len(warnings) != 1 {
		t.Fatalf("expected 1 warning in first call, got %v", warnings)
	}

	goodDir := t.TempDir()
	writeImportFile(t, goodDir, "imp.caddy", "respond \"good\"\n")
	if _, warnings, err := adaptInDir(t, goodDir, ":8080 {\n\timport imp.caddy\n}\n", stubServerType{}); err != nil {
		t.Fatalf("second Adapt: %v", err)
	} else if len(warnings) != 0 {
		t.Errorf("state leaked between calls: unexpected warnings %v", warnings)
	}

	// concurrent Adapts must keep their collection state separate
	const goroutines = 16
	const iterations = 25
	type fixture struct {
		dir      string
		imported string
	}
	fixtures := make([]fixture, goroutines)
	for g := range fixtures {
		dir := t.TempDir()
		path := writeImportFile(t, dir, "imp.caddy", "respond  \"concurrent\"\n")
		fixtures[g] = fixture{dir: dir, imported: path}
	}
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fx := fixtures[id]
			for i := 0; i < iterations; i++ {
				_, warnings, err := adaptInDir(t, fx.dir, ":8080 {\n\timport imp.caddy\n}\n", stubServerType{})
				if err != nil {
					t.Errorf("goroutine %d: Adapt error: %v", id, err)
					return
				}
				if len(warnings) != 1 || warnings[0].File != fx.imported {
					t.Errorf("goroutine %d: expected single warning for its own %s, got %v", id, fx.imported, warnings)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
