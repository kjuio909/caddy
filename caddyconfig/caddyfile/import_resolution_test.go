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

// captureServerType is a minimal ServerType used to drive the public
// Adapter: it records the parsed blocks and never produces its own error.
type captureServerType struct {
	blocks *[]ServerBlock
}

func (c captureServerType) Setup(blocks []ServerBlock, _ map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	*c.blocks = blocks
	return new(caddy.Config), nil, nil
}

// writeImportFile is a small helper that writes content to a file under
// the test's temporary directory, creating parent directories as needed.
func writeImportFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directories for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("creating directories for %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating symlinks is not supported here: %v", err)
	}
}

// parseInDir parses body as if it were the root Caddyfile located at
// <dir>/Caddyfile, so relative imports resolve inside dir.
func parseInDir(t *testing.T, dir, body string) ([]ServerBlock, error) {
	t.Helper()
	root := filepath.Join(dir, "Caddyfile")
	return Parse(root, []byte(body))
}

func blockKeys(blocks []ServerBlock) []string {
	var keys []string
	for _, b := range blocks {
		keys = append(keys, b.GetKeysText()...)
	}
	return keys
}

// site returns a minimal site block with host as its address, so each
// imported host becomes its own server block during parsing.
func site(host string) string { return host + " {\n}\n" }

// A single glob statement may hit the same regular file through several
// matches (the file itself and a symlink alias). It must expand exactly
// once, in an order based on the canonical path, and must not be reported
// as a cycle.
func TestImportGlobDeduplicatesByRealFile(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "a.conf"), site("host-a"))
	writeImportFile(t, filepath.Join(dir, "m.conf"), site("host-m"))
	symlinkOrSkip(t, filepath.Join(dir, "a.conf"), filepath.Join(dir, "z-alias.conf"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := blockKeys(blocks)
	want := []string{"host-a", "host-m"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v (each file once, canonical order), got %v", want, got)
	}
}

// A plain file matched twice within one glob through two aliases still
// expands only once.
func TestImportGlobDeduplicatesTwoAliases(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "real", "a.conf"), site("host-a"))
	symlinkOrSkip(t, filepath.Join(dir, "real", "a.conf"), filepath.Join(dir, "one.conf"))
	symlinkOrSkip(t, filepath.Join(dir, "real", "a.conf"), filepath.Join(dir, "two.conf"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*.conf")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := blockKeys(blocks)
	if len(got) != 1 || got[0] != "host-a" {
		t.Errorf("expected a single [host-a], got %v", got)
	}
}

// Expansion order follows the canonical (real) path, not the order the
// filesystem returned matches or the lexical spelling of an alias. Here
// the alias sorts first lexically but its target sorts after the other
// real file.
func TestImportGlobOrderFollowsRealPath(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "real", "zzz.conf"), site("host-zzz"))
	writeImportFile(t, filepath.Join(dir, "b.conf"), site("host-b"))
	// "a-link" sorts before "b" lexically, but resolves to real/zzz which
	// sorts after b by canonical path.
	symlinkOrSkip(t, filepath.Join(dir, "real", "zzz.conf"), filepath.Join(dir, "a-link.conf"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*.conf")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := blockKeys(blocks)
	want := []string{"host-b", "host-zzz"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected canonical-path order %v, got %v", want, got)
	}
}

// Explicitly writing the same import twice are two independent statements
// and must keep the original semantics (expanded twice), even though a
// single glob dedupes.
func TestImportExplicitDuplicateNotDeduped(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.conf")
	writeImportFile(t, target, site("host-a"))

	body := "import " + target + "\nimport " + target + "\n"
	blocks, err := parseInDir(t, dir, body)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := blockKeys(blocks)
	if len(got) != 2 || got[0] != "host-a" || got[1] != "host-a" {
		t.Errorf("expected the file to expand twice, got %v", got)
	}
}

// A file reached through a symlink must resolve its own relative imports
// relative to the file that actually declares them (the symlink target's
// directory), not the alias's directory.
func TestImportRelativeToDeclaringFileThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "real", "a.conf"), "import b.conf\n"+site("host-a"))
	writeImportFile(t, filepath.Join(dir, "real", "b.conf"), site("host-b"))
	symlinkOrSkip(t, filepath.Join(dir, "real", "a.conf"), filepath.Join(dir, "alias", "link.conf"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "alias", "link.conf")+"\n")
	if err != nil {
		t.Fatalf("expected relative import to resolve against the real file, got: %v", err)
	}

	got := blockKeys(blocks)
	want := []string{"host-b", "host-a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// A relative import containing ".." still resolves against the declaring
// file, including across nested imports.
func TestImportRelativeWithDotDot(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "proj", "sub", "b.conf"), site("host-b"))
	writeImportFile(t, filepath.Join(dir, "proj", "a.conf"), "import sub/../sub/b.conf\n"+site("host-a"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "proj", "a.conf")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := blockKeys(blocks)
	want := []string{"host-b", "host-a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// A self-import reached through a symlink alias is a real cycle and must
// fail fast with a diagnostic naming the file, not a recursion-depth or
// generic I/O error.
func TestImportCycleThroughSymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	writeImportFile(t, a, "import link.conf\n"+site("host-a"))
	symlinkOrSkip(t, a, filepath.Join(dir, "link.conf"))

	_, err := parseInDir(t, dir, "import "+a+"\n")
	if err == nil {
		t.Fatal("expected a cycle error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cycle") {
		t.Errorf("expected a cycle diagnostic, got: %v", err)
	}
	if !strings.Contains(msg, "a.conf") {
		t.Errorf("expected the diagnostic to name the offending file a.conf, got: %v", err)
	}
	if !strings.Contains(msg, "link.conf") {
		t.Errorf("expected the diagnostic to name the import statement's target, got: %v", err)
	}
}

// A multi-file loop closing through a symlink alias must report the full
// chain of files involved.
func TestImportCycleChainThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	writeImportFile(t, a, "import b.conf\n")
	writeImportFile(t, filepath.Join(dir, "b.conf"), "import link.conf\n")
	symlinkOrSkip(t, a, filepath.Join(dir, "link.conf"))

	_, err := parseInDir(t, dir, "import "+a+"\n")
	if err == nil {
		t.Fatal("expected a cycle error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"a.conf", "b.conf", "link.conf"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected diagnostic to contain %q, got: %v", want, err)
		}
	}
	// the chain closes back to its start
	chain := strings.SplitN(msg, " (while", 2)[0]
	if !strings.HasPrefix(chain, "import cycle detected: ") {
		t.Errorf("expected a cycle-chain prefix, got: %v", err)
	}
	if !strings.HasSuffix(chain, "a.conf") {
		t.Errorf("expected the chain to loop back to a.conf, got: %v", err)
	}
}

