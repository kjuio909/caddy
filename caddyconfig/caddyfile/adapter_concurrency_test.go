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

//go:build !windows

package caddyfile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// recordingServerType renders the expanded server blocks into the adapted
// JSON body: every expanded site address and every file that contributed
// tokens is listed. That makes cross-tree contamination observable straight
// from the returned body, without reaching into parser internals. The
// warning it returns is derived solely from the current call's expansion.
type recordingServerType struct{}

func (recordingServerType) Setup(blocks []ServerBlock, _ map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	hosts := blockKeys(blocks)
	fileSet := make(map[string]struct{})
	for _, b := range blocks {
		for _, seg := range b.Segments {
			for _, tok := range seg {
				fileSet[tok.File] = struct{}{}
			}
		}
		for _, tok := range b.Keys {
			fileSet[tok.File] = struct{}{}
		}
	}
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	raw, err := json.Marshal(struct {
		Hosts []string `json:"hosts"`
		Files []string `json:"files"`
	}{Hosts: hosts, Files: files})
	if err != nil {
		return nil, nil, err
	}
	warnings := []caddyconfig.Warning{
		{Message: "expanded hosts: " + strings.Join(hosts, ",")},
	}
	return &caddy.Config{AppsRaw: caddy.ModuleMap{"importisotest": raw}}, warnings, nil
}

type adaptOutcome struct {
	out      []byte
	warnings []caddyconfig.Warning
	err      error
}

func adaptAt(t *testing.T, adapter Adapter, root string, body []byte) adaptOutcome {
	t.Helper()
	out, warnings, err := adapter.Adapt(body, map[string]any{"filename": root})
	return adaptOutcome{out: out, warnings: warnings, err: err}
}

// requireSuccess asserts the outcome is the expected successful adaptation:
// identical body and warnings, no error, and no trace of the other tree.
func requireSuccess(t *testing.T, label string, got, want adaptOutcome, foreignDir, foreignMarker string) {
	t.Helper()
	if got.err != nil {
		t.Fatalf("%s: expected success, got error: %v", label, got.err)
	}
	if want.err != nil {
		t.Fatalf("%s: reference adaptation unexpectedly failed: %v", label, want.err)
	}
	if !bytes.Equal(got.out, want.out) {
		t.Errorf("%s: body drift\nwant: %s\ngot:  %s", label, want.out, got.out)
	}
	if !reflect.DeepEqual(got.warnings, want.warnings) {
		t.Errorf("%s: warnings drift\nwant: %v\ngot:  %v", label, want.warnings, got.warnings)
	}
	if bytes.Contains(got.out, []byte(foreignDir)) {
		t.Errorf("%s: body leaks the other tree's path %q", label, foreignDir)
	}
	if foreignMarker != "" && bytes.Contains(got.out, []byte(foreignMarker)) {
		t.Errorf("%s: body leaks the other tree's marker %q", label, foreignMarker)
	}
	for _, w := range got.warnings {
		if strings.Contains(w.File+w.Message, foreignDir) || strings.Contains(w.File+w.Message, foreignMarker) {
			t.Errorf("%s: warning leaks the other tree: %+v", label, w)
		}
	}
}

// requireFailure asserts the outcome is the expected failed adaptation: no
// partially expanded body, an identical error chain, and diagnostics that
// only mention the failing tree.
func requireFailure(t *testing.T, label string, got, want adaptOutcome, ownDir, foreignDir string, wantSnippets ...string) {
	t.Helper()
	if got.err == nil {
		t.Fatalf("%s: expected failure, got body %s", label, got.out)
	}
	if got.out != nil {
		t.Errorf("%s: failed call returned a partial body (%d bytes)", label, len(got.out))
	}
	if want.err == nil {
		t.Fatalf("%s: reference adaptation unexpectedly succeeded", label)
	}
	if got.err.Error() != want.err.Error() {
		t.Errorf("%s: error drift\nwant: %s\ngot:  %s", label, want.err, got.err)
	}
	if !reflect.DeepEqual(got.warnings, want.warnings) {
		t.Errorf("%s: warnings on failure drift\nwant: %v\ngot:  %v", label, want.warnings, got.warnings)
	}
	msg := got.err.Error()
	if !strings.Contains(msg, ownDir) {
		t.Errorf("%s: error does not name its own tree %q: %s", label, ownDir, msg)
	}
	if strings.Contains(msg, foreignDir) {
		t.Errorf("%s: error leaks the other tree's path %q: %s", label, foreignDir, msg)
	}
	for _, snippet := range wantSnippets {
		if !strings.Contains(msg, snippet) {
			t.Errorf("%s: error %q missing %q", label, msg, snippet)
		}
	}
}

