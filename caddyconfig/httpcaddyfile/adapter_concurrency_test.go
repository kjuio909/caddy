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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// The trees below share one site shape: a redir followed (lexically) by a
// respond. In the default directive order redir runs first; the ALPHA tree
// flips that with the "order" global option so respond runs first. The
// adapted JSON therefore differs between the trees and reveals any leakage
// of one call's directive order into the other. Each tree also exercises
// nested, globbed and relative imports, which contribute header directives
// and anchor the diagnostics to the tree's own files.
func writeOrderTree(t *testing.T, dir, host string) {
	t.Helper()
	marker := strings.SplitN(host, "-", 2)[0] // "alpha" or "bravo"
	write := func(rel, content string) {
		writeAdaptFile(t, filepath.Join(dir, rel), content)
	}
	write(filepath.Join(marker, "sub", "deep.caddy"), "vars deep "+marker+"\n")
	write(filepath.Join(marker, "dir.caddy"), "import sub/../sub/deep.caddy\n")
	write(filepath.Join(marker, "parts", "p1.caddy"), "vars glob "+marker+"\n")
	write(filepath.Join(marker, "site.caddy"), strings.Join([]string{
		"import dir.caddy",
		"import parts/*.caddy",
		"redir /" + marker + "-old /" + marker + "-new",
		`respond "` + strings.ToUpper(marker) + `"`,
	}, "\n")+"\n")
}

func alphaBody() []byte {
	return []byte(`{
	order respond before redir
}
alpha-iso.example {
	import alpha/site.caddy
}
`)
}

func bravoBody() []byte {
	return []byte(`bravo-iso.example {
	import bravo/site.caddy
}
`)
}

type adaptResult struct {
	out      []byte
	warnings []caddyconfig.Warning
	err      error
}

func runAdapt(t *testing.T, adapter caddyfile.Adapter, root string, body []byte) adaptResult {
	t.Helper()
	out, warnings, err := adapter.Adapt(body, map[string]any{"filename": root})
	return adaptResult{out: out, warnings: warnings, err: err}
}

