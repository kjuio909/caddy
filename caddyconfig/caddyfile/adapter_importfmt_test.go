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
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// fakeServerType adapts parsed blocks into an empty config and emits no
// warnings of its own, so the only warnings observed are formatting ones.
type fakeServerType struct{}

func (fakeServerType) Setup(_ []ServerBlock, _ map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	return new(caddy.Config), nil, nil
}

func adaptCaddyfile(t *testing.T, rootPath string, body string) ([]byte, []caddyconfig.Warning, error) {
	t.Helper()
	adapter := Adapter{ServerType: fakeServerType{}}
	return adapter.Adapt([]byte(body), map[string]any{"filename": rootPath})
}

func writeImportFile(t *testing.T, path string, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs path of %s: %v", path, err)
	}
	return abs
}

const (
	goodImportFile = "localhost {\n\trespond \"good\"\n}\n"
	// spaces instead of tabs; first difference is on line 2
	badImportFile = "localhost {\n    respond \"bad\"\n}\n"
	// CRLF newlines but otherwise well-formatted, so no warning expected
	crlfGoodImportFile = "localhost {\r\n\trespond \"crlf\"\r\n}\r\n"
	// CRLF newlines plus spaces instead of tabs; first difference is line 2
	crlfBadImportFile = "localhost {\r\n    respond \"crlf-bad\"\r\n}\r\n"
)