// A loop formed purely by ".." spellings must be detected even though
// the lexical paths differ in writing.
func TestImportCycleThroughDotDot(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "proj", "a.conf")
	b := filepath.Join(dir, "proj", "sub", "b.conf")
	writeImportFile(t, a, "import sub/b.conf\n")
	writeImportFile(t, b, "import ../a.conf\n")

	_, err := parseInDir(t, dir, "import "+a+"\n")
	if err == nil {
		t.Fatal("expected a cycle error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cycle") || !strings.Contains(msg, "a.conf") || !strings.Contains(msg, "b.conf") {
		t.Errorf("expected a chain naming a.conf and b.conf, got: %v", err)
	}
}

// The classic recursive self-import still fails, through the new chained
// diagnostic.
func TestImportRecursiveCycleChain(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	writeImportFile(t, a, "import a.conf\n")

	_, err := parseInDir(t, dir, "import "+a+"\n")
	if err == nil {
		t.Fatal("expected a cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected a cycle diagnostic, got: %v", err)
	}
}

// The same regular file imported at distinct points of a diamond-shaped
// graph is not a cycle and must succeed. It expands once per explicit
// import statement.
func TestImportDiamondSameFileNotCycle(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "d.conf"), site("host-d"))
	writeImportFile(t, filepath.Join(dir, "b.conf"), "import d.conf\n")
	writeImportFile(t, filepath.Join(dir, "c.conf"), "import d.conf\n")
	writeImportFile(t, filepath.Join(dir, "a.conf"), "import b.conf\nimport c.conf\n")

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "a.conf")+"\n")
	if err != nil {
		t.Fatalf("expected a shared leaf not to be a cycle, got: %v", err)
	}

	got := blockKeys(blocks)
	if len(got) != 2 || got[0] != "host-d" || got[1] != "host-d" {
		t.Errorf("expected two host-d expansions, got %v", got)
	}
}