// redirRespondOrder decodes the site subroute and returns the relative
// pipeline order of the redir (a static_response carrying status 302) and
// the plain respond (a static_response carrying a body). Headers are
// ignored.
func redirRespondOrder(t *testing.T, out []byte) []string {
	t.Helper()
	var doc struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []struct {
						Handle []struct {
							Routes []struct {
								Handle []struct {
									StatusCode int    `json:"status_code"`
									Body       string `json:"body"`
								} `json:"handle"`
							} `json:"routes"`
						} `json:"handle"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshaling adapted JSON: %v\n%s", err, out)
	}
	var order []string
	for _, srv := range doc.Apps.HTTP.Servers {
		for _, route := range srv.Routes {
			for _, h := range route.Handle {
				for _, sr := range h.Routes {
					for _, hh := range sr.Handle {
						if hh.StatusCode == 302 {
							order = append(order, "redir")
						} else if hh.Body != "" {
							order = append(order, "respond")
						}
					}
				}
			}
		}
	}
	return order
}

func expectHandlerOrder(t *testing.T, label string, out []byte, wantFirst, wantSecond string) {
	t.Helper()
	order := redirRespondOrder(t, out)
	redirAt, respondAt := indexOf(order, "redir"), indexOf(order, "respond")
	if redirAt < 0 || respondAt < 0 {
		t.Fatalf("%s: expected one redir and one respond, got %v", label, order)
	}
	gotFirst, gotSecond := "redir", "respond"
	if respondAt < redirAt {
		gotFirst, gotSecond = "respond", "redir"
	}
	if gotFirst != wantFirst || gotSecond != wantSecond {
		t.Errorf("%s: expected %s before %s, got pipeline %v", label, wantFirst, wantSecond, order)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func assertResultEqual(t *testing.T, label string, got, want adaptResult) {
	t.Helper()
	if (got.err == nil) != (want.err == nil) {
		t.Fatalf("%s: error presence drift: want %v, got %v", label, want.err, got.err)
	}
	if want.err != nil && got.err.Error() != want.err.Error() {
		t.Errorf("%s: error text drift\nwant: %s\ngot:  %s", label, want.err, got.err)
	}
	if !bytes.Equal(got.out, want.out) {
		t.Errorf("%s: body drift\nwant: %s\ngot:  %s", label, want.out, got.out)
	}
	if !reflect.DeepEqual(got.warnings, want.warnings) {
		t.Errorf("%s: warnings drift\nwant: %v\ngot:  %v", label, want.warnings, got.warnings)
	}
}

// TestAdaptConcurrentDirectiveOrderIsolation drives one shared
// caddyfile.Adapter (with the real HTTP ServerType) concurrently from two
// disjoint file trees: one uses the "order" global option to reorder
// respond before redir, the other relies on the default order. Neither the
// returned JSON, the warnings, nor the error of one call may reflect the
// other call's tree or directive order: this both detects data races (run
// under -race) and semantic cross-talk through process-wide state mutated
// while adapting.
func TestAdaptConcurrentDirectiveOrderIsolation(t *testing.T) {
	const (
		workers    = 8
		iterations = 4
	)

	alphaDir := t.TempDir()
	bravoDir := t.TempDir()
	writeOrderTree(t, alphaDir, "alpha-iso")
	writeOrderTree(t, bravoDir, "bravo-iso")
	alphaRoot := filepath.Join(alphaDir, "Caddyfile")
	bravoRoot := filepath.Join(bravoDir, "Caddyfile")
	aBody, bBody := alphaBody(), bravoBody()

	// Serial references computed as if each tree were adapted in isolation.
	// bravo (default order) is evaluated first, before any "order" option has
	// run, so its reference represents a process that only ever saw bravo.
	fresh := caddyfile.Adapter{ServerType: ServerType{}}
	wantBravo := runAdapt(t, fresh, bravoRoot, bBody)
	if wantBravo.err != nil {
		t.Fatalf("bravo reference failed: %v", wantBravo.err)
	}
	wantAlpha := runAdapt(t, fresh, alphaRoot, aBody)
	if wantAlpha.err != nil {
		t.Fatalf("alpha reference failed: %v", wantAlpha.err)
	}
	expectHandlerOrder(t, "alpha reference", wantAlpha.out, "respond", "redir")
	expectHandlerOrder(t, "bravo reference", wantBravo.out, "redir", "respond")
	if bytes.Contains(wantAlpha.out, []byte("bravo")) || bytes.Contains(wantBravo.out, []byte("alpha")) {
		t.Fatal("reference bodies already leak between trees")
	}

	shared := caddyfile.Adapter{ServerType: ServerType{}}

	runBatch := func(alphaFirst bool) {
		t.Helper()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var alphaResults, bravoResults []adaptResult

		launch := func(body []byte, root string, sink *[]adaptResult) {
			defer wg.Done()
			<-start
			res := runAdapt(t, shared, root, body)
			mu.Lock()
			*sink = append(*sink, res)
			mu.Unlock()
		}

		for i := 0; i < workers; i++ {
			wg.Add(2)
			if alphaFirst {
				go launch(aBody, alphaRoot, &alphaResults)
				go launch(bBody, bravoRoot, &bravoResults)
			} else {
				go launch(bBody, bravoRoot, &bravoResults)
				go launch(aBody, alphaRoot, &alphaResults)
			}
		}
		close(start)
		wg.Wait()

		if len(alphaResults) != workers || len(bravoResults) != workers {
			t.Fatalf("expected %d results per tree, got %d alpha / %d bravo",
				workers, len(alphaResults), len(bravoResults))
		}
		for i, res := range alphaResults {
			assertResultEqual(t, "alpha#"+strconv.Itoa(i), res, wantAlpha)
			if bytes.Contains(res.out, []byte("bravo")) {
				t.Errorf("alpha#%d body leaks bravo", i)
			}
		}
		for i, res := range bravoResults {
			assertResultEqual(t, "bravo#"+strconv.Itoa(i), res, wantBravo)
			if bytes.Contains(res.out, []byte("alpha")) {
				t.Errorf("bravo#%d body leaks alpha", i)
			}
		}
	}

	for iter := 0; iter < iterations; iter++ {
		runBatch(iter%2 == 0)
	}

	// Post-batch retries through the same shared Adapter keep each tree's
	// own result, in either order, and repeat stably.
	assertResultEqual(t, "post-batch alpha", runAdapt(t, shared, alphaRoot, aBody), wantAlpha)
	assertResultEqual(t, "post-batch bravo", runAdapt(t, shared, bravoRoot, bBody), wantBravo)
	assertResultEqual(t, "post-batch bravo again", runAdapt(t, shared, bravoRoot, bBody), wantBravo)
	assertResultEqual(t, "post-batch alpha again", runAdapt(t, shared, alphaRoot, aBody), wantAlpha)
}

// TestAdaptConcurrentFailureIsolation mixes a failing tree (an import cycle
// or a syntax error) with the successful order-reordered tree. Failures
// return no partial body and name only their own file chain; the successful
// tree is unaffected. The same shared Adapter is then reused for
// fail -> repair -> succeed -> re-break -> fail.
func TestAdaptConcurrentFailureIsolation(t *testing.T) {
	const workers = 6

	alphaDir := t.TempDir()
	writeOrderTree(t, alphaDir, "alpha-iso")
	alphaRoot := filepath.Join(alphaDir, "Caddyfile")
	aBody := alphaBody()
	wantAlpha := runAdapt(t, caddyfile.Adapter{ServerType: ServerType{}}, alphaRoot, aBody)
	if wantAlpha.err != nil {
		t.Fatalf("alpha reference failed: %v", wantAlpha.err)
	}

	type fault struct {
		name     string
		snippets []string
		break_   func(t *testing.T, target, link string)
		repair   func(t *testing.T, target, link string)
	}
	faults := []fault{
		{
			name:     "cycle",
			snippets: []string{"import cycle detected", "target.caddy", "loop-link.caddy"},
			break_: func(t *testing.T, target, link string) {
				writeAdaptFile(t, target, "import loop-link.caddy\n")
				if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
					t.Skipf("cannot create symlink: %v", err)
				}
			},
			repair: func(t *testing.T, target, link string) {
				if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
					t.Fatalf("remove link: %v", err)
				}
				writeAdaptFile(t, target, "charlie-fixed.example {\n\trespond \"FIXED\"\n}\n")
			},
		},
		{
			name:     "syntax",
			snippets: []string{"syntax error"},
			break_: func(t *testing.T, target, _ string) {
				writeAdaptFile(t, target, "charlie-broken.example {\n")
			},
			repair: func(t *testing.T, target, _ string) {
				writeAdaptFile(t, target, "charlie-fixed.example {\n\trespond \"FIXED\"\n}\n")
			},
		},
	}

	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			charlieDir := t.TempDir()
			charlieRoot := filepath.Join(charlieDir, "Caddyfile")
			target := filepath.Join(charlieDir, "charlie", "target.caddy")
			link := filepath.Join(charlieDir, "charlie", "loop-link.caddy")
			writeAdaptFile(t, filepath.Join(charlieDir, "charlie", "entry.caddy"), "import target.caddy\n")
			writeAdaptFile(t, charlieRoot, "import charlie/entry.caddy\n")
			f.break_(t, target, link)
			cBody := []byte("import charlie/entry.caddy\n")

			wantFail := runAdapt(t, caddyfile.Adapter{ServerType: ServerType{}}, charlieRoot, cBody)
			if wantFail.err == nil {
				t.Fatal("reference failure unexpectedly succeeded")
			}
			if wantFail.out != nil {
				t.Fatal("reference failure returned a partial body")
			}

			shared := caddyfile.Adapter{ServerType: ServerType{}}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			var oks, fails []adaptResult
			for i := 0; i < workers; i++ {
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					r := runAdapt(t, shared, alphaRoot, aBody)
					mu.Lock()
					oks = append(oks, r)
					mu.Unlock()
				}()
				go func() {
					defer wg.Done()
					<-start
					r := runAdapt(t, shared, charlieRoot, cBody)
					mu.Lock()
					fails = append(fails, r)
					mu.Unlock()
				}()
			}
			close(start)
			wg.Wait()

			if len(oks) != workers || len(fails) != workers {
				t.Fatalf("expected %d of each, got %d ok / %d fail", workers, len(oks), len(fails))
			}
			for i, r := range oks {
				assertResultEqual(t, "success#"+strconv.Itoa(i), r, wantAlpha)
			}
			for i, r := range fails {
				label := "failure#" + strconv.Itoa(i)
				if r.err == nil {
					t.Errorf("%s: expected failure, got body %s", label, r.out)
					continue
				}
				if r.out != nil {
					t.Errorf("%s: partial body on failure (%d bytes)", label, len(r.out))
				}
				if r.err.Error() != wantFail.err.Error() {
					t.Errorf("%s: error text drift\nwant: %s\ngot:  %s", label, wantFail.err, r.err)
				}
				msg := r.err.Error()
				for _, snippet := range f.snippets {
					if !strings.Contains(msg, snippet) {
						t.Errorf("%s: error %q missing %q", label, msg, snippet)
					}
				}
				if !strings.Contains(msg, charlieDir) {
					t.Errorf("%s: error does not name its own tree: %s", label, msg)
				}
				if strings.Contains(msg, alphaDir) {
					t.Errorf("%s: error leaks the successful tree's path: %s", label, msg)
				}
			}

			// retry failure, then repair: only repaired calls succeed, and a
			// fresh adapter produces the same repaired result.
			if r := runAdapt(t, shared, charlieRoot, cBody); r.err == nil {
				t.Fatal("failure must persist on immediate retry")
			}
			f.repair(t, target, link)
			wantFixed := runAdapt(t, caddyfile.Adapter{ServerType: ServerType{}}, charlieRoot, cBody)
			if wantFixed.err != nil {
				t.Fatalf("repaired reference failed: %v", wantFixed.err)
			}
			assertResultEqual(t, "repaired", runAdapt(t, shared, charlieRoot, cBody), wantFixed)
			assertResultEqual(t, "alpha after repair", runAdapt(t, shared, alphaRoot, aBody), wantAlpha)

			// re-break: fails again with the same category and chain.
			f.break_(t, target, link)
			gotFail := runAdapt(t, shared, charlieRoot, cBody)
			if gotFail.err == nil {
				t.Fatal("re-broken tree unexpectedly succeeded")
			}
			if gotFail.err.Error() != wantFail.err.Error() {
				t.Errorf("re-broken error drift\nwant: %s\ngot:  %s", wantFail.err, gotFail.err)
			}
			assertResultEqual(t, "alpha after re-break", runAdapt(t, shared, alphaRoot, aBody), wantAlpha)
		})
	}
}

func writeAdaptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestAdaptOrderOptionIsLocalToAdaptation covers two ordering semantics that
// used to rely on a process-wide mutation: (1) several "order" options in one
// global block compose within that adaptation, and (2) a later adaptation
// without the option still uses the baseline rather than inheriting the
// earlier call's order. Repeating the ordered adaptation must be stable too.
func TestAdaptOrderOptionIsLocalToAdaptation(t *testing.T) {
	ordered := []byte(`{
	order respond first
	order redir last
}
ordered-iso.example {
	vars x 1
	redir /a /b
	respond "O"
}
`)
	plain := []byte(`plain-iso.example {
	redir /a /b
	respond "P"
}
`)
	adapter := caddyfile.Adapter{ServerType: ServerType{}}

	var first []byte
	for i := 0; i < 3; i++ {
		out, _, err := adapter.Adapt(ordered, nil)
		if err != nil {
			t.Fatalf("ordered adapt %d: %v", i, err)
		}
		order := redirRespondOrder(t, out)
		if order[0] != "respond" || order[len(order)-1] != "redir" {
			t.Fatalf("ordered adapt %d: expected respond first and redir last, got %v", i, order)
		}
		if first == nil {
			first = out
		} else if !bytes.Equal(out, first) {
			t.Fatalf("ordered adapt %d drifted from the first", i)
		}
	}

	// a subsequent adaptation with no "order" option must not inherit it
	out, _, err := adapter.Adapt(plain, nil)
	if err != nil {
		t.Fatalf("plain adapt: %v", err)
	}
	order := redirRespondOrder(t, out)
	if order[0] != "redir" {
		t.Fatalf("plain adaptation inherited reordered directive order, got %v", order)
	}
}