// buildGoodImportTree builds a tree exercising nested imports, a glob and
// ".." relative imports. The expansion order is:
// A-nested (via the ".." nested import), A-p1, A-p2 (glob, canonical
// order), A-a.
func buildGoodImportTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "proj", "sub", "nested.conf"), site("A-nested"))
	writeImportFile(t, filepath.Join(dir, "proj", "parts", "p1.conf"), site("A-p1"))
	writeImportFile(t, filepath.Join(dir, "proj", "parts", "p2.conf"), site("A-p2"))
	writeImportFile(t, filepath.Join(dir, "proj", "a.conf"), strings.Join([]string{
		"import sub/../sub/nested.conf",
		"import parts/*.conf",
		site("A-a"),
	}, "\n"))
	writeImportFile(t, filepath.Join(dir, "Caddyfile"), "import proj/a.conf\n")
	return dir
}

// failureKind selects how the bad tree's imported target fails.
type failureKind struct {
	name       string
	snippets   []string
	breakTree  func(t *testing.T, target, link string)
	repairTree func(t *testing.T, target, link string)
}

func failureKinds() []failureKind {
	return []failureKind{
		{
			name:     "cycle",
			snippets: []string{"import cycle detected", "target.conf", "loop-link.conf"},
			breakTree: func(t *testing.T, target, link string) {
				writeImportFile(t, target, "import loop-link.conf\n")
				symlinkOrSkip(t, target, link)
			},
			repairTree: func(t *testing.T, target, link string) {
				if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
					t.Fatalf("removing cycle link: %v", err)
				}
				writeImportFile(t, target, site("B-fixed"))
			},
		},
		{
			name:     "syntax",
			snippets: []string{"syntax error", "expecting '}'"},
			breakTree: func(t *testing.T, target, _ string) {
				// an unclosed block is a genuine syntax error
				writeImportFile(t, target, "B-syntax {\n")
			},
			repairTree: func(t *testing.T, target, _ string) {
				writeImportFile(t, target, site("B-fixed"))
			},
		},
		{
			name:     "unreadable",
			snippets: []string{"permission denied"},
			breakTree: func(t *testing.T, target, _ string) {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses unreadable-file permissions")
				}
				writeImportFile(t, target, site("B-fixed"))
				if err := os.Chmod(target, 0o000); err != nil {
					t.Fatalf("making target unreadable: %v", err)
				}
			},
			repairTree: func(t *testing.T, target, _ string) {
				if err := os.Chmod(target, 0o644); err != nil {
					t.Fatalf("restoring target permissions: %v", err)
				}
				writeImportFile(t, target, site("B-fixed"))
			},
		},
	}
}

// buildBadImportTree builds a tree whose nested import target can be broken,
// repaired (to a regular successful site) and broken again. Before repair
// the failure must surface; after repair it expands B-b1, B-good, B-fixed.
func buildBadImportTree(t *testing.T, kind failureKind) (dir, root string, body []byte) {
	t.Helper()
	dir = t.TempDir()
	root = filepath.Join(dir, "Caddyfile")
	writeImportFile(t, filepath.Join(dir, "parts", "b1.conf"), site("B-b1"))
	writeImportFile(t, filepath.Join(dir, "good.conf"), "import parts/*.conf\n"+site("B-good"))
	writeImportFile(t, filepath.Join(dir, "broken.conf"), "import broken/target.conf\n")
	target := filepath.Join(dir, "broken", "target.conf")
	link := filepath.Join(dir, "broken", "loop-link.conf")
	writeImportFile(t, target, "")
	kind.breakTree(t, target, link)
	writeImportFile(t, root, "import good.conf\nimport broken.conf\n")
	return dir, root, []byte("import good.conf\nimport broken.conf\n")
}

