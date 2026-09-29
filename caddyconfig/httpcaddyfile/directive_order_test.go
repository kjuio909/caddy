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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// orderConstraint builds a constraint for the unit tests; only the graph
// resolver reads these, so file/line can stay fixed.
func orderConstraint(subject string, pos Positional, anchor string) directiveOrderConstraint {
	return directiveOrderConstraint{
		subject:  subject,
		position: pos,
		anchor:   anchor,
		file:     "Caddyfile",
		line:     2,
	}
}

func indexIn(order []string, dir string) int {
	for i, d := range order {
		if d == dir {
			return i
		}
	}
	return -1
}

// TestResolveDirectiveOrderDefaults verifies that without constraints the
// baseline is returned verbatim, and that a single Before/After edge moves
// a directive next to its anchor while leaving every unrelated pair in its
// default relative order.
func TestResolveDirectiveOrderDefaults(t *testing.T) {
	got, err := resolveDirectiveOrder(defaultDirectiveOrder, nil)
	if err != nil {
		t.Fatalf("baseline resolution failed: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(defaultDirectiveOrder) {
		t.Fatalf("no constraints changed the order:\n%v", got)
	}

	got, err = resolveDirectiveOrder(defaultDirectiveOrder, []directiveOrderConstraint{
		orderConstraint("respond", Before, "redir"),
	})
	if err != nil {
		t.Fatalf("before edge failed: %v", err)
	}
	respondAt, redirAt := indexIn(got, "respond"), indexIn(got, "redir")
	if respondAt != redirAt-1 {
		t.Fatalf("respond should be immediately before redir, got %d vs %d", respondAt, redirAt)
	}
	// unrelated directives keep their default relative positions
	if indexIn(got, "header") > respondAt ||
		indexIn(got, "vars") > indexIn(got, "header") ||
		indexIn(got, "file_server") < redirAt {
		t.Fatalf("unrelated directives moved:\n%v", got)
	}

	got, err = resolveDirectiveOrder(defaultDirectiveOrder, []directiveOrderConstraint{
		orderConstraint("redir", After, "respond"),
	})
	if err != nil {
		t.Fatalf("after edge failed: %v", err)
	}
	if indexIn(got, "respond") != indexIn(got, "redir")-1 {
		t.Fatalf("'redir after respond' should put redir right after respond")
	}
}

// TestResolveDirectiveOrderStableUnderLineOrder shuffles declaration order
// extensively; equivalent constraint sets must always resolve identically.
func TestResolveDirectiveOrderStableUnderLineOrder(t *testing.T) {
	constraints := []directiveOrderConstraint{
		orderConstraint("respond", Before, "redir"),
		orderConstraint("abort", Before, "respond"),
		orderConstraint("file_server", After, "redir"),
		orderConstraint("error", After, "respond"),
	}
	want, err := resolveDirectiveOrder(defaultDirectiveOrder, constraints)
	if err != nil {
		t.Fatalf("reference resolution failed: %v", err)
	}

	// permute the constraint slice (rotations + reversals cover all the
	// adjacencies a swap of declaration lines can produce)
	for shift := 0; shift < len(constraints); shift++ {
		rotated := append(append([]directiveOrderConstraint{}, constraints[shift:]...), constraints[:shift]...)
		got, err := resolveDirectiveOrder(defaultDirectiveOrder, rotated)
		if err != nil {
			t.Fatalf("rotation %d failed: %v", shift, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("rotation %d changed order:\nwant %v\ngot  %v", shift, want, got)
		}

		reversed := append([]directiveOrderConstraint{}, rotated...)
		for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
			reversed[i], reversed[j] = reversed[j], reversed[i]
		}
		got, err = resolveDirectiveOrder(defaultDirectiveOrder, reversed)
		if err != nil {
			t.Fatalf("reversal %d failed: %v", shift, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("reversal %d changed order:\nwant %v\ngot  %v", shift, want, got)
		}
	}
}

// TestResolveDirectiveOrderAnchors covers first/last, including against a
// plugin directive that has no default position.
func TestResolveDirectiveOrderAnchors(t *testing.T) {
	baseline := append(append([]string{}, defaultDirectiveOrder...), "myplugin")

	got, err := resolveDirectiveOrder(baseline, []directiveOrderConstraint{
		orderConstraint("respond", First, ""),
		orderConstraint("redir", Last, ""),
		orderConstraint("myplugin", After, "respond"),
	})
	if err != nil {
		t.Fatalf("anchor resolution failed: %v", err)
	}
	if got[0] != "respond" {
		t.Fatalf("first anchor should lead, got %q", got[0])
	}
	if got[1] != "myplugin" {
		t.Fatalf("plugin ordered after a first-anchored directive should cluster with it, got %q at 1", got[1])
	}
	if got[len(got)-1] != "redir" {
		t.Fatalf("last anchor should trail, got %q", got[len(got)-1])
	}

	// the middle keeps default relative order
	rootAt, headerAt := indexIn(got, "root"), indexIn(got, "header")
	if !(0 < rootAt && rootAt < headerAt) {
		t.Fatalf("default middle order disturbed: root=%d header=%d", rootAt, headerAt)
	}
}

// TestResolveDirectiveOrderPluginNextToStandard verifies a plugin
// directive is parked beside its standard anchor and chains of plugin
// constraints cluster together instead of interleaving with defaults.
func TestResolveDirectiveOrderPluginNextToStandard(t *testing.T) {
	baseline := append(append([]string{}, defaultDirectiveOrder...), "myplugin_a", "myplugin_b")
	got, err := resolveDirectiveOrder(baseline, []directiveOrderConstraint{
		orderConstraint("myplugin_a", After, "vars"),
		orderConstraint("myplugin_b", After, "myplugin_a"),
	})
	if err != nil {
		t.Fatalf("plugin chain failed: %v", err)
	}
	varsAt, aAt, bAt := indexIn(got, "vars"), indexIn(got, "myplugin_a"), indexIn(got, "myplugin_b")
	if aAt != varsAt+1 || bAt != aAt+1 {
		t.Fatalf("plugin chain should sit right after vars, got vars=%d a=%d b=%d\n%v", varsAt, aAt, bAt, got)
	}
}

// TestResolveDirectiveOrderCycle reports every cyclic constraint set with a
// stable, locatable error, and a valid set still resolves after the cycle
// is removed.
func TestResolveDirectiveOrderCycle(t *testing.T) {
	cyclic := []directiveOrderConstraint{
		orderConstraint("redir", Before, "respond"),
		orderConstraint("respond", Before, "abort"),
		orderConstraint("abort", Before, "redir"),
	}

	var firstErr string
	for shift := 0; shift < len(cyclic); shift++ {
		rotated := append(append([]directiveOrderConstraint{}, cyclic[shift:]...), cyclic[:shift]...)
		_, err := resolveDirectiveOrder(defaultDirectiveOrder, rotated)
		if err == nil {
			t.Fatalf("rotation %d: expected a cycle error", shift)
		}
		msg := err.Error()
		for _, dir := range []string{"redir", "respond", "abort"} {
			if !strings.Contains(msg, dir) {
				t.Fatalf("rotation %d error does not name %q: %s", shift, dir, msg)
			}
		}
		if !strings.Contains(msg, "Caddyfile:2") {
			t.Fatalf("rotation %d error is not locatable: %s", shift, msg)
		}
		if firstErr == "" {
			firstErr = msg
		} else if msg != firstErr {
			t.Fatalf("cycle error text drifted under line reordering:\n%s\n%s", firstErr, msg)
		}
	}

	// removing one edge makes the graph acyclic again
	fixed := cyclic[:2]
	if _, err := resolveDirectiveOrder(defaultDirectiveOrder, fixed); err != nil {
		t.Fatalf("repaired graph should resolve: %v", err)
	}
}

// adaptOrderConfig is a small adapter wrapper for the order-option tests.
func adaptOrderConfig(t *testing.T, body string) ([]byte, error) {
	t.Helper()
	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	out, _, err := adapter.Adapt([]byte(body), nil)
	return out, err
}

// TestOrderOptionUnregisteredDirective rejects an unknown subject or
// anchor, naming the directive and its source location.
func TestOrderOptionUnregisteredDirective(t *testing.T) {
	for _, body := range []string{
		"{\n\torder nosuchdirective before redir\n}\nexample.com {\n\tredir /a /b\n}\n",
		"{\n\torder redir after nosuchdirective\n}\nexample.com {\n\tredir /a /b\n}\n",
	} {
		out, err := adaptOrderConfig(t, body)
		if err == nil {
			t.Fatalf("expected error for %q", body)
		}
		if out != nil {
			t.Fatalf("error returned partial JSON for %q: %s", body, out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "not a registered directive") ||
			!strings.Contains(msg, "nosuchdirective") ||
			!strings.Contains(msg, "Caddyfile:2") {
			t.Fatalf("unexpected error: %s", msg)
		}
	}
}

// TestOrderOptionConflictingRelations rejects opposite relations on the
// same pair and first/last on the same directive.
func TestOrderOptionConflictingRelations(t *testing.T) {
	cases := map[string]string{
		"before then after":  "{\n\torder respond before redir\n\torder redir before respond\n}\nexample.com {\n\tredir /a /b\n}\n",
		"after then before":  "{\n\torder redir before respond\n\torder respond before redir\n}\nexample.com {\n\tredir /a /b\n}\n",
		"first and last":     "{\n\torder respond first\n\torder respond last\n}\nexample.com {\n\tredir /a /b\n}\n",
		"relative to itself": "{\n\torder respond before respond\n}\nexample.com {\n\tredir /a /b\n}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := adaptOrderConfig(t, body)
			if err == nil {
				t.Fatal("expected conflict error")
			}
			if out != nil {
				t.Fatalf("conflict returned partial JSON: %s", out)
			}
			if !strings.Contains(err.Error(), "conflicting") &&
				!strings.Contains(err.Error(), "relative to itself") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestOrderOptionCycleFailsAdaptation ensures a cyclic relation graph
// fails the whole adaptation: no JSON at all and an error anchored to the
// declaration.
func TestOrderOptionCycleFailsAdaptation(t *testing.T) {
	body := "{\n\torder redir before respond\n\torder respond before abort\n\torder abort before redir\n}\nexample.com {\n\tredir /a /b\n\tabort\n\trespond \"X\"\n}\n"
	out, err := adaptOrderConfig(t, body)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if out != nil {
		t.Fatalf("cycle returned partial JSON: %s", out)
	}
	if !strings.Contains(err.Error(), "cyclic directive order constraint") ||
		!strings.Contains(err.Error(), "Caddyfile:") {
		t.Fatalf("error not locatable: %v", err)
	}
}

// offsetOf finds the first byte offset of needle in out.
func offsetOf(out []byte, needle string) int {
	return bytes.Index(out, []byte(needle))
}

// TestOrderOptionOnlyAffectsSiteTopLevel verifies the scope rule: the
// global order rearranges a site's top-level chain, but a handle block and
// a route block inside that site keep their written local order.
func TestOrderOptionOnlyAffectsSiteTopLevel(t *testing.T) {
	body := "{\n\torder respond before redir\n}\nexample.com {\n\tredir /old /new\n\trespond \"OUTER\"\n\thandle /inner {\n\t\tredir /i-old /i-new\n\t\trespond \"INNER-HANDLE\"\n\t}\n\troute /routed {\n\t\tredir /r-old /r-new\n\t\trespond \"INNER-ROUTE\"\n\t}\n}\n"
	out, err := adaptOrderConfig(t, body)
	if err != nil {
		t.Fatal(err)
	}

	// top level: respond (OUTER) must run before the top-level redir (the
	// first 302 in the JSON byte stream)
	outerRespond := offsetOf(out, `"body":"OUTER"`)
	first302 := offsetOf(out, `"status_code":302`)
	if outerRespond < 0 || first302 < 0 || outerRespond > first302 {
		t.Fatalf("top-level respond should precede top-level redir: respond=%d redir=%d", outerRespond, first302)
	}

	// inner handle: its redir (the 302 immediately preceding INNER-HANDLE)
	// keeps textual order, i.e. occurs before INNER-HANDLE
	innerHandle := offsetOf(out, `"body":"INNER-HANDLE"`)
	segment := out[first302 : innerHandle+1]
	if !bytes.Contains(segment, []byte(`"status_code":302`)) {
		t.Fatal("inner handle redir should still precede its respond")
	}

	// inner route likewise keeps its written order
	innerRoute := offsetOf(out, `"body":"INNER-ROUTE"`)
	if innerRoute < 0 {
		t.Fatal("missing inner route respond")
	}
	routeSegment := out[innerHandle : innerRoute+1]
	if !bytes.Contains(routeSegment, []byte(`"status_code":302`)) {
		t.Fatal("inner route redir should still precede its respond")
	}
}

// TestOrderOptionDoesNotCrossSites ensures directives from one site never
// move into another: each site's subroute contains only its own handlers.
func TestOrderOptionDoesNotCrossSites(t *testing.T) {
	body := "{\n\torder respond before redir\n}\none.example.com {\n\tredir /a /b\n\trespond \"ONE\"\n}\ntwo.example.com {\n\tredir /c /d\n\trespond \"TWO\"\n}\n"
	out, err := adaptOrderConfig(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(out, []byte(`"body":"ONE"`)) != 1 || bytes.Count(out, []byte(`"body":"TWO"`)) != 1 {
		t.Fatal("site handler bodies leaked across sites")
	}
}

// TestOrderOptionAdapterReuseFailFixRefail drives one shared Adapter
// through fail -> repair -> succeed -> re-break -> fail. Each result must
// reflect only the current file tree, successful JSON must be byte-stable,
// and the failure chain must be identical across attempts.
func TestOrderOptionAdapterReuseFailFixRefail(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Caddyfile")

	broken := []byte("{\n\torder respond before redir\n\torder redir before respond\n}\nexample.com {\n\tredir /a /b\n}\n")
	fixed := []byte("{\n\torder respond before redir\n}\nexample.com {\n\tredir /a /b\n\trespond \"FIXED\"\n}\n")

	write := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(root, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	run := func() ([]byte, error) {
		body, err := os.ReadFile(root)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := adapter.Adapt(body, map[string]any{"filename": root})
		return out, err
	}

	write(broken)
	out1, err1 := run()
	if err1 == nil {
		t.Fatal("first adapt should fail")
	}
	if out1 != nil {
		t.Fatal("failed adapt returned JSON")
	}
	out1b, err1b := run()
	if err1b == nil || err1b.Error() != err1.Error() {
		t.Fatalf("repeated failure should be identical:\n%v\n%v", err1, err1b)
	}
	if out1b != nil {
		t.Fatal("repeated failure returned JSON")
	}

	write(fixed)
	out2, err2 := run()
	if err2 != nil {
		t.Fatalf("repaired adapt failed: %v", err2)
	}
	if !bytes.Contains(out2, []byte("FIXED")) {
		t.Fatalf("repaired JSON does not reflect current tree: %s", out2)
	}
	out2b, err2b := run()
	if err2b != nil || !bytes.Equal(out2, out2b) {
		t.Fatalf("repaired result is not byte-stable: err=%v", err2b)
	}

	write(broken)
	out3, err3 := run()
	if err3 == nil {
		t.Fatal("re-broken adapt should fail")
	}
	if out3 != nil {
		t.Fatal("re-broken adapt returned JSON")
	}
	if err3.Error() != err1.Error() {
		t.Fatalf("re-broken error chain drifted:\nwant %s\ngot  %s", err1, err3)
	}
}

// TestOrderOptionNoLeakAcrossReusedOptionsMap reuses a single options map
// (the "order" key aside) across an ordered and an unordered adaptation:
// the second call must not inherit the first call's constraints.
func TestOrderOptionNoLeakAcrossReusedOptionsMap(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	opts := map[string]any{}

	ordered := []byte("{\n\torder respond before redir\n}\nexample.com {\n\tredir /a /b\n\trespond \"O\"\n}\n")
	out, _, err := adapter.Adapt(ordered, opts)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Index(out, []byte(`"body":"O"`)) > bytes.Index(out, []byte(`"status_code":302`)) {
		t.Fatal("ordered adapt did not put respond first")
	}

	plain := []byte("example.com {\n\tredir /a /b\n\trespond \"P\"\n}\n")
	out, _, err = adapter.Adapt(plain, opts)
	if err != nil {
		t.Fatal(err)
	}
	// default order has redir (302) before the respond body
	if bytes.Index(out, []byte(`"status_code":302`)) > bytes.Index(out, []byte(`"body":"P"`)) {
		t.Fatal("plain adapt inherited a previous call's directive order")
	}
}

// TestOrderOptionStableAcrossImportArrangement builds two equivalent file
// trees whose only difference is the traversal order in which imports
// contribute directives and the order in which glob patterns are listed.
// Because the trees live in different temp directories the JSON cannot be
// compared byte-for-byte, so it asserts the decisive property instead:
// the imported handlers end up in the same relative pipeline order.
func TestOrderOptionStableAcrossImportArrangement(t *testing.T) {
	makeTree := func(t *testing.T, dir string, reversedPatterns bool) string {
		t.Helper()
		writeAdaptFile(t, filepath.Join(dir, "parts", "p1.caddy"), "redir /old /new\n")
		writeAdaptFile(t, filepath.Join(dir, "parts", "p2.caddy"), "respond \"IMPORTED\"\n")
		writeAdaptFile(t, filepath.Join(dir, "site.caddy"), "import parts/p2.caddy\nimport parts/p1.caddy\n")
		// a single glob with two overlapping patterns: each regular file is
		// hit by both patterns but must still expand only once
		patterns := "import parts/*.caddy parts/p*.caddy\n"
		rootBody := "{\n\torder respond before redir\n}\nexample.com {\n\timport site.caddy\n\t" + patterns + "}\n"
		if reversedPatterns {
			rootBody = "{\n\torder respond before redir\n}\nexample.com {\n\t" + patterns + "import site.caddy\n}\n"
		}
		root := filepath.Join(dir, "Caddyfile")
		writeAdaptFile(t, root, rootBody)
		return root
	}

	dirA, dirB := t.TempDir(), t.TempDir()
	rootA := makeTree(t, dirA, false)
	rootB := makeTree(t, dirB, true)

	adapter := caddyfile.Adapter{ServerType: ServerType{}}
	adaptFile := func(root string) []byte {
		t.Helper()
		body, err := os.ReadFile(root)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := adapter.Adapt(body, map[string]any{"filename": root})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	outA := adaptFile(rootA)
	outB := adaptFile(rootB)
	// A successful JSON carries no source paths, so equivalent trees must
	// adapt to identical bytes regardless of import/glob traversal order.
	if !bytes.Equal(outA, outB) {
		t.Fatalf("equivalent import arrangements produced different JSON:\n%s\n%s", outA, outB)
	}
	// and the imported respond must precede its redir under the order edge
	iRespond := offsetOf(outA, `"body":"IMPORTED"`)
	if iRespond < 0 {
		t.Fatal("missing imported respond")
	}
	if bytes.Contains(outA[:iRespond], []byte(`"status_code":302`)) {
		t.Fatal("imported directives were not reordered equivalently")
	}
}