// Directories remain an error using the existing category.
func TestImportDirectoryStillErrors(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := parseInDir(t, dir, "import "+sub+"\n")
	if err == nil {
		t.Fatal("expected an error importing a directory, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected a directory error category, got: %v", err)
	}
}

// A glob matching nothing is still a non-error; an explicit missing file
// is still an error.
func TestImportNoMatchCategories(t *testing.T) {
	dir := t.TempDir()
	if _, err := parseInDir(t, dir, "import "+filepath.Join(dir, "nope", "*.conf")+"\n"); err != nil {
		t.Errorf("expected a non-matching glob to be a non-error, got: %v", err)
	}
	if _, err := parseInDir(t, dir, "import "+filepath.Join(dir, "nope", "file.conf")+"\n"); err == nil {
		t.Error("expected a missing explicit file to be an error, got nil")
	}
}

// Repeatedly adapting the same file tree yields identical expansion
// order; a cycle yields the identical error chain.
func TestImportDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "a.conf"), site("host-a"))
	writeImportFile(t, filepath.Join(dir, "m.conf"), site("host-m"))
	symlinkOrSkip(t, filepath.Join(dir, "a.conf"), filepath.Join(dir, "z-alias.conf"))
	root := filepath.Join(dir, "Caddyfile")
	body := []byte("import " + filepath.Join(dir, "*") + "\n")

	var first []string
	for run := 0; run < 3; run++ {
		blocks, err := Parse(root, body)
		if err != nil {
			t.Fatalf("run %d: expected no error, got: %v", run, err)
		}
		keys := blockKeys(blocks)
		if first == nil {
			first = keys
			continue
		}
		if strings.Join(keys, ",") != strings.Join(first, ",") {
			t.Errorf("run %d: expected %v, got %v", run, first, keys)
		}
	}

	// error chain stability
	cyc := filepath.Join(dir, "cyc.conf")
	writeImportFile(t, cyc, "import link2.conf\n")
	symlinkOrSkip(t, cyc, filepath.Join(dir, "link2.conf"))
	cycleBody := []byte("import " + cyc + "\n")
	var firstErr string
	for run := 0; run < 3; run++ {
		_, err := Parse(root, cycleBody)
		if err == nil {
			t.Fatalf("run %d: expected a cycle error, got nil", run)
		}
		if firstErr == "" {
			firstErr = err.Error()
			continue
		}
		if err.Error() != firstErr {
			t.Errorf("run %d: expected identical error\nwant: %s\ngot:  %s", run, firstErr, err.Error())
		}
	}
}

