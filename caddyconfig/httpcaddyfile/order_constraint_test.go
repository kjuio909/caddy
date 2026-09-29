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
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// TestResolveDirectiveOrder covers the graph merge directly: adjacent
// placement, transitive edges, end sections, and the failure modes that
// must abort an adaptation.
func TestResolveDirectiveOrder(t *testing.T) {
	// a small baseline with a known shape stands in for the real default
	base := []string{"map", "root", "header", "redir", "method", "rewrite", "handle", "respond", "file_server"}
	mk := func(dir, pos, other string, line int) orderConstraint {
		c := orderConstraint{dir: dir, pos: Positional(pos), file: "Caddyfile", line: line}
		c.otherDir = other
		return c
	}

	for i, tc := range []struct {
		name        string
		constraints orderConstraints
		want        string
	}{
		{
			name:        "before moves adjacent to anchor",
			constraints: orderConstraints{mk("respond", "before", "redir", 2)},
			want:        "map root header respond redir method rewrite handle file_server",
		},
		{
			name:        "after moves adjacent to anchor",
			constraints: orderConstraints{mk("respond", "after", "header", 2)},
			want:        "map root header respond redir method rewrite handle file_server",
		},
		{
			name:        "transitive edges compose",
			constraints: orderConstraints{mk("respond", "before", "method", 2), mk("redir", "before", "respond", 3)},
			want:        "map root header redir respond method rewrite handle file_server",
		},
		{
			name: "first pins to the front, others keep baseline",
			constraints: orderConstraints{
				mk("root", "first", "", 2),
				mk("respond", "before", "method", 3),
			},
			want: "root map header redir respond method rewrite handle file_server",
		},
		{
			name:        "last pins to the back",
			constraints: orderConstraints{mk("method", "last", "", 2)},
			want:        "map root header redir rewrite handle respond file_server method",
		},
		{
			name: "two firsts coexist in baseline tiebreak",
			constraints: orderConstraints{
				mk("root", "first", "", 2),
				mk("redir", "first", "", 3),
			},
			want: "root redir map header method rewrite handle respond file_server",
		},
		{
			name: "non-baseline directive placed by its edge",
			constraints: orderConstraints{
				mk("plugin_a", "after", "root", 2),
				mk("plugin_b", "before", "header", 3),
			},
			want: "map root plugin_a plugin_b header redir method rewrite handle respond file_server",
		},
		{
			name: "duplicate relation is idempotent",
			constraints: orderConstraints{
				mk("respond", "before", "redir", 2),
				mk("respond", "before", "redir", 3),
			},
			want: "map root header respond redir method rewrite handle file_server",
		},
		{
			name:        "directive ordered relative to itself is a no-op",
			constraints: orderConstraints{mk("respond", "before", "respond", 2)},
			want:        strings.Join(base, " "),
		},
	} {
		got, err := resolveDirectiveOrder(base, tc.constraints)
		if err != nil {
			t.Errorf("case %d %s: unexpected error: %v", i, tc.name, err)
			continue
		}
		if joined := strings.Join(got, " "); joined != tc.want {
			t.Errorf("case %d %s:\n got %s\nwant %s", i, tc.name, joined, tc.want)
		}
	}
}

// TestResolveDirectiveOrderInvariantToDeclarationOrder swaps declaration
// lines: the graph and the merged order, and therefore the adapted JSON,
// must not depend on the order in which the edges were written.
func TestResolveDirectiveOrderInvariantToDeclarationOrder(t *testing.T) {
	base := []string{"map", "root", "header", "redir", "method", "rewrite", "handle", "respond", "file_server"}
	cs := orderConstraints{
		{dir: "respond", pos: Before, otherDir: "method", file: "Caddyfile", line: 2},
		{dir: "redir", pos: After, otherDir: "header", file: "Caddyfile", line: 3},
		{dir: "rewrite", pos: Before, otherDir: "respond", file: "Caddyfile", line: 4},
		{dir: "root", pos: First, file: "Caddyfile", line: 5},
	}
	var reference string
	for _, perm := range [][4]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{1, 0, 3, 2},
		{2, 3, 0, 1},
	} {
		shuffled := orderConstraints{cs[perm[0]], cs[perm[1]], cs[perm[2]], cs[perm[3]]}
		got, err := resolveDirectiveOrder(base, shuffled)
		if err != nil {
			t.Fatalf("permutation %v: %v", perm, err)
		}
		joined := strings.Join(got, " ")
		if reference == "" {
			reference = joined
		} else if joined != reference {
			t.Errorf("permutation %v:\n got %s\nwant %s", perm, joined, reference)
		}
	}
}

