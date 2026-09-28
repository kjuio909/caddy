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
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// recordingServerType renders the parsed blocks (their keys, every token
// text, and the set of files the tokens came from) into deterministic JSON
// so an adaptation's expanded tree is fully observable through Adapt's
// returned body. It never returns an error or a warning of its own, so any
// difference between two adaptations can only come from parsing/import
// expansion.
type recordingServerType struct {
	calls *atomic.Int64
}

type recordedBlock struct {
	Keys   []string `json:"keys,omitempty"`
	Tokens []string `json:"tokens,omitempty"`
	Files  []string `json:"files,omitempty"`
}

func (s recordingServerType) Setup(blocks []ServerBlock, _ map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}

	out := make([]recordedBlock, 0, len(blocks))
	for _, block := range blocks {
		rb := recordedBlock{Keys: block.GetKeysText()}
		fileSet := make(map[string]struct{})
		for _, seg := range block.Segments {
			for _, tok := range seg {
				rb.Tokens = append(rb.Tokens, tok.Text)
				fileSet[tok.File] = struct{}{}
			}
		}
		for file := range fileSet {
			rb.Files = append(rb.Files, file)
		}
		sort.Strings(rb.Files)
		out = append(out, rb)
	}

	raw, err := json.Marshal(struct {
		Blocks []recordedBlock `json:"blocks"`
	}{Blocks: out})
	if err != nil {
		return nil, nil, err
	}
	return &caddy.Config{AppsRaw: caddy.ModuleMap{"recording": raw}}, nil, nil
}

// adaptation is everything a caller of the public entry point can observe.
type adaptation struct {
	out      []byte
	warnings []caddyconfig.Warning
	errText  string // empty when err is nil
}

func adaptNow(t *testing.T, adapter Adapter, body []byte, root string) adaptation {
	t.Helper()
	out, warnings, err := adapter.Adapt(body, map[string]any{"filename": root})
	res := adaptation{out: out, warnings: warnings}
	if err != nil {
		res.errText = err.Error()
	}
	return res
}

// assertSameResult fails the test when two observable adaptations differ in
// expanded body, warning set/order, or error.
func assertSameResult(t *testing.T, label string, got, want adaptation) {
	t.Helper()
	if (got.errText == "") != (want.errText == "") {
		t.Errorf("%s: error presence differs:\ngot:  %q\nwant: %q", label, got.errText, want.errText)
		return
	}
	if got.errText != want.errText {
		t.Errorf("%s: error text differs:\ngot:  %s\nwant: %s", label, got.errText, want.errText)
	}
	if !bytes.Equal(got.out, want.out) {
		t.Errorf("%s: expanded body differs:\ngot:  %s\nwant: %s", label, got.out, want.out)
	}
	if !reflect.DeepEqual(got.warnings, want.warnings) {
		t.Errorf("%s: warnings differ (set or order):\ngot:  %#v\nwant: %#v", label, got.warnings, want.warnings)
	}
}

// assertResultContainedInTree verifies an observable result references only
// its own file tree and none of the other tree's paths or expanded hosts.
func assertResultContainedInTree(t *testing.T, label, otherDir string, otherHosts []string, res adaptation) {
	t.Helper()
	if bytes.Contains(res.out, []byte(otherDir)) {
		t.Errorf("%s: success body references the other tree's path %s: %s", label, otherDir, res.out)
	}
	if strings.Contains(res.errText, otherDir) {
		t.Errorf("%s: error references the other tree's path %s: %s", label, otherDir, res.errText)
	}
	for _, w := range res.warnings {
		if strings.Contains(w.File, otherDir) {
			t.Errorf("%s: warning points into the other tree: %s", label, w.File)
		}
	}
	for _, host := range otherHosts {
		if bytes.Contains(res.out, []byte(host)) {
			t.Errorf("%s: success body contains the other tree's expansion %q: %s", label, host, res.out)
		}
		if strings.Contains(res.errText, host) {
			t.Errorf("%s: error contains the other tree's expansion %q: %s", label, host, res.errText)
		}
	}
}

// importTree is one self-contained, fixed file tree exercised through the
// public Adapter. Its layout mixes nested imports, a glob with a symlink
// alias (same real file hit twice by one glob), a relative ".." import, an
// explicitly duplicated import, and an {args[*]} placeholder.
type importTree struct {
	dir         string
	root        string
	body        []byte
	expectHosts []string
}