// TestAdapterConcurrentImportIsolation drives one shared Adapter with two
// disjoint file trees: one always succeeds with nested/glob/relative
// imports, the other fails through one of several fault categories. Many
// adaptations of both trees run concurrently and repeatedly; the batch is
// launched in both submission orders. Every call must observe exactly what
// a fresh Adapter would observe serially for its own tree. Afterwards the
// same Adapter handles success, failure, repaired-success and re-broken
// retries on both trees.
func TestAdapterConcurrentImportIsolation(t *testing.T) {
	const (
		workers    = 6
		iterations = 3
	)

	goodDir := buildGoodImportTree(t)
	goodRoot := filepath.Join(goodDir, "Caddyfile")
	goodBody := []byte("import proj/a.conf\n")

	for _, kind := range failureKinds() {
		t.Run(kind.name, func(t *testing.T) {
			badDir, badRoot, badBody := buildBadImportTree(t, kind)
			target := filepath.Join(badDir, "broken", "target.conf")
			link := filepath.Join(badDir, "broken", "loop-link.conf")

			// fresh-adapter serial reference outcomes
			fresh := Adapter{ServerType: recordingServerType{}}
			wantGood := adaptAt(t, fresh, goodRoot, goodBody)
			wantBad := adaptAt(t, fresh, badRoot, badBody)
			if wantGood.err != nil || wantBad.err == nil {
				t.Fatalf("bad reference outcomes: good=%v bad=%v", wantGood.err, wantBad.err)
			}
			requireFailure(t, "reference", wantBad, wantBad, badDir, goodDir, kind.snippets...)

			// one shared Adapter for every concurrent call and every retry
			shared := Adapter{ServerType: recordingServerType{}}

			runBatch := func(badFirst bool) {
				t.Helper()
				start := make(chan struct{})
				var wg sync.WaitGroup
				var mu sync.Mutex
				var goodResults, badResults []adaptOutcome

				launchGood := func() {
					defer wg.Done()
					<-start
					res := adaptAt(t, shared, goodRoot, goodBody)
					mu.Lock()
					goodResults = append(goodResults, res)
					mu.Unlock()
				}
				launchBad := func() {
					defer wg.Done()
					<-start
					res := adaptAt(t, shared, badRoot, badBody)
					mu.Lock()
					badResults = append(badResults, res)
					mu.Unlock()
				}

				for i := 0; i < workers; i++ {
					if badFirst {
						wg.Add(2)
						go launchBad()
						go launchGood()
					} else {
						wg.Add(2)
						go launchGood()
						go launchBad()
					}
				}
				close(start)
				wg.Wait()

				if len(goodResults) != workers || len(badResults) != workers {
					t.Fatalf("expected %d of each, got %d good / %d bad", workers, len(goodResults), len(badResults))
				}
				for i, res := range goodResults {
					requireSuccess(t, "good#"+strconv.Itoa(i)+" (badFirst="+strconv.FormatBool(badFirst)+")", res, wantGood, badDir, "B-")
				}
				for i, res := range badResults {
					requireFailure(t, "bad#"+strconv.Itoa(i)+" (badFirst="+strconv.FormatBool(badFirst)+")", res, wantBad, badDir, goodDir, kind.snippets...)
				}
			}

			for iter := 0; iter < iterations; iter++ {
				runBatch(iter%2 == 0)
			}

			// post-batch retries on the same Adapter: still success/failure
			gotGood := adaptAt(t, shared, goodRoot, goodBody)
			requireSuccess(t, "post-batch good", gotGood, wantGood, badDir, "B-")
			gotBad := adaptAt(t, shared, badRoot, badBody)
			requireFailure(t, "post-batch bad", gotBad, wantBad, badDir, goodDir, kind.snippets...)

			// repair the bad tree: only repaired calls now succeed; the good
			// tree is untouched and keeps its result.
			kind.repairTree(t, target, link)
			repairedFresh := Adapter{ServerType: recordingServerType{}}
			wantRepaired := adaptAt(t, repairedFresh, badRoot, badBody)
			if wantRepaired.err != nil {
				t.Fatalf("repaired tree should adapt with a fresh adapter, got: %v", wantRepaired.err)
			}
			gotRepaired := adaptAt(t, shared, badRoot, badBody)
			requireSuccess(t, "repaired bad", gotRepaired, wantRepaired, goodDir, "A-")
			gotGood2 := adaptAt(t, shared, goodRoot, goodBody)
			requireSuccess(t, "good after repair", gotGood2, wantGood, badDir, "B-")

			// re-introduce the fault: it must fail again, with the same
			// category and file chain, while the good tree stays successful.
			kind.breakTree(t, target, link)
			wantBadAgain := adaptAt(t, Adapter{ServerType: recordingServerType{}}, badRoot, badBody)
			requireFailure(t, "fresh reference after re-break", wantBadAgain, wantBad, badDir, goodDir, kind.snippets...)
			gotBadAgain := adaptAt(t, shared, badRoot, badBody)
			requireFailure(t, "re-broken bad", gotBadAgain, wantBad, badDir, goodDir, kind.snippets...)
			gotGood3 := adaptAt(t, shared, goodRoot, goodBody)
			requireSuccess(t, "good after re-break", gotGood3, wantGood, badDir, "B-")
		})
	}
}