// TestResolveDirectiveOrderFailures checks each invalid graph: the error
// must be non-nil and name both the current declaration and, where
// relevant, the prior declaration it clashes with.
func TestResolveDirectiveOrderFailures(t *testing.T) {
	base := defaultDirectiveOrder
	c := func(dir, pos, other string, line int) orderConstraint {
		cc := orderConstraint{dir: dir, pos: Positional(pos), file: "Caddyfile", line: line}
		cc.otherDir = other
		return cc
	}
	for i, tc := range []struct {
		name        string
		constraints orderConstraints
		wantParts   []string
	}{
		{
			name: "direct opposite pair",
			constraints: orderConstraints{
				c("respond", "before", "redir", 2),
				c("redir", "before", "respond", 3),
			},
			wantParts: []string{"Caddyfile:3", "contradicts", "Caddyfile:2", "respond", "redir"},
		},
		{
			name: "two-node cycle via after",
			constraints: orderConstraints{
				c("respond", "after", "redir", 2),
				c("redir", "after", "respond", 3),
			},
			wantParts: []string{"Caddyfile:3", "contradicts", "Caddyfile:2"},
		},
		{
			name: "three-node cycle",
			constraints: orderConstraints{
				c("respond", "before", "redir", 2),
				c("redir", "before", "method", 3),
				c("method", "before", "respond", 4),
			},
			wantParts: []string{"cyclic order constraint", "respond", "redir", "method", "Caddyfile:2", "Caddyfile:3", "Caddyfile:4"},
		},
		{
			name: "first and last overlap",
			constraints: orderConstraints{
				c("root", "first", "", 2),
				c("root", "last", "", 3),
			},
			wantParts: []string{"Caddyfile:3", "root", "Caddyfile:2"},
		},
	} {
		_, err := resolveDirectiveOrder(base, tc.constraints)
		if err == nil {
			t.Errorf("case %d %s: expected error, got nil", i, tc.name)
			continue
		}
		msg := err.Error()
		for _, part := range tc.wantParts {
			if !strings.Contains(msg, part) {
				t.Errorf("case %d %s: error %q missing %q", i, tc.name, msg, part)
			}
		}
	}
}

// dynamic JSON shapes for walking adapted configs
type (
	dynRoute struct {
		HandlersRaw []dynHandler `json:"handle"`
	}
	dynHandler struct {
		Handler    string          `json:"handler"`
		StatusCode json.RawMessage `json:"status_code"`
		Body       json.RawMessage `json:"body"`
		Routes     []dynRoute      `json:"routes"`
	}
	dynServer struct {
		Routes []dynRoute `json:"routes"`
		Errors *struct {
			Routes []dynRoute `json:"routes"`
		} `json:"errors"`
	}
)

func classifyHandler(h dynHandler) (string, bool) {
	if h.Handler != "static_response" {
		return "", false
	}
	if len(h.Body) > 0 && string(h.Body) != `""` {
		return "respond:" + strings.Trim(string(h.Body), `"`), true
	}
	if len(h.StatusCode) > 0 && strings.Contains(string(h.StatusCode), "30") {
		return "redir", true
	}
	return "other", true
}

// walkStaticResponses collects classified static responses keyed by
// subroute nesting depth: top-level site directives are depth 1, handlers
// inside a route/handle subroute are depth 2, and handle_errors adds
// another wrapper at depth 3.
func walkStaticResponses(handlers []dynHandler, depth int, sink map[int][]string) {
	for _, h := range handlers {
		if h.Handler == "subroute" {
			for _, r := range h.Routes {
				walkStaticResponses(r.HandlersRaw, depth+1, sink)
			}
			continue
		}
		if kind, ok := classifyHandler(h); ok {
			sink[depth] = append(sink[depth], kind)
		}
	}
}

func adaptBody(t *testing.T, adapter caddyfile.Adapter, body string) ([]byte, []caddyconfig.Warning, error) {
	t.Helper()
	out, warnings, err := adapter.Adapt([]byte(body), nil)
	return out, warnings, err
}

func mustAdapt(t *testing.T, body string) []byte {
	t.Helper()
	out, _, err := adaptBody(t, caddyfile.Adapter{ServerType: ServerType{}}, body)
	if err != nil {
		t.Fatalf("adaptation failed: %v\nbody:\n%s", err, body)
	}
	return out
}