// buildImportTree populates dir with the fixed tree.
//
// Layout (prefix distinguishes the two trees):
//
//	Caddyfile                root body (see buildRootBody)
//	modules/<p>-one.conf     imports nested/deep.conf, then defines <prefix>-one
//	modules/<p>-two.conf     imports ../shared/<p>-rel.conf, then defines <prefix>-two
//	modules/nested/deep.conf defines <prefix>-deep
//	modules/zz-alias.conf    symlink alias to <p>-one.conf (glob dedup target)
//	shared/<p>-rel.conf      defines <prefix>-rel
//	snippets/greet.conf      defines a site named by {args[0]}
func buildImportTree(t *testing.T, dir, prefix string, formatted bool) importTree {
	t.Helper()

	modules := filepath.Join(dir, "modules")
	writeImportFile(t, filepath.Join(modules, "nested", "deep.conf"), site(prefix+"-deep"))
	writeImportFile(t, filepath.Join(modules, prefix+"-one.conf"),
		"import nested/deep.conf\n"+site(prefix+"-one"))
	writeImportFile(t, filepath.Join(modules, prefix+"-two.conf"),
		"import ../shared/"+prefix+"-rel.conf\n"+site(prefix+"-two"))
	symlinkOrSkip(t, filepath.Join(modules, prefix+"-one.conf"),
		filepath.Join(modules, "zz-alias.conf"))
	writeImportFile(t, filepath.Join(dir, "shared", prefix+"-rel.conf"), site(prefix+"-rel"))
	writeImportFile(t, filepath.Join(dir, "snippets", "greet.conf"), "{args[0]} {\n}\n")

	// A doubled space after "import" makes Format report exactly one
	// formatting warning against the root; the clean spelling reports none.
	spacing := " "
	if !formatted {
		spacing = "  "
	}
	body := "import" + spacing + "modules/*.conf\n" +
		"import modules/" + prefix + "-one.conf\n" +
		"import modules/" + prefix + "-one.conf\n" +
		"import snippets/greet.conf " + prefix + "-greet\n"

	root := filepath.Join(dir, "Caddyfile")
	writeImportFile(t, root, body)
	readBody, err := os.ReadFile(root)
	if err != nil {
		t.Fatalf("reading root %s: %v", root, err)
	}

	// glob: one -> deep, one; two -> rel, two; the alias collapses onto one.
	// then two explicit one-imports (deep, one each), then the args import.
	return importTree{
		dir:  dir,
		root: root,
		body: readBody,
		expectHosts: []string{
			prefix + "-deep", prefix + "-one",
			prefix + "-rel", prefix + "-two",
			prefix + "-deep", prefix + "-one",
			prefix + "-deep", prefix + "-one",
			prefix + "-greet",
		},
	}
}

// faultModes cover the three ways one tree's import may fail.
type faultMode string

const (
	faultNone       faultMode = "success"
	faultCycle      faultMode = "cycle"
	faultSyntax     faultMode = "syntax"
	faultUnreadable faultMode = "unreadable"
)

// faultFile is the root-relative import the failing tree adds at the end.
const faultImport = "fault.conf"

// configureFault places the failing target and returns a repair/re-apply
// pair: repair makes the same import succeed, reinstall restores the fault.
// unreadable faults skip when the current user can read mode-0000 files.
func configureFault(t *testing.T, dir string, mode faultMode) (repair, reinstall func()) {
	t.Helper()
	fault := filepath.Join(dir, faultImport)
	link := filepath.Join(dir, "fault-link.conf")

	valid := func() { writeImportFile(t, fault, "host-b-fixed {\n}\n") }
	writeFault := func() {
		switch mode {
		case faultCycle:
			writeImportFile(t, fault, "import fault-link.conf\n")
		case faultSyntax:
			writeImportFile(t, fault, "broken {\n")
		case faultUnreadable:
			writeImportFile(t, fault, "secret\n")
			if err := os.Chmod(fault, 0o000); err != nil {
				t.Fatalf("making %s unreadable: %v", fault, err)
			}
		}
	}

	switch mode {
	case faultNone:
		return func() {}, func() {}
	case faultCycle:
		// fault.conf imports fault-link.conf, an alias back to it: a genuine
		// loop closed through a symlink. The symlink target must exist first.
		valid()
		symlinkOrSkip(t, fault, link)
	case faultUnreadable:
		writeFault()
		if f, err := os.Open(fault); err == nil {
			f.Close()
			if err := os.Chmod(fault, 0o644); err != nil {
				t.Fatalf("restoring %s after skip probe: %v", fault, err)
			}
			t.Skipf("unreadable import cannot be exercised: this user can read mode-0000 files")
		}
	}
	if mode != faultUnreadable {
		writeFault()
	}

	repair = func() {
		if mode == faultUnreadable {
			if err := os.Chmod(fault, 0o644); err != nil {
				t.Fatalf("restoring read permission on %s: %v", fault, err)
			}
		}
		valid()
	}
	reinstall = writeFault
	return repair, reinstall
}

