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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// absKey makes a slash-style absolute path absolute on every
// platform, prefixing the temp dir's volume on Windows.
func absKey(p string) string {
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		return p
	}
	if vol := filepath.VolumeName(os.TempDir()); vol != "" {
		return filepath.Join(vol, p)
	}
	return p
}

// flattenBlocks renders parse results as a deterministic sequence
// of "block <keys>" and segment lines for byte-level comparison.
func flattenBlocks(blocks []ServerBlock) string {
	var sb strings.Builder
	for bi, block := range blocks {
		sb.WriteString("block ")
		sb.WriteString(strings.Join(block.GetKeysText(), ","))
		sb.WriteByte('\n')
		for _, seg := range block.Segments {
			for ti, tok := range seg {
				if ti > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(tok.Text)
			}
			sb.WriteByte('\n')
		}
		if bi < len(blocks)-1 {
			sb.WriteString("---\n")
		}
	}
	return sb.String()
}

func mustParseWithReader(t *testing.T, filename string, files map[string]string) string {
	t.Helper()
	blocks, err := Parse(filename, []byte(files[filename]), WithImportReader(NewMapImportReader(files)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks == nil {
		t.Fatal("successful parse returned nil blocks")
	}
	return flattenBlocks(blocks)
}

func parseErrWithReader(t *testing.T, filename string, files map[string]string) error {
	t.Helper()
	blocks, err := Parse(filename, []byte(files[filename]), WithImportReader(NewMapImportReader(files)))
	if err == nil {
		t.Fatalf("expected error, got blocks:\n%s", flattenBlocks(blocks))
	}
	if blocks != nil {
		t.Fatalf("failed parse must not return usable blocks, got:\n%s", flattenBlocks(blocks))
	}
	return err
}

func TestParseWithImportReaderRelativeAndGlobOrder(t *testing.T) {
	files := map[string]string{
		absKey("/etc/caddy/Caddyfile"): "import sites/*.caddy\n",
		absKey("/etc/caddy/sites/b.caddy"): `b.example.com {
	respond B
}
`,
		absKey("/etc/caddy/sites/a.caddy"): `a.example.com {
	respond A
}
`,
		absKey("/etc/caddy/sites/sub/c.caddy"): "c.example.com {\n\trespond C\n}\n",
	}
	got := mustParseWithReader(t, absKey("/etc/caddy/Caddyfile"), files)
	want := strings.Join([]string{
		"block a.example.com",
		"respond A",
		"---",
		"block b.example.com",
		"respond B",
		"",
	}, "\n")
	if got != want {
		t.Errorf("glob expansion order mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestParseWithImportReaderDotSegmentsCleaned(t *testing.T) {
	files := map[string]string{
		absKey("/etc/caddy/Caddyfile"):     "example.com {\n\timport ./sub/../sites/a.caddy\n}\n",
		absKey("/etc/caddy/sites/a.caddy"): "respond A\n",
	}
	got := mustParseWithReader(t, absKey("/etc/caddy/Caddyfile"), files)
	if !strings.Contains(got, "block example.com") || !strings.Contains(got, "respond A") {
		t.Errorf("relative import with dot segments failed:\n%s", got)
	}
}

func TestParseImportErrorsAreFailures(t *testing.T) {
	// unreadable target of a non-glob import
	files := map[string]string{
		absKey("/Caddyfile"): "example.com\nimport missing.caddy\n",
	}
	err := parseErrWithReader(t, absKey("/Caddyfile"), files)
	if !strings.Contains(err.Error(), "file to import not found") {
		t.Errorf("expected not-found error, got: %v", err)
	}
	if strings.Contains(err.Error(), "..") {
		t.Errorf("error path must be cleaned: %v", err)
	}

	// unterminated quote in imported file
	files = map[string]string{
		absKey("/Caddyfile"): "example.com {\n\timport bad.caddy\n}\n",
		absKey("/bad.caddy"): `respond "unclosed`,
	}
	if err := parseErrWithReader(t, absKey("/Caddyfile"), files); !strings.Contains(filepath.ToSlash(err.Error()), "/Caddyfile:2") {
		t.Errorf("error must point at the import statement, got: %v", err)
	}

	// directory target
	blocks, err := Parse(absKey("/Caddyfile"), []byte("import /etc\n"), WithImportReader(dirImportReader{}))
	if err == nil {
		t.Fatalf("expected directory import error, got %v", blocks)
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected directory error, got: %v", err)
	}
}

// dirImportReader reports its target as a directory.
type dirImportReader struct{}

func (dirImportReader) ReadFile(string) ([]byte, error) { return nil, nil }
func (dirImportReader) Stat(string) (fs.FileInfo, error) {
	return mapFileInfo{name: "etc", dir: true}, nil
}
func (dirImportReader) Glob(string) ([]string, error) { return nil, nil }

// errorReader makes Stat succeed but every ReadFile fail.
type errorReader struct{ calls atomic.Int32 }

func (r *errorReader) ReadFile(string) ([]byte, error) {
	r.calls.Add(1)
	return nil, errors.New("disk on fire")
}
func (r *errorReader) Stat(string) (fs.FileInfo, error) {
	return mapFileInfo{name: "a.caddy"}, nil
}
func (r *errorReader) Glob(string) ([]string, error) { return nil, nil }

func TestParseImportReadError(t *testing.T) {
	r := &errorReader{}
	blocks, err := Parse(absKey("/Caddyfile"), []byte("import /a.caddy\n"), WithImportReader(r))
	if err == nil {
		t.Fatalf("expected read error, got %v", blocks)
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("expected wrapped read error, got: %v", err)
	}
	if r.calls.Load() != 1 {
		t.Errorf("failed read must not be retried, got %d calls", r.calls.Load())
	}
}

// versionedReader returns a different suffix on every ReadFile,
// simulating a reader that serves another version on repeat reads.
type versionedReader struct {
	mu       sync.Mutex
	contents map[string]string
	reads    map[string]int
}

func (r *versionedReader) ReadFile(name string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.contents[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	r.reads[name]++
	return []byte(strings.Replace(body, "VERSION", "read"+itoa(r.reads[name]), 1)), nil
}
func (r *versionedReader) Stat(name string) (fs.FileInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.contents[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return mapFileInfo{name: filepath.Base(name), size: int64(len(body))}, nil
}
func (r *versionedReader) Glob(pattern string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name := range r.contents {
		matched, err := filepath.Match(pattern, name)
		if err != nil {
			return nil, err
		}
		if matched {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func TestParseSnapshotRepeatedReadsAndDuplicateImports(t *testing.T) {
	// the same file is imported twice; even though each read would
	// serve another version, both expansions must come from one read
	root := "example.com {\n\timport shared.caddy\n\timport shared.caddy\n}\n"
	r := &versionedReader{
		contents: map[string]string{
			absKey("/Caddyfile"):    root,
			absKey("/shared.caddy"): `respond "VERSION"`,
		},
		reads: map[string]int{},
	}
	blocks, err := Parse(absKey("/Caddyfile"), []byte(root), WithImportReader(r))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	segs := blocks[0].Segments
	if len(segs) != 2 {
		t.Fatalf("expected two respond segments (duplicate import), got %d: %v", len(segs), segs)
	}
	if segs[0][1].Text != segs[1][1].Text {
		t.Errorf("duplicate imports must use one snapshot: %q vs %q", segs[0][1].Text, segs[1][1].Text)
	}
	if segs[0][1].Text != "read1" {
		t.Errorf("expected first read version, got %q", segs[0][1].Text)
	}
	if r.reads["/shared.caddy"] != 1 {
		t.Errorf("expected exactly one read of shared file, got %d", r.reads["/shared.caddy"])
	}
}

func TestParseSnapshotFixedForCallAndReplaceableAfterwards(t *testing.T) {
	t.Parallel()

	root := "example.com {\n\timport inc.caddy\n}\n"
	v1 := NewMapImportReader(map[string]string{
		absKey("/Caddyfile"): root,
		absKey("/inc.caddy"): "respond ONE\n",
	})
	v2 := NewMapImportReader(map[string]string{
		absKey("/Caddyfile"): root,
		absKey("/inc.caddy"): "respond TWO\n",
	})

	blocks, err := Parse(absKey("/Caddyfile"), []byte(root), WithImportReader(v1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := blocks[0].Segments[0][1].Text; got != "ONE" {
		t.Fatalf("expected ONE, got %q", got)
	}

	// parsing the same top-level file again against another tree
	// returns only the new tree's directives; the earlier result
	// keeps its bytes
	blocks2, err := Parse(absKey("/Caddyfile"), []byte(root), WithImportReader(v2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := blocks2[0].Segments[0][1].Text; got != "TWO" {
		t.Errorf("new parse must see only new tree, got %q", got)
	}
	if got := blocks[0].Segments[0][1].Text; got != "ONE" {
		t.Errorf("earlier result changed after switching readers: %q", got)
	}
}

// hookedSnapshotReader signals once Parse has frozen its
// start-of-call snapshot.
type hookedSnapshotReader struct {
	*MapImportReader
	snapshotOnce sync.Once
	snapshotted  chan struct{}
}

func (r *hookedSnapshotReader) Snapshot() ImportReader {
	frozen := r.MapImportReader.Snapshot()
	r.snapshotOnce.Do(func() { close(r.snapshotted) })
	return frozen
}

func TestParseSnapshotImmuneToConcurrentMutation(t *testing.T) {
	t.Parallel()

	root := "example.com {\n\timport inc.caddy\n}\n"
	r := &hookedSnapshotReader{
		MapImportReader: NewMapImportReader(map[string]string{
			absKey("/Caddyfile"): root,
			absKey("/inc.caddy"): "respond SNAPSHOT\n",
		}),
		snapshotted: make(chan struct{}),
	}

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		blocks, err := Parse(absKey("/Caddyfile"), []byte(root), WithImportReader(r))
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{text: blocks[0].Segments[0][1].Text}
	}()

	<-r.snapshotted
	// the parse froze its own copy at entry; replacing the source
	// tree while parsing proceeds must not change a single byte
	r.files = NewMapImportReader(map[string]string{
		absKey("/Caddyfile"): root,
		absKey("/inc.caddy"): "respond MUTATED\n",
	}).files

	res := <-done
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.text != "SNAPSHOT" {
		t.Errorf("parse mixed in another snapshot: got %q, want SNAPSHOT", res.text)
	}
}

func TestParseDeterministicBytesAndErrors(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		absKey("/Caddyfile"): "import a.caddy\nimport b.caddy\n",
		absKey("/a.caddy"):   "a.example.com\nrespond A\n",
		absKey("/b.caddy"):   "b.example.com\nrespond B\n",
	}
	var first string
	for i := 0; i < 3; i++ {
		got := mustParseWithReader(t, absKey("/Caddyfile"), files)
		if i == 0 {
			first = got
		} else if got != first {
			t.Errorf("run %d differs:\n%s\nvs\n%s", i, first, got)
		}
	}

	// failure text is stable and shows the readable closed chain
	badFiles := map[string]string{
		absKey("/Caddyfile"): "import a.caddy\n",
		absKey("/a.caddy"):   "import b.caddy\n",
		absKey("/b.caddy"):   "import a.caddy\n",
	}
	var firstErr string
	for i := 0; i < 3; i++ {
		err := parseErrWithReader(t, absKey("/Caddyfile"), badFiles)
		if i == 0 {
			firstErr = err.Error()
		} else if err.Error() != firstErr {
			t.Errorf("run %d error differs:\n%s\nvs\n%s", i, firstErr, err.Error())
		}
	}
	if !strings.Contains(filepath.ToSlash(firstErr), "/a.caddy -> /b.caddy -> /a.caddy") {
		t.Errorf("cycle error must show readable closed chain, got: %s", firstErr)
	}
}

func TestParseParallelCallsIsolated(t *testing.T) {
	t.Parallel()

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		i := w
		go func() {
			defer wg.Done()
			files := map[string]string{
				absKey("/Caddyfile"): "example.com {\n\timport inc.caddy\n}\n",
				absKey("/inc.caddy"): "respond " + itoa(i) + "\n",
			}
			blocks, err := Parse(absKey("/Caddyfile"), []byte(files[absKey("/Caddyfile")]),
				WithImportReader(NewMapImportReader(files)))
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			if got := blocks[0].Segments[0][1].Text; got != itoa(i) {
				t.Errorf("worker %d got %q, want %q", i, got, itoa(i))
			}
		}()
	}
	wg.Wait()
}

func TestParseArgsScopeDoesNotLeakIntoNestedFiles(t *testing.T) {
	// outer passes OUTER; the nested snippet import declares no
	// args, so its {args[0]} must fail rather than see the outer value
	files := map[string]string{
		absKey("/Caddyfile"):   "import inner.caddy OUTER\n",
		absKey("/inner.caddy"): "(s) {\n\trespond {args[0]}\n}\nimport s\n",
	}
	parseErrWithReader(t, absKey("/Caddyfile"), files)

	// but a nested file import with its own args uses those
	files = map[string]string{
		absKey("/Caddyfile"):   "example.com {\n\timport inner.caddy OUTER\n}\n",
		absKey("/inner.caddy"): "import leaf.caddy INNER\n",
		absKey("/leaf.caddy"):  "respond {args[0]}\n",
	}
	got := mustParseWithReader(t, absKey("/Caddyfile"), files)
	if !strings.Contains(got, "respond INNER") {
		t.Errorf("nested import must use its own args:\n%s", got)
	}
}

func TestParseArgsSnippetScope(t *testing.T) {
	// args passed to snippet A are not visible inside snippet B
	files := map[string]string{
		absKey("/Caddyfile"): "(A) {\n\timport B\n}\n(B) {\n\trespond {args[0]}\n}\nimport A VALUE\n",
	}
	parseErrWithReader(t, absKey("/Caddyfile"), files)

	// args passed on the nested import itself work
	files["/Caddyfile"] = "(A) {\n\timport B DIRECT\n}\n(B) {\n\trespond {args[0]}\n}\nexample.com {\n\timport A\n}\n"
	got := mustParseWithReader(t, absKey("/Caddyfile"), files)
	if !strings.Contains(got, "respond DIRECT") {
		t.Errorf("direct nested snippet args should expand:\n%s", got)
	}
}

func TestParseArgsStrictnessEndToEnd(t *testing.T) {
	// undeclared placeholder fails the whole parse
	files := map[string]string{
		absKey("/Caddyfile"): "example.com {\n\timport inc.caddy\n}\n",
		absKey("/inc.caddy"): "respond {args[0]}\n",
	}
	parseErrWithReader(t, absKey("/Caddyfile"), files)

	// out-of-range placeholder fails the parse
	files["/Caddyfile"] = "example.com {\n\timport inc.caddy only\n}\n"
	files["/inc.caddy"] = "respond {args[5]}\n"
	parseErrWithReader(t, absKey("/Caddyfile"), files)

	// variadic expansion: spaces, quotes, and empty values expand
	// to separate tokens in declaration order
	files = map[string]string{
		absKey("/Caddyfile"): "example.com {\n\timport inc.caddy a \"b c\" \"\" d\n}\n",
		absKey("/inc.caddy"): "respond {args[:]}\n",
	}
	blocks, err := Parse(absKey("/Caddyfile"), []byte(files[absKey("/Caddyfile")]), WithImportReader(NewMapImportReader(files)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seg := blocks[0].Segments[0]
	want := []string{"respond", "a", "b c", "", "d"}
	if len(seg) != len(want) {
		t.Fatalf("expected %d tokens, got %v", len(want), seg)
	}
	for i, tok := range seg {
		if tok.Text != want[i] {
			t.Errorf("token %d: got %q want %q", i, tok.Text, want[i])
		}
	}

	// braces inside arg values are never expanded again
	files = map[string]string{
		absKey("/Caddyfile"): "example.com {\n\timport inc.caddy \"{host}\"\n}\n",
		absKey("/inc.caddy"): "respond {args[0]}\n",
	}
	got := mustParseWithReader(t, absKey("/Caddyfile"), files)
	if !strings.Contains(got, "respond {host}") {
		t.Errorf("braces in arg value must survive verbatim:\n%s", got)
	}
}

func TestParseNoMatchGlobStillSucceeds(t *testing.T) {
	files := map[string]string{
		absKey("/Caddyfile"): "example.com {\n\timport empty/*.caddy\n\trespond ok\n}\n",
	}
	got := mustParseWithReader(t, absKey("/Caddyfile"), files)
	if !strings.Contains(got, "respond ok") {
		t.Errorf("glob without matches must keep success semantics:\n%s", got)
	}
}

func TestParseSnippetAndSiteOrderPreserved(t *testing.T) {
	files := map[string]string{
		absKey("/Caddyfile"): `(common) {
	header X-Common yes
}
a.example.com {
	import common
	respond A
}
b.example.com {
	respond B
}
`,
	}
	got := mustParseWithReader(t, absKey("/Caddyfile"), files)
	want := "block a.example.com\nheader X-Common yes\nrespond A\n---\nblock b.example.com\nrespond B\n"
	if got != want {
		t.Errorf("order mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestParseNoImportReaderReadsOSFiles(t *testing.T) {
	t.Parallel()
	// the legacy entry point with no reader still resolves files
	// relative to the declaring file (covered against testdata)
	blocks, err := Parse("Testfile", []byte("import testdata/import_test1.txt\n"))
	if err != nil {
		t.Fatalf("legacy import failed: %v", err)
	}
	got := flattenBlocks(blocks)
	if !strings.Contains(got, "dir2") || !strings.Contains(got, "arg1") || !strings.Contains(got, "dir3") {
		t.Errorf("legacy OS import mismatch:\n%s", got)
	}
}