func staticByDepth(t *testing.T, out []byte) map[int][]string {
	t.Helper()
	var doc struct {
		Apps struct {
			HTTP struct {
				Servers map[string]dynServer `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal adapted JSON: %v\n%s", err, out)
	}
	sink := make(map[int][]string)
	for _, srv := range doc.Apps.HTTP.Servers {
		for _, route := range srv.Routes {
			walkStaticResponses(route.HandlersRaw, 0, sink)
		}
		if srv.Errors != nil {
			for _, route := range srv.Errors.Routes {
				walkStaticResponses(route.HandlersRaw, 0, sink)
			}
		}
	}
	return sink
}

// TestAdaptOrderEdgeStability verifies that swapped declaration lines and
// swapped top-level directive occurrence produce byte-identical JSON, and
// that the edges apply to every site in the configuration.
func TestAdaptOrderEdgeStability(t *testing.T) {
	bodyA := `{
	order respond before redir
	order vars after redir
}
edge-stable.example {
	redir /a /b
	respond "A"
	vars v 1
}
`
	bodyB := `{
	order vars after redir
	order respond before redir
}
edge-stable.example {
	vars v 1
	respond "A"
	redir /a /b
}
`
	outA := mustAdapt(t, bodyA)
	outB := mustAdapt(t, bodyB)
	if !reflect.DeepEqual(outA, outB) {
		t.Fatalf("swapped declarations and occurrence order changed the JSON\n%s\n----\n%s", outA, outB)
	}
	top := staticByDepth(t, outA)[1]
	// vars renders as a vars handler, not a static_response, so check the
	// static pair order explicitly
	if indexOf(top, "respond:A") < 0 || indexOf(top, "respond:A") > indexOf(top, "redir") {
		t.Fatalf("constraint not applied: %v", top)
	}

	// every site gets the same top-level relationship: two independent
	// sites must not exchange directives, but both must be reordered
	multiSite := `{
	order respond before redir
}
edge-one.example {
	redir /1 /x
	respond "ONE"
}
edge-two.example {
	redir /2 /y
	respond "TWO"
}
`
	out := mustAdapt(t, multiSite)
	var doc struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []struct {
						Match []struct {
							Host []string `json:"host"`
						} `json:"match"`
						Handle []struct {
							Routes []struct {
								Handle []dynHandler `json:"handle"`
							} `json:"routes"`
						} `json:"handle"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, srv := range doc.Apps.HTTP.Servers {
		for _, route := range srv.Routes {
			host := ""
			if len(route.Match) > 0 && len(route.Match[0].Host) > 0 {
				host = route.Match[0].Host[0]
			}
			for _, h := range route.Handle {
				for _, sr := range h.Routes {
					for _, hh := range sr.Handle {
						if kind, ok := classifyHandler(hh); ok {
							seen[host] = append(seen[host], kind)
						}
					}
				}
			}
		}
	}
	for host, want := range map[string][]string{
		"edge-one.example": {"respond:ONE", "redir"},
		"edge-two.example": {"respond:TWO", "redir"},
	} {
		if got := seen[host]; !reflect.DeepEqual(got, want) {
			t.Errorf("site %s: got %v, want %v", host, got, want)
		}
	}
}

// chainsByOrigin splits the static_response classification into the site's
// top-level chain, chains nested inside regular routes (route/handle
// blocks), and the handle_errors chain.
func chainsByOrigin(t *testing.T, out []byte) (top []string, nested []string, errChain []string) {
	t.Helper()
	var doc struct {
		Apps struct {
			HTTP struct {
				Servers map[string]dynServer `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal adapted JSON: %v\n%s", err, out)
	}
	for _, srv := range doc.Apps.HTTP.Servers {
		for _, route := range srv.Routes {
			for _, h := range route.HandlersRaw {
				switch h.Handler {
				case "static_response":
					if kind, ok := classifyHandler(h); ok {
						top = append(top, kind)
					}
				case "subroute":
					sink := make(map[int][]string)
					for _, r := range h.Routes {
						walkStaticResponses(r.HandlersRaw, 1, sink)
					}
					for depth, vals := range sink {
						if depth >= 2 {
							nested = append(nested, vals...)
						} else {
							top = append(top, vals...)
						}
					}
				}
			}
		}
		if srv.Errors != nil {
			sink := make(map[int][]string)
			for _, route := range srv.Errors.Routes {
				walkStaticResponses(route.HandlersRaw, 0, sink)
			}
			// the errors route wraps the user subroute in another subroute,
			// so user directives are one level deeper than the regular ones
			for depth, vals := range sink {
				if depth >= 2 {
					errChain = append(errChain, vals...)
				}
			}
		}
	}
	return top, nested, errChain
}

// TestAdaptOrderDoesNotReorderNestedScopes verifies that outer "order"
// constraints reach the site's top-level chain but never into route,
// handle or handle_errors blocks: handle/handle_errors keep their baseline
// local order and route keeps its written local order regardless of the
// outer declaration.
func TestAdaptOrderDoesNotReorderNestedScopes(t *testing.T) {
	t.Run("handle keeps baseline local order", func(t *testing.T) {
		body := `{
	order respond before redir
}
nested-handle.example {
	handle {
		respond "N"
		redir /n /x
	}
	redir /t /q
	respond "T"
}
`
		out := mustAdapt(t, body)
		top, nested, _ := chainsByOrigin(t, out)
		if indexOf(top, "respond:T") > indexOf(top, "redir") {
			t.Errorf("top-level chain not reordered: %v", top)
		}
		// respond was written first in the block, but the baseline local
		// order (redir before respond) must win inside the handle block
		if len(nested) != 2 || nested[0] != "redir" || nested[1] != "respond:N" {
			t.Errorf("handle block reordered by outer order: %v", nested)
		}
	})

	t.Run("route keeps written local order", func(t *testing.T) {
		body := `{
	order respond before redir
}
nested-route.example {
	route {
		respond "R"
		redir /r /z
	}
	redir /t /q
	respond "T"
}
`
		out := mustAdapt(t, body)
		top, nested, _ := chainsByOrigin(t, out)
		if indexOf(top, "respond:T") > indexOf(top, "redir") {
			t.Errorf("top-level chain not reordered: %v", top)
		}
		// route blocks are never sorted; written order must be preserved
		if len(nested) != 2 || nested[0] != "respond:R" || nested[1] != "redir" {
			t.Errorf("route block order changed: %v", nested)
		}
	})

	t.Run("route written order survives a contradicting edge", func(t *testing.T) {
		// edge opposite to the route's written order must not enter it
		body := `{
	order redir before respond
}
nested-route-keep.example {
	route {
		respond "R2"
		redir /r2 /z
	}
	redir /t /q
	respond "T"
}
`
		out := mustAdapt(t, body)
		top, nested, _ := chainsByOrigin(t, out)
		if indexOf(top, "redir") > indexOf(top, "respond:T") {
			t.Errorf("top-level chain not reordered: %v", top)
		}
		if len(nested) != 2 || nested[0] != "respond:R2" || nested[1] != "redir" {
			t.Errorf("outer order rearranged items inside route: %v", nested)
		}
	})

	t.Run("handle_errors keeps baseline local order", func(t *testing.T) {
		body := `{
	order respond before redir
}
nested-errors.example {
	handle_errors {
		respond "E"
		redir /e /y
	}
	redir /t /q
	respond "T"
}
`
		out := mustAdapt(t, body)
		top, _, errChain := chainsByOrigin(t, out)
		if indexOf(top, "respond:T") > indexOf(top, "redir") {
			t.Errorf("top-level chain not reordered: %v", top)
		}
		if len(errChain) != 2 || errChain[0] != "redir" || errChain[1] != "respond:E" {
			t.Errorf("handle_errors chain reordered by outer order: %v", errChain)
		}
	})
}

// TestAdaptOrderFailuresReturnNoPartialResult verifies that invalid order
// graphs fail the whole adaptation with a locatable error, no JSON body and
// no warnings, so a caller cannot mistake the result for a partial success.
func TestAdaptOrderFailuresReturnNoPartialResult(t *testing.T) {
	for i, tc := range []struct {
		name      string
		body      string
		wantParts []string
	}{
		{
			name: "unregistered subject",
			body: `{
	order bogus before redir
}
bad-subject.example {
	respond "x"
}
`,
			wantParts: []string{"bogus is not a registered directive", "Caddyfile:2"},
		},
		{
			name: "unregistered target",
			body: `{
	order respond before bogus
}
bad-target.example {
	respond "x"
}
`,
			wantParts: []string{"bogus is not a registered directive", "Caddyfile:2"},
		},
		{
			name: "contradictory pair",
			body: `{
	order respond before redir
	order redir before respond
}
contradiction.example {
	respond "x"
}
`,
			wantParts: []string{"contradicts", "Caddyfile:3", "Caddyfile:2"},
		},
		{
			name: "cycle",
			body: `{
	order respond before redir
	order redir before vars
	order vars before respond
}
cycle.example {
	respond "x"
}
`,
			wantParts: []string{"cyclic order constraint", "Caddyfile:4", "respond", "redir", "vars"},
		},
	} {
		adapter := caddyfile.Adapter{ServerType: ServerType{}}
		out, warnings, err := adaptBody(t, adapter, tc.body)
		if err == nil {
			t.Errorf("case %d %s: expected error, got JSON %s", i, tc.name, out)
			continue
		}
		if out != nil {
			t.Errorf("case %d %s: failure returned partial JSON (%d bytes)", i, tc.name, len(out))
		}
		if len(warnings) != 0 {
			t.Errorf("case %d %s: failure carried warnings: %v", i, tc.name, warnings)
		}
		for _, part := range tc.wantParts {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("case %d %s: error %q missing %q", i, tc.name, err.Error(), part)
			}
		}
	}
}

// TestAdaptOrderFailureRepairRefail reuses one adapter across
// fail -> succeed -> fail with different file contents. Each result
// reflects only the current input: repaired input succeeds completely,
// re-breaking fails again with the original error, and successful results
// are byte-reproducible with a fresh adapter.
func TestAdaptOrderFailureRepairRefail(t *testing.T) {
	bad := `{
	order respond before redir
	order redir before respond
}
repair.example {
	redir /a /b
	respond "X"
}
`
	good := `{
	order respond before redir
}
repair.example {
	redir /a /b
	respond "X"
}
`
	adapter := caddyfile.Adapter{ServerType: ServerType{}}

	out1, warnings1, err1 := adaptBody(t, adapter, bad)
	if err1 == nil {
		t.Fatalf("first adaptation should fail, got %s", out1)
	}
	firstErr := err1.Error()

	out2, _, err2 := adaptBody(t, adapter, good)
	if err2 != nil {
		t.Fatalf("repaired adaptation should succeed: %v", err2)
	}
	freshOut, _, freshErr := adaptBody(t, caddyfile.Adapter{ServerType: ServerType{}}, good)
	if freshErr != nil {
		t.Fatalf("fresh adapter failed on good config: %v", freshErr)
	}
	if !reflect.DeepEqual(out2, freshOut) {
		t.Fatalf("repaired result differs from a fresh adapter's result")
	}
	if depths := staticByDepth(t, out2); indexOf(depths[1], "respond:X") > indexOf(depths[1], "redir") {
		t.Fatalf("repaired config did not apply its constraint: %v", depths[1])
	}

	out3, warnings3, err3 := adaptBody(t, adapter, bad)
	if err3 == nil {
		t.Fatalf("re-broken adaptation should fail, got %s", out3)
	}
	if err3.Error() != firstErr {
		t.Fatalf("error text drift across identical failures\nfirst: %s\nagain: %s", firstErr, err3.Error())
	}
	if out3 != nil || len(warnings3) != 0 {
		t.Fatalf("re-broken adaptation leaked results: %d bytes, %d warnings", len(out3), len(warnings3))
	}
	_ = warnings1
}

// TestAdaptOrderConstraintsFromImports verifies that edges collected
// through imports are equivalent regardless of import or glob traversal:
// two trees that differ only in how the same constraints are distributed
// across files produce identical JSON.
func TestAdaptOrderConstraintsFromImports(t *testing.T) {
	makeTree := func(t *testing.T, names []string) (string, []byte) {
		t.Helper()
		dir := t.TempDir()
		partsDir := filepath.Join(dir, "parts")
		if err := os.MkdirAll(partsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		lines := []string{
			"order respond before redir\n",
			"order vars after redir\n",
		}
		for i, name := range names {
			if err := os.WriteFile(filepath.Join(partsDir, name), []byte(lines[i]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		root := filepath.Join(dir, "Caddyfile")
		body := []byte(`{
	import parts/*.caddy
}
imported-order.example {
	vars m 1
	redir /a /b
	respond "I"
}
`)
		if err := os.WriteFile(root, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return root, body
	}

	rootA, bodyA := makeTree(t, []string{"a-first.caddy", "b-second.caddy"})
	rootB, bodyB := makeTree(t, []string{"z-first.caddy", "a-second.caddy"})

	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	outA, warnA, errA := adapter.Adapt(bodyA, map[string]any{"filename": rootA})
	if errA != nil {
		t.Fatalf("tree A failed: %v", errA)
	}
	outB, warnB, errB := adapter.Adapt(bodyB, map[string]any{"filename": rootB})
	if errB != nil {
		t.Fatalf("tree B failed: %v", errB)
	}
	if !reflect.DeepEqual(outA, outB) {
		t.Fatalf("glob traversal order leaked into adapted JSON\n%s\n----\n%s", outA, outB)
	}
	if !reflect.DeepEqual(warnA, warnB) {
		t.Fatalf("warnings drift: %v vs %v", warnA, warnB)
	}
	if depths := staticByDepth(t, outA); indexOf(depths[1], "respond:I") < 0 ||
		indexOf(depths[1], "respond:I") > indexOf(depths[1], "redir") {
		t.Fatalf("imported constraint not applied: %v", depths[1])
	}
}

// TestAdaptOrderConstraintsIsolatedAcrossCalls confirms that an
// adaptation never reads ordering state left by a previous call, whether
// the state is carried in a reused options map or a shared Adapter.
func TestAdaptOrderConstraintsIsolatedAcrossCalls(t *testing.T) {
	ordered := `{
	order respond before redir
}
leak-ordered.example {
	redir /a /b
	respond "O"
}
`
	plain := `leak-plain.example {
	redir /a /b
	respond "P"
}
`
	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	sharedOpts := map[string]any{}

	outOrdered, _, err := adapter.Adapt([]byte(ordered), sharedOpts)
	if err != nil {
		t.Fatalf("ordered adapt: %v", err)
	}
	if indexOf(staticByDepth(t, outOrdered)[1], "respond:O") > indexOf(staticByDepth(t, outOrdered)[1], "redir") {
		t.Fatalf("ordered adaptation did not reorder")
	}
	if _, present := sharedOpts["order"]; !present {
		t.Fatal("expected constraints to be collected on the provided options map")
	}

	// a second adaptation reusing the same map must start from a clean
	// constraint set and keep the baseline (redir before respond)
	outPlain, _, err := adapter.Adapt([]byte(plain), sharedOpts)
	if err != nil {
		t.Fatalf("plain adapt: %v", err)
	}
	top := staticByDepth(t, outPlain)[1]
	if indexOf(top, "redir") > indexOf(top, "respond:P") {
		t.Fatalf("plain adaptation inherited the prior call's order: %v", top)
	}
	if _, present := sharedOpts["order"]; present {
		t.Fatal("constraints from the ordered adaptation were not cleared")
	}
}

// TestAdaptOrderResultRepeats verifies byte-identical JSON and warnings
// across repeated calls on one adapter.
func TestAdaptOrderResultRepeats(t *testing.T) {
	good := `ordered-repeat.example {
	redir /a /b
	respond "Z"
}
`
	// intentionally not gofmt-aligned so the formatting warning fires and
	// can be checked for stable ordering
	warningBody := "ordered-repeat.example {\nredir /a /b\nrespond \"Z\"\n}\n"
	adapter := caddyfile.Adapter{ServerType: ServerType{}}

	var first []byte
	var firstWarnings []caddyconfig.Warning
	for i := 0; i < 3; i++ {
		out, warnings, err := adaptBody(t, adapter, good)
		if err != nil {
			t.Fatalf("adapt %d: %v", i, err)
		}
		if first == nil {
			first = out
			firstWarnings = warnings
		} else if !reflect.DeepEqual(out, first) {
			t.Fatalf("adapt %d drifted from the first", i)
		}
		if !reflect.DeepEqual(warnings, firstWarnings) {
			t.Fatalf("adapt %d warnings drifted", i)
		}
	}

	out1, warn1, err1 := adaptBody(t, adapter, warningBody)
	if err1 != nil {
		t.Fatalf("warning body failed: %v", err1)
	}
	out2, warn2, err2 := adaptBody(t, adapter, warningBody)
	if err2 != nil {
		t.Fatalf("warning body repeat failed: %v", err2)
	}
	if !reflect.DeepEqual(out1, out2) || !reflect.DeepEqual(warn1, warn2) {
		t.Fatal("warning-bearing adaptation was not reproducible")
	}
	if len(warn1) == 0 {
		t.Fatal("expected a formatting warning")
	}
}