// faultyBody appends the failing import line to a tree's root body.
func faultyBody(body []byte) []byte {
	return append(append([]byte{}, body...), []byte("import "+faultImport+"\n")...)
}

// TestAdapterConcurrentIsolation drives one shared Adapter from many
// goroutines against two non-overlapping file trees, and requires every
// observable result (body, warnings in order, error) to be identical to the
// same input adapted sequentially by a brand-new Adapter. One tree is
// healthy; the other is put through a cycle, a syntax error, and an
// unreadable target in turn. The batch is repeated with shuffled launch
// order; afterwards the same Adapter retries success, failure, repaired
// success, and restored failure.
//
// Nothing in this test reads internal state or injects failures: all
// judgments come from Adapt's return values over a fixed file tree.
func TestAdapterConcurrentIsolation(t *testing.T) {
	for _, mode := range []faultMode{faultNone, faultCycle, faultSyntax, faultUnreadable} {
		t.Run(string(mode), func(t *testing.T) {
			base := t.TempDir()
			dirA := filepath.Join(base, "treeA")
			dirB := filepath.Join(base, "treeB")

			// tree A is intentionally unformatted so a successful adaptation
			// carries one warning anchored to A's own root; tree B is clean.
			treeA := buildImportTree(t, dirA, "host-a", false)
			treeB := buildImportTree(t, dirB, "host-b", true)

			repair, reinstall := configureFault(t, dirB, mode)

			bodyB := treeB.body
			if mode != faultNone {
				bodyB = faultyBody(bodyB)
			}

			// one Adapter reused by every goroutine; fresh Adapters only
			// build the sequential oracles
			var calls atomic.Int64
			shared := Adapter{ServerType: recordingServerType{calls: &calls}}
			fresh := func() Adapter { return Adapter{ServerType: recordingServerType{}} }

			oracleA := adaptNow(t, fresh(), treeA.body, treeA.root)
			oracleB := adaptNow(t, fresh(), bodyB, treeB.root)

			wantSuccessB := mode == faultNone
			if oracleA.errText != "" {
				t.Fatalf("tree A oracle should succeed, got: %s", oracleA.errText)
			}
			if wantSuccessB && oracleB.errText != "" {
				t.Fatalf("tree B oracle should succeed, got: %s", oracleB.errText)
			}
			if !wantSuccessB {
				if oracleB.errText == "" {
					t.Fatalf("tree B oracle should fail (%s), got success", mode)
				}
				if oracleB.out != nil {
					t.Fatalf("failed adaptation must not return a partial body, got %d bytes", len(oracleB.out))
				}
				// the failure must speak only B's own file chain
				assertResultContainedInTree(t, "oracle B failure", dirA, treeA.expectHosts, oracleB)
				switch mode {
				case faultCycle:
					if !strings.Contains(oracleB.errText, "cycle") {
						t.Errorf("expected a cycle category, got: %s", oracleB.errText)
					}
					if !strings.Contains(oracleB.errText, faultImport) {
						t.Errorf("expected the chain to name %s, got: %s", faultImport, oracleB.errText)
					}
				case faultSyntax:
					if !strings.Contains(oracleB.errText, "syntax error") {
						t.Errorf("expected a syntax category, got: %s", oracleB.errText)
					}
				case faultUnreadable:
					if !strings.Contains(oracleB.errText, "permission denied") {
						t.Errorf("expected an unreadable-target category, got: %s", oracleB.errText)
					}
				}
			}

			// A's warning is its own root file; a successful B has none.
			if len(oracleA.warnings) != 1 {
				t.Fatalf("tree A oracle should have one formatting warning, got %#v", oracleA.warnings)
			}
			if strings.Contains(oracleA.warnings[0].File, dirB) {
				t.Errorf("tree A warning must point at A's root, got %s", oracleA.warnings[0].File)
			}
			if wantSuccessB && len(oracleB.warnings) != 0 {
				t.Errorf("clean tree B should have no warnings, got %#v", oracleB.warnings)
			}

			// expanded bodies must show each tree's hosts, in order, and no
			// paths from the other tree
			for label, oracle := range map[string]adaptation{"A": oracleA, "B": oracleB} {
				if oracle.errText != "" {
					continue
				}
				tree := treeA
				otherDir, otherHosts := dirB, treeB.expectHosts
				if label == "B" {
					tree, otherDir, otherHosts = treeB, dirA, treeA.expectHosts
				}
				for _, h := range tree.expectHosts {
					if !bytes.Contains(oracle.out, []byte(h)) {
						t.Errorf("tree %s output missing expansion %q: %s", label, h, oracle.out)
					}
				}
				assertResultContainedInTree(t, "oracle "+label, otherDir, otherHosts, oracle)
			}

			// concurrent jobs: A twice (identical concurrent calls) plus B
			type job struct {
				label  string
				body   []byte
				root   string
				oracle adaptation
			}
			jobs := []job{
				{"A#1", treeA.body, treeA.root, oracleA},
				{"A#2", treeA.body, treeA.root, oracleA},
				{"B", bodyB, treeB.root, oracleB},
			}

			const rounds = 6
			var expectedSuccess int64
			for r := 0; r < rounds; r++ {
				results := make([]adaptation, len(jobs))
				var wg sync.WaitGroup
				start := make(chan struct{})
				order := rand.Perm(len(jobs)) // shuffle launch order each round
				for _, idx := range order {
					wg.Add(1)
					go func(j job, slot int) {
						defer wg.Done()
						<-start // release every adaptation together
						results[slot] = adaptNow(t, shared, j.body, j.root)
					}(jobs[idx], idx)
				}
				close(start)
				wg.Wait()

				for idx, j := range jobs {
					res := results[idx]
					label := "round " + strconv.Itoa(r) + " " + j.label
					assertSameResult(t, label, res, j.oracle)
					otherDir, otherHosts := dirB, treeB.expectHosts
					if strings.HasPrefix(j.label, "B") {
						otherDir, otherHosts = dirA, treeA.expectHosts
					}
					assertResultContainedInTree(t, label, otherDir, otherHosts, res)
					if res.errText == "" {
						expectedSuccess++
						if res.out == nil {
							t.Errorf("%s: success returned no body", label)
						}
					} else if res.out != nil {
						t.Errorf("%s: failure returned a partial body (%d bytes)", label, len(res.out))
					}
				}
			}

			// post-batch retries on the very same Adapter: success, failure,
			// repaired success, restored failure — each compared to a fresh
			// Adapter handling the same current input.
			assertSameResult(t, "retry A success",
				adaptNow(t, shared, treeA.body, treeA.root), adaptNow(t, fresh(), treeA.body, treeA.root))
			expectedSuccess++

			if mode != faultNone {
				assertSameResult(t, "retry B failure",
					adaptNow(t, shared, bodyB, treeB.root), oracleB)

				repair()
				repaired := adaptNow(t, fresh(), bodyB, treeB.root)
				if repaired.errText != "" {
					t.Fatalf("repaired tree B should succeed, got: %s", repaired.errText)
				}
				assertSameResult(t, "retry B repaired success",
					adaptNow(t, shared, bodyB, treeB.root), repaired)
				if !bytes.Contains(repaired.out, []byte("host-b-fixed")) {
					t.Errorf("repaired output should contain the fixed host, got %s", repaired.out)
				}
				expectedSuccess++

				// A must be unaffected by the repair.
				assertSameResult(t, "retry A after B repair",
					adaptNow(t, shared, treeA.body, treeA.root), oracleA)
				expectedSuccess++

				reinstall()
				assertSameResult(t, "retry B restored failure",
					adaptNow(t, shared, bodyB, treeB.root), oracleB)

				// success remains success after the restored failure
				assertSameResult(t, "retry A after B restored fault",
					adaptNow(t, shared, treeA.body, treeA.root), oracleA)
				expectedSuccess++
			} else {
				assertSameResult(t, "retry B success",
					adaptNow(t, shared, bodyB, treeB.root), oracleB)
				expectedSuccess++
			}

			// Setup must run exactly once per successful adaptation and
			// never for a failed expansion.
			if calls.Load() != expectedSuccess {
				t.Errorf("Setup invocation count = %d, want %d", calls.Load(), expectedSuccess)
			}
		})
	}
}