func TestAdaptImportFormatWarnings(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	goodA := writeImportFile(t, filepath.Join(dir, "a.txt"), goodImportFile)
	badB := writeImportFile(t, filepath.Join(dir, "b.txt"), badImportFile)
	crlfGood := writeImportFile(t, filepath.Join(dir, "crlf-good.txt"), crlfGoodImportFile)
	crlfBad := writeImportFile(t, filepath.Join(dir, "crlf-bad.txt"), crlfBadImportFile)

	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeImportFile(t, filepath.Join(emptyDir, "empty.txt"), "")
	writeImportFile(t, filepath.Join(emptyDir, "whitespace.txt"), "   \n\t\n")

	root := "import a.txt\nimport b.txt\nimport crlf-good.txt\nimport crlf-bad.txt\nimport empty/*\nimport missing-*.txt\n"

	_, warnings, err := adaptCaddyfile(t, rootPath, root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// b.txt and crlf-bad.txt are the only ones that are misformatted;
	// snippet-free glob imports, empty files and unmatched globs are
	// not reported
	if len(warnings) != 2 {
		var files []string
		for _, w := range warnings {
			files = append(files, w.File)
		}
		t.Fatalf("expected exactly 2 formatting warnings, got %d: %v", len(warnings), files)
	}

	// warnings follow first-read order: b.txt before crlf-bad.txt
	if warnings[0].File != badB || warnings[1].File != crlfBad {
		t.Errorf("expected warnings for [%s %s], got [%s %s]",
			badB, crlfBad, warnings[0].File, warnings[1].File)
	}

	for i, w := range warnings {
		if w.Line != 2 {
			t.Errorf("warning %d: expected first difference on line 2, got %d", i, w.Line)
		}
		if w.Directive != "" {
			t.Errorf("warning %d: expected empty directive, got %q", i, w.Directive)
		}
		if w.Message != "Caddyfile input is not formatted; run 'caddy fmt --overwrite' to fix inconsistencies" {
			t.Errorf("warning %d: unexpected message %q", i, w.Message)
		}
		if !filepath.IsAbs(w.File) {
			t.Errorf("warning %d: expected absolute file path, got %q", i, w.File)
		}
	}

	// the well-formatted files, including the CRLF-only one, produced none
	for _, w := range warnings {
		if w.File == goodA || w.File == crlfGood {
			t.Errorf("did not expect a warning for %s", w.File)
		}
	}
}

func TestAdaptRootWarningFirst(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")
	badImport := writeImportFile(t, filepath.Join(dir, "bad.txt"), badImportFile)

	// root itself is also misformatted: spaces instead of tabs
	root := "localhost {\n    import bad.txt\n}\n"

	_, warnings, err := adaptCaddyfile(t, rootPath, root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (root + import), got %d", len(warnings))
	}
	if warnings[0].File != rootPath {
		t.Errorf("expected first warning for root file %q, got %q", rootPath, warnings[0].File)
	}
	if warnings[0].Line != 2 {
		t.Errorf("expected root warning on line 2, got %d", warnings[0].Line)
	}
	if warnings[1].File != badImport {
		t.Errorf("expected second warning for %q, got %q", badImport, warnings[1].File)
	}
}

func TestAdaptImportDedupAndOrder(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// glob matches in lexical order: a.txt, b.txt, c.txt
	badA := writeImportFile(t, filepath.Join(filesDir, "a.txt"), badImportFile)
	badB := writeImportFile(t, filepath.Join(filesDir, "b.txt"), badImportFile)
	badC := writeImportFile(t, filepath.Join(filesDir, "c.txt"), badImportFile)

	// c.txt is read explicitly first, then the glob reads a.txt, b.txt
	// and encounters c.txt a second time; the duplicate must be suppressed
	root := "import files/c.txt\nimport files/*.txt\n"

	_, warnings, err := adaptCaddyfile(t, rootPath, root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	got := make([]string, len(warnings))
	for i, w := range warnings {
		got[i] = w.File
	}
	want := []string{badC, badA, badB}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected stable first-read order %v, got %v", want, got)
	}
}

func TestAdaptRecursiveImports(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	// a.txt imports b.txt; both are misformatted (leading whitespace)
	badB := writeImportFile(t, filepath.Join(dir, "b.txt"), "   respond \"b\"\n")
	badA := writeImportFile(t, filepath.Join(dir, "a.txt"),
		"import b.txt\n   respond \"a\"\n")

	root := "localhost {\n\timport a.txt\n}\n"

	_, warnings, err := adaptCaddyfile(t, rootPath, root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected warnings for both files in the import chain, got %d", len(warnings))
	}
	// a.txt is fully read before its tokens are spliced in and its own
	// import directive is expanded, so it precedes b.txt
	if warnings[0].File != badA || warnings[1].File != badB {
		t.Errorf("expected order [%s %s], got [%s %s]",
			badA, badB, warnings[0].File, warnings[1].File)
	}
	if warnings[0].Line != 2 {
		t.Errorf("expected a.txt difference on line 2, got %d", warnings[0].Line)
	}
	if warnings[1].Line != 1 {
		t.Errorf("expected b.txt difference on line 1, got %d", warnings[1].Line)
	}
}

func TestAdaptSnippetImportNotTracked(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	// snippet defined in, and imported from, the root file: no file-based
	// import happens, so no extra warning beyond the root check
	root := "(mysnippet) {\n\trespond \"snip\"\n}\n\nlocalhost {\n\timport mysnippet\n}\n"

	result, warnings, err := adaptCaddyfile(t, rootPath, root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no formatting warnings, got %+v", warnings)
	}
	if len(result) == 0 {
		t.Error("expected JSON config, got empty bytes")
	}
}

func TestAdaptImportErrorPreservesChain(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	// a.txt imports b.txt; b.txt contains an unterminated heredoc, which
	// is a lexical error raised while expanding a.txt's import
	brokenB := writeImportFile(t, filepath.Join(dir, "b.txt"),
		"respond <<END\nunterminated\n")
	middleA := writeImportFile(t, filepath.Join(dir, "a.txt"), "import b.txt\n")

	root := "localhost {\n\timport a.txt\n}\n"

	result, _, err := adaptCaddyfile(t, rootPath, root)
	if err == nil {
		t.Fatal("expected parse error from broken import, got none")
	}
	if result != nil {
		t.Errorf("expected no partial JSON on error, got %s", result)
	}
	if !strings.Contains(err.Error(), brokenB) {
		t.Errorf("expected error to name imported file %q, got: %v", brokenB, err)
	}
	if !strings.Contains(err.Error(), middleA) {
		t.Errorf("expected error to preserve full import chain through %q, got: %v", middleA, err)
	}
}

// TestParseAllImportTracking exercises tracking at the parse level,
// including preservation of raw bytes despite environment-variable
// expansion and unchanged parse results versus Parse.
func TestParseAllImportTracking(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "Caddyfile")

	// contains an env-var span, which is spliced in place during parsing
	envFile := writeImportFile(t, filepath.Join(dir, "env.txt"),
		"localhost {\n\trespond \"{$TEST_CADDYFILE_FMT_VAR}\"\n}\n")
	emptyFile := writeImportFile(t, filepath.Join(dir, "empty.txt"), "")

	root := "import env.txt\nimport empty.txt\nimport env.txt\n"
	blocks, imported, err := parseWithImports(rootPath, []byte(root))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("expected parsed server blocks")
	}
	if len(imported) != 1 {
		t.Fatalf("expected env.txt tracked exactly once (empty file skipped, duplicate suppressed), got %d", len(imported))
	}
	if imported[0].absPath != envFile {
		t.Errorf("expected tracked path %q, got %q", envFile, imported[0].absPath)
	}
	if !strings.Contains(string(imported[0].body), "{$TEST_CADDYFILE_FMT_VAR}") {
		t.Errorf("expected raw bytes preserved before env var expansion, got: %q", imported[0].body)
	}
	if strings.Contains(string(imported[0].body), "expanded-value") {
		t.Errorf("tracked bytes should never contain expanded env vars, got: %q", imported[0].body)
	}
	if imported[0].absPath == emptyFile {
		t.Error("empty imported files must not be tracked")
	}

	// tracking must not change parse results relative to Parse
	t.Setenv("TEST_CADDYFILE_FMT_VAR", "expanded-value")
	blocksTracked, _, err := parseWithImports(rootPath, []byte(root))
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	blocksPlain, err := Parse(rootPath, []byte(root))
	if err != nil {
		t.Fatalf("plain parse: %v", err)
	}
	if flattenBlocks(blocksTracked) != flattenBlocks(blocksPlain) {
		t.Errorf("parseWithImports changed parse results:\ntracked: %s\nplain:    %s",
			flattenBlocks(blocksTracked), flattenBlocks(blocksPlain))
	}
}

func flattenBlocks(blocks []ServerBlock) string {
	var sb strings.Builder
	for _, block := range blocks {
		for _, key := range block.Keys {
			sb.WriteString(key.Text)
			sb.WriteByte('|')
		}
		for _, segment := range block.Segments {
			for _, token := range segment {
				sb.WriteString(token.Text)
				sb.WriteByte(',')
			}
			sb.WriteByte(';')
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