// Exercise the public Adapter.Adapt entry point: a glob that hits the
// same real file through an alias expands it once and succeeds, while a
// cycle means the whole adaptation fails (the caller never receives a
// partially expanded successful result).
func TestAdapterImportResolution(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Caddyfile")
	writeImportFile(t, filepath.Join(dir, "a.conf"), site("host-a"))
	writeImportFile(t, filepath.Join(dir, "m.conf"), site("host-m"))
	symlinkOrSkip(t, filepath.Join(dir, "a.conf"), filepath.Join(dir, "z-alias.conf"))

	var captured []ServerBlock
	adapter := Adapter{ServerType: captureServerType{blocks: &captured}}

	// success: alias + real file collapsed, order follows real paths
	out, _, err := adapter.Adapt([]byte("import "+filepath.Join(dir, "*")+"\n"),
		map[string]any{"filename": root})
	if err != nil {
		t.Fatalf("expected successful adaptation, got: %v", err)
	}
	if len(out) == 0 {
		t.Error("expected non-empty JSON output, got empty")
	}
	if got := blockKeys(captured); strings.Join(got, ",") != "host-a,host-m" {
		t.Errorf("expected hosts [host-a host-m], got %v", got)
	}

	// failure: any imported file triggering a cycle fails the whole adapt
	cyc := filepath.Join(dir, "cyc.conf")
	writeImportFile(t, cyc, "import alias2.conf\n")
	symlinkOrSkip(t, cyc, filepath.Join(dir, "alias2.conf"))
	captured = nil
	out, _, err = adapter.Adapt([]byte("import "+cyc+"\n"),
		map[string]any{"filename": root})
	if err == nil {
		t.Fatal("expected the whole adaptation to fail on a cycle, got nil")
	}
	if out != nil {
		t.Errorf("expected no partial result on failure (got %d bytes)", len(out))
	}
	if captured != nil {
		t.Errorf("expected Setup not to run on a failed expansion (partial result)")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected a cycle diagnostic, got: %v", err)
	}
}

// A dangling symlink keeps the existing "not found / could not import"
// error category rather than surfacing a path-resolution error.
func TestAdapterDanglingSymlinkCategory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Caddyfile")
	symlinkOrSkip(t, filepath.Join("does-not-exist"), filepath.Join(dir, "dangling.conf"))

	var captured []ServerBlock
	adapter := Adapter{ServerType: captureServerType{blocks: &captured}}
	_, _, err := adapter.Adapt([]byte("import "+filepath.Join(dir, "dangling.conf")+"\n"),
		map[string]any{"filename": root})
	if err == nil {
		t.Fatal("expected an error for a dangling symlink, got nil")
	}
	if strings.Contains(err.Error(), "Could not resolve import") {
		t.Errorf("dangling symlink should keep existing I/O error category, got: %v", err)
	}
}

// fsCaseInsensitive reports whether dir folds file-name case, observed from
// the live filesystem rather than from the target operating system. A
// default APFS or NTFS/FAT volume folds case even when the kernel is Unix-like.
func fsCaseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probeName := ".case_probe_abc"
	probe := filepath.Join(dir, probeName)
	writeImportFile(t, probe, "")
	info, err := os.Stat(probe)
	if err != nil {
		t.Skipf("cannot probe filesystem case rules: %v", err)
	}
	flipped, err := os.Stat(filepath.Join(dir, strings.ToUpper(probeName)))
	if err != nil {
		return false
	}
	return os.SameFile(info, flipped)
}

// On a case-insensitive volume, aliases differing only in case (here a symlink
// whose target is spelled with different case) still denote the same real
// file: a single glob must expand it once and must not depend on a fixed
// per-OS folding rule.
func TestImportGlobDeduplicatesCaseOnlyAlias(t *testing.T) {
	dir := t.TempDir()
	if !fsCaseInsensitive(t, dir) {
		t.Skip("requires a case-insensitive filesystem")
	}
	writeImportFile(t, filepath.Join(dir, "real", "a.conf"), site("host-a"))
	writeImportFile(t, filepath.Join(dir, "m.conf"), site("host-m"))
	// the target is spelled in a case that does not exist on disk
	symlinkOrSkip(t, filepath.Join(dir, "REAL", "A.CONF"), filepath.Join(dir, "z-link.conf"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*.conf")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	got := blockKeys(blocks)
	// the alias canonicalizes to real/a.conf, which sorts after m.conf;
	// the important assertion is that the alias adds no extra expansion
	want := []string{"host-m", "host-a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v (case alias collapsed, canonical order), got %v", want, got)
	}
}

// A cycle closed through a case-only alias on a case-insensitive volume must
// be detected and report the same chain on immediate retries.
func TestImportCycleThroughCaseOnlyAlias(t *testing.T) {
	dir := t.TempDir()
	if !fsCaseInsensitive(t, dir) {
		t.Skip("requires a case-insensitive filesystem")
	}
	a := filepath.Join(dir, "a.conf")
	writeImportFile(t, a, "import link.conf\n"+site("host-a"))
	// the alias points at the same file using a different-case spelling
	symlinkOrSkip(t, filepath.Join(dir, "A.CONF"), filepath.Join(dir, "link.conf"))

	body := []byte("import " + a + "\n")
	var firstErr string
	for run := 0; run < 2; run++ {
		_, err := Parse(filepath.Join(dir, "Caddyfile"), body)
		if err == nil {
			t.Fatalf("run %d: expected a cycle error, got nil", run)
		}
		if run == 0 {
			firstErr = err.Error()
			continue
		}
		if err.Error() != firstErr {
			t.Errorf("expected identical error chain on retry\nwant: %s\ngot:  %s", firstErr, err.Error())
		}
	}
	if !strings.Contains(firstErr, "cycle") ||
		!strings.Contains(firstErr, "a.conf") ||
		!strings.Contains(firstErr, "link.conf") {
		t.Errorf("expected a cycle chain naming a.conf and the link, got: %s", firstErr)
	}
}

// On a case-sensitive volume, two files whose names differ only in case are
// genuinely different files and must both expand; case folding must never be
// forced by the implementation.
func TestImportCaseDistinctOnCaseSensitiveFS(t *testing.T) {
	dir := t.TempDir()
	if fsCaseInsensitive(t, dir) {
		t.Skip("requires a case-sensitive filesystem")
	}
	writeImportFile(t, filepath.Join(dir, "CASE.conf"), site("host-upper"))
	writeImportFile(t, filepath.Join(dir, "case.conf"), site("host-lower"))

	blocks, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*.conf")+"\n")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	got := blockKeys(blocks)
	if len(got) != 2 {
		t.Fatalf("expected both case-distinct files to expand, got %v", got)
	}
	upper := strings.Join(got, ",")
	if upper != "host-upper,host-lower" && upper != "host-lower,host-upper" {
		t.Errorf("expected both hosts in a stable order, got %v", got)
	}
}

// A failed adaptation leaves no state that changes later adaptations:
// failing, then succeeding, then failing again through the same Adapter
// yields the same error category and chain both times.
func TestAdapterFailureIsStateless(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Caddyfile")
	writeImportFile(t, filepath.Join(dir, "a.conf"), site("host-a"))
	writeImportFile(t, filepath.Join(dir, "cyc.conf"), "import cyc-link.conf\n")
	symlinkOrSkip(t, filepath.Join(dir, "cyc.conf"), filepath.Join(dir, "cyc-link.conf"))

	var captured []ServerBlock
	adapter := Adapter{ServerType: captureServerType{blocks: &captured}}
	failBody := []byte("import " + filepath.Join(dir, "cyc.conf") + "\n")
	goodBody := []byte("import " + filepath.Join(dir, "a.conf") + "\n")

	out, _, err := adapter.Adapt(failBody, map[string]any{"filename": root})
	if err == nil {
		t.Fatal("expected the first adaptation to fail, got nil")
	}
	if out != nil {
		t.Errorf("expected no partial output on failure, got %d bytes", len(out))
	}
	if captured != nil {
		t.Error("expected Setup not to run on a failed adaptation")
	}
	firstErr := err.Error()

	if out, _, err := adapter.Adapt(goodBody, map[string]any{"filename": root}); err != nil {
		t.Fatalf("expected a later adaptation to succeed, got: %v", err)
	} else if len(out) == 0 {
		t.Error("expected non-empty output from the successful adaptation")
	}
	if got := blockKeys(captured); len(got) != 1 || got[0] != "host-a" {
		t.Errorf("expected [host-a] after recovery, got %v", got)
	}

	captured = nil
	out, _, err = adapter.Adapt(failBody, map[string]any{"filename": root})
	if err == nil {
		t.Fatal("expected the repeated failure to fail again, got nil")
	}
	if out != nil {
		t.Errorf("expected no partial output on the repeated failure, got %d bytes", len(out))
	}
	if captured != nil {
		t.Error("expected Setup not to run on the repeated failure")
	}
	if err.Error() != firstErr {
		t.Errorf("expected the identical error chain after success\nwant: %s\ngot:  %s", firstErr, err.Error())
	}
}
