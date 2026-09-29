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
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// defaultDirectiveOrder specifies the default order
// to apply directives in HTTP routes. This must only
// consist of directives that are included in Caddy's
// standard distribution.
//
// e.g. The 'root' directive goes near the start in
// case rewrites or redirects depend on existence of
// files, i.e. the file matcher, which must know the
// root first.
//
// e.g. The 'header' directive goes before 'redir' so
// that headers can be manipulated before doing redirects.
//
// e.g. The 'respond' directive is near the end because it
// writes a response and terminates the middleware chain.
var defaultDirectiveOrder = []string{
	"tracing",

	// set variables that may be used by other directives
	"map",
	"vars",
	"fs",
	"root",
	"log_append",
	"skip_log", // TODO: deprecated, renamed to log_skip
	"log_skip",
	"log_name",

	"header",
	"copy_response_headers", // only in reverse_proxy's handle_response
	"request_body",

	"redir",

	// incoming request manipulation
	"method",
	"rewrite",
	"uri",
	"try_files",

	// middleware handlers; some wrap responses
	"basicauth", // TODO: deprecated, renamed to basic_auth
	"basic_auth",
	"forward_auth",
	"request_header",
	"encode",
	"push",
	"intercept",
	"templates",

	// special routing & dispatching directives
	"invoke",
	"handle",
	"handle_path",
	"route",

	// handlers that typically respond to requests
	"abort",
	"error",
	"copy_response", // only in reverse_proxy's handle_response
	"respond",
	"metrics",
	"reverse_proxy",
	"php_fastcgi",
	"file_server",
	"acme_server",
}

// directiveOrder specifies the baseline order to apply directives in HTTP
// routes, modified at init time by plugins via RegisterDirectiveOrder. It is
// immutable after package initialization: the "order" global option only
// accumulates per-adaptation constraints (stored in the adaptation's options
// and Helper) which are merged with this baseline into a fresh order for
// that adaptation, so concurrent adaptations never share or leak directive
// ordering.
var directiveOrder = defaultDirectiveOrder

// RegisterDirective registers a unique directive dir with an
// associated unmarshaling (setup) function. When directive dir
// is encountered in a Caddyfile, setupFunc will be called to
// unmarshal its tokens.
func RegisterDirective(dir string, setupFunc UnmarshalFunc) {
	if _, ok := registeredDirectives[dir]; ok {
		panic("directive " + dir + " already registered")
	}
	registeredDirectives[dir] = setupFunc
}

// RegisterHandlerDirective is like RegisterDirective, but for
// directives which specifically output only an HTTP handler.
// Directives registered with this function will always have
// an optional matcher token as the first argument.
func RegisterHandlerDirective(dir string, setupFunc UnmarshalHandlerFunc) {
	RegisterDirective(dir, func(h Helper) ([]ConfigValue, error) {
		if !h.Next() {
			return nil, h.ArgErr()
		}

		matcherSet, err := h.ExtractMatcherSet()
		if err != nil {
			return nil, err
		}

		val, err := setupFunc(h)
		if err != nil {
			return nil, err
		}

		return h.NewRoute(matcherSet, val), nil
	})
}

// RegisterDirectiveOrder registers the default order for a
// directive from a plugin.
//
// This is useful when a plugin has a well-understood place
// it should run in the middleware pipeline, and it allows
// users to avoid having to define the order themselves.
//
// The directive dir may be placed in the position relative
// to ('before' or 'after') a directive included in Caddy's
// standard distribution. It cannot be relative to another
// plugin's directive.
//
// EXPERIMENTAL: This API may change or be removed.
func RegisterDirectiveOrder(dir string, position Positional, standardDir string) {
	// check if directive was already ordered
	if slices.Contains(directiveOrder, dir) {
		panic("directive '" + dir + "' already ordered")
	}

	if position != Before && position != After {
		panic("the 2nd argument must be either 'before' or 'after', got '" + position + "'")
	}

	// check if directive exists in standard distribution, since
	// we can't allow plugins to depend on one another; we can't
	// guarantee the order that plugins are loaded in.
	foundStandardDir := slices.Contains(defaultDirectiveOrder, standardDir)
	if !foundStandardDir {
		panic("the 3rd argument '" + standardDir + "' must be a directive that exists in the standard distribution of Caddy")
	}

	// insert directive into proper position
	newOrder := directiveOrder
	for i, d := range newOrder {
		if d != standardDir {
			continue
		}
		switch position {
		case Before:
			newOrder = append(newOrder[:i], append([]string{dir}, newOrder[i:]...)...)
		case After:
			newOrder = append(newOrder[:i+1], append([]string{dir}, newOrder[i+1:]...)...)
		case First, Last:
		}
		break
	}
	directiveOrder = newOrder
}

// RegisterGlobalOption registers a unique global option opt with
// an associated unmarshaling (setup) function. When the global
// option opt is encountered in a Caddyfile, setupFunc will be
// called to unmarshal its tokens.
func RegisterGlobalOption(opt string, setupFunc UnmarshalGlobalFunc) {
	if _, ok := registeredGlobalOptions[opt]; ok {
		panic("global option " + opt + " already registered")
	}
	registeredGlobalOptions[opt] = setupFunc
}

// Helper is a type which helps setup a value from
// Caddyfile tokens.
type Helper struct {
	*caddyfile.Dispenser
	// State stores intermediate variables during caddyfile adaptation.
	State map[string]any
	// BlockState stores intermediate variables scoped to the current block.
	// It propagates down, but unlike state not back up from child to parent.
	BlockState   map[string]any
	options      map[string]any
	warnings     *[]caddyconfig.Warning
	matcherDefs  map[string]caddy.ModuleMap
	parentBlock  caddyfile.ServerBlock
	groupCounter counter
	// directiveOrder is the directive ordering in effect for this Helper's
	// scope. The site's top-level chain uses the per-adaptation order merged
	// from the "order" global option; nested scopes (route, handle and other
	// subroutes) use the registered baseline, since outer ordering must not
	// rearrange items within a nested handler. It is never shared across
	// adaptations.
	directiveOrder []string
}

// orderConstraint is one relative-position declaration from the "order"
// global option: dir is the directive being placed; pos is Before, After,
// First or Last; otherDir is the directive it is placed relative to (empty
// for First and Last). File and Line locate the declaration for errors.
type orderConstraint struct {
	dir      string
	pos      Positional
	otherDir string
	file     string
	line     int
}

// orderConstraints is the set of "order" declarations collected for one
// adaptation, in the order they were written.
type orderConstraints []orderConstraint

// directiveOrderForOptions returns the directive order in effect for an
// adaptation given its options. The "order" global option accumulates the
// adaptation's constraints; they are merged with the registered baseline
// into a fresh order by a stable topological sort. Without constraints the
// registered baseline is used. This never reads or writes shared mutable
// state.
func directiveOrderForOptions(options map[string]any) ([]string, error) {
	if options != nil {
		if constraints, ok := options["order"].(orderConstraints); ok && len(constraints) > 0 {
			return resolveDirectiveOrder(directiveOrder, constraints)
		}
	}
	return directiveOrder, nil
}

// orderEdge is a directed ordering relation from one directive to another.
type orderEdge struct{ from, to string }

// resolveDirectiveOrder merges the baseline directive order with the
// before/after/first/last constraints of one adaptation into a single stable
// order. The constraints form a directed graph of user declarations only;
// the baseline is not part of that graph, it only supplies the traversal
// order. Baseline directives are emitted in their default order, but before
// a directive is emitted every directive constrained to run before it is
// pulled up recursively, so a reordered directive lands adjacent to its
// anchor (like a single insertion) while transitive constraints are honored.
// Because graph construction never depends on the sequence in which the
// constraints were collected, swapping declaration lines, import traversal
// order or glob order cannot change the result as long as the constraint
// set is equivalent. Directives unrelated to the constraints keep their
// default positions.
func resolveDirectiveOrder(baseline []string, constraints orderConstraints) ([]string, error) {
	// rank of each node in the baseline; directives absent from the
	// baseline (handler plugins without a registered default position)
	// sort after all baseline directives and tiebreak lexicographically,
	// so their placement never depends on declaration order
	baselineSet := make(map[string]struct{}, len(baseline))
	for _, dir := range baseline {
		baselineSet[dir] = struct{}{}
	}
	for _, c := range constraints {
		if _, ok := baselineSet[c.dir]; !ok {
			baselineSet[c.dir] = struct{}{}
		}
		if c.otherDir != "" {
			if _, ok := baselineSet[c.otherDir]; !ok {
				baselineSet[c.otherDir] = struct{}{}
			}
		}
	}
	rank := make(map[string]int, len(baselineSet))
	for i, dir := range baseline {
		rank[dir] = i
	}
	var extras []string
	for dir := range baselineSet {
		if _, ok := rank[dir]; ok {
			continue
		}
		extras = append(extras, dir)
	}
	sort.Strings(extras)
	for i, dir := range extras {
		rank[dir] = len(baseline) + i
	}
	nodes := make([]string, 0, len(baselineSet))
	for dir := range baselineSet {
		nodes = append(nodes, dir)
	}
	sort.Slice(nodes, func(i, j int) bool { return rank[nodes[i]] < rank[nodes[j]] })

	// every user-declared edge remembers its first declaration; this makes
	// contradictions locatable
	origin := make(map[orderEdge]orderConstraint)
	constraintText := func(c orderConstraint) string {
		if c.otherDir != "" {
			return fmt.Sprintf("%s %s %s", c.dir, c.pos, c.otherDir)
		}
		return fmt.Sprintf("%s %s", c.dir, c.pos)
	}
	addEdge := func(from, to string, c orderConstraint) error {
		e := orderEdge{from, to}
		if _, ok := origin[e]; ok {
			// the same relation, possibly declared again, is idempotent
			return nil
		}
		if reverse, ok := origin[orderEdge{to, from}]; ok {
			return fmt.Errorf("%s:%d: conflicting order constraints for directives '%s' and '%s': %q (here) contradicts %q (at %s:%d)",
				c.file, c.line, from, to, constraintText(c), constraintText(reverse), reverse.file, reverse.line)
		}
		origin[e] = c
		return nil
	}

	// first/last constraints fix a section rather than a single neighbor;
	// track their subjects separately (with their declarations, for
	// locatable errors) so the adjacency pass only walks the one-anchor
	// before/after edges
	firstDirs := make(map[string]struct{})
	lastDirs := make(map[string]struct{})
	firstDecl := make(map[string]orderConstraint)
	lastDecl := make(map[string]orderConstraint)

	// pass 1: one-anchor before/after edges, plus collect section subjects.
	// Sections are expanded in pass 2 once all single edges are known.
	for _, c := range constraints {
		switch c.pos {
		case Before, After:
			// a directive ordered relative to itself imposes no relation;
			// ignore it rather than reading it as a self-loop cycle
			if c.dir == c.otherDir {
				continue
			}
			if c.pos == Before {
				if err := addEdge(c.dir, c.otherDir, c); err != nil {
					return nil, err
				}
			} else {
				if err := addEdge(c.otherDir, c.dir, c); err != nil {
					return nil, err
				}
			}
		case First:
			firstDirs[c.dir] = struct{}{}
			if _, seen := firstDecl[c.dir]; !seen {
				firstDecl[c.dir] = c
			}
		case Last:
			lastDirs[c.dir] = struct{}{}
			if _, seen := lastDecl[c.dir]; !seen {
				lastDecl[c.dir] = c
			}
		default:
			return nil, fmt.Errorf("%s:%d: unknown positional '%s'", c.file, c.line, c.pos)
		}
	}

	// adjacency of the one-anchor edges; a directive constrained before a
	// first-section directive belongs to the first section too, and one
	// constrained after a last-section directive belongs to the last
	singleSucc := make(map[string][]string)
	singlePred := make(map[string][]string)
	recordSingle := func(from, to string) {
		singleSucc[from] = append(singleSucc[from], to)
		singlePred[to] = append(singlePred[to], from)
	}
	// origin maps user edges to their declarations; edges originating from
	// before/after constraints are exactly the ones with a non-empty anchor
	for e, c := range origin {
		if c.otherDir != "" {
			recordSingle(e.from, e.to)
		}
	}
	firstSet := closureSection(firstDirs, singlePred)
	lastSet := closureSection(lastDirs, singleSucc)

	// a directive cannot belong to both end sections: that would require it
	// to run before everything and after everything at once
	if overlap := lowestRanked(firstSet, lastSet, rank); overlap != "" {
		firstSeed, lastSeed := reachingSeed(overlap, firstDirs, singlePred), reachingSeed(overlap, lastDirs, singleSucc)
		fc, lc := firstDecl[firstSeed], lastDecl[lastSeed]
		return nil, fmt.Errorf("%s:%d: conflicting order constraints around directive '%s': %s %s (at %s:%d) forces it to the front but %s %s (at %s:%d) forces it to the back",
			lc.file, lc.line, overlap, fc.dir, fc.pos, fc.file, fc.line, lc.dir, lc.pos, lc.file, lc.line)
	}

	// pass 2: fan-out edges for section subjects, skipping members of the
	// subject's own section (two "first" directives are mutually
	// unconstrained and rank-tiebreak instead). A reverse edge here means
	// the section contradicts a before/after declaration.
	for _, c := range constraints {
		switch c.pos {
		case First:
			for _, node := range nodes {
				if node == c.dir {
					continue
				}
				if _, inSection := firstSet[node]; inSection {
					continue
				}
				if err := addEdge(c.dir, node, c); err != nil {
					return nil, err
				}
			}
		case Last:
			for _, node := range nodes {
				if node == c.dir {
					continue
				}
				if _, inSection := lastSet[node]; inSection {
					continue
				}
				if err := addEdge(node, c.dir, c); err != nil {
					return nil, err
				}
			}
		}
	}

	// build adjacency lists and in-degrees from the full user graph
	succ := make(map[string][]string, len(nodes))
	indeg := make(map[string]int, len(nodes))
	for _, node := range nodes {
		indeg[node] = 0
	}
	for e := range origin {
		succ[e.from] = append(succ[e.from], e.to)
		indeg[e.to]++
	}
	rankLess := func(a, b string) bool { return rank[a] < rank[b] }
	for n := range succ {
		sort.Slice(succ[n], func(i, j int) bool { return rankLess(succ[n][i], succ[n][j]) })
		sort.Slice(singleSucc[n], func(i, j int) bool { return rankLess(singleSucc[n][i], singleSucc[n][j]) })
		sort.Slice(singlePred[n], func(i, j int) bool { return rankLess(singlePred[n][i], singlePred[n][j]) })
	}

	// reject cycles before emitting; a graph with a cycle has no valid order
	remaining := make(map[string]int, len(nodes))
	for node, d := range indeg {
		remaining[node] = d
	}
	queue := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if remaining[node] == 0 {
			queue = append(queue, node)
		}
	}
	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range succ[cur] {
			remaining[next]--
			if remaining[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(nodes) {
		return nil, orderCycleError(nodes, succ, remaining, rankLess, origin)
	}

	// emit the three sections in order: first, middle, last. Within each
	// section nodes are produced by the same baseline-driven, constraint-
	// adjacent walk, restricted to the section; unconstrained members of a
	// section tiebreak by baseline rank, so two "first" or two "last"
	// directives are stable and never depend on declaration order.
	emitted := make(map[string]bool, len(nodes))
	ordered := make([]string, 0, len(nodes))

	allSinglePredsEmitted := func(node string) bool {
		for _, prerequisite := range singlePred[node] {
			if !emitted[prerequisite] {
				return false
			}
		}
		return true
	}
	var visit func(string, map[string]bool)
	visit = func(node string, allowed map[string]bool) {
		// mark on entry: a prerequisite pull followed by a push-down can
		// re-enter this same node before its outer call has finished
		if emitted[node] || !allowed[node] {
			return
		}
		emitted[node] = true
		for _, prerequisite := range singlePred[node] {
			visit(prerequisite, allowed)
		}
		ordered = append(ordered, node)
		for _, successor := range singleSucc[node] {
			// only place an "after" subject once every one of its anchors
			// has been emitted, or pulling it here would drag a later
			// anchor out of position
			if !emitted[successor] && allowed[successor] && allSinglePredsEmitted(successor) {
				visit(successor, allowed)
			}
		}
	}

	emitSection := func(set map[string]struct{}) {
		allowed := make(map[string]bool, len(set))
		var members []string
		for dir := range set {
			allowed[dir] = true
			members = append(members, dir)
		}
		sort.Slice(members, func(i, j int) bool { return rankLess(members[i], members[j]) })
		for _, dir := range members {
			visit(dir, allowed)
		}
	}

	emitSection(firstSet)

	middle := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if _, inFirst := firstSet[node]; inFirst {
			continue
		}
		if _, inLast := lastSet[node]; inLast {
			continue
		}
		middle[node] = true
	}
	for _, node := range nodes {
		visit(node, middle)
	}

	emitSection(lastSet)

	return ordered, nil
}

// closureSection expands a set of section subjects: following neighbors
// turns every directive transitively tied into the section into a member.
// The first section pulls in directives constrained before its subjects
// (predecessors); the last section pulls in directives constrained after
// its subjects (successors).
func closureSection(seeds map[string]struct{}, neighbors map[string][]string) map[string]struct{} {
	set := make(map[string]struct{}, len(seeds))
	var stack []string
	for seed := range seeds {
		set[seed] = struct{}{}
		stack = append(stack, seed)
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range neighbors[cur] {
			if _, ok := set[next]; ok {
				continue
			}
			set[next] = struct{}{}
			stack = append(stack, next)
		}
	}
	return set
}

// lowestRanked returns the lowest-ranked directive present in both sets, or
// "" when the sets are disjoint.
func lowestRanked(a, b map[string]struct{}, rank map[string]int) string {
	overlap := ""
	for dir := range a {
		if _, ok := b[dir]; !ok {
			continue
		}
		if overlap == "" || rank[dir] < rank[overlap] {
			overlap = dir
		}
	}
	return overlap
}

// reachingSeed returns the lowest-ranked seed from which target is
// reachable through neighbors. Callers guarantee such a seed exists.
func reachingSeed(target string, seeds map[string]struct{}, neighbors map[string][]string) string {
	found := ""
	for seed := range seeds {
		seen := map[string]bool{seed: true}
		stack := []string{seed}
		reachable := false
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == target {
				reachable = true
				break
			}
			for _, next := range neighbors[cur] {
				if !seen[next] {
					seen[next] = true
					stack = append(stack, next)
				}
			}
		}
		if reachable && (found == "" || seed < found) {
			found = seed
		}
	}
	return found
}

// orderCycleError builds a deterministic, locatable error for a cycle in the
// ordering graph. Nodes still carrying a positive in-degree after a Kahn pass
// comprise the cycles plus everything downstream of them; downstream nodes
// are peeled off from the sinks until only cycle nodes remain. The reported
// walk then starts at the lowest-ranked cycle node and follows
// lowest-ranked successors until it returns to that node, and names the user
// declarations (file:line) that supply the walked edges.
func orderCycleError(
	nodes []string,
	succ map[string][]string,
	remaining map[string]int,
	rankLess func(a, b string) bool,
	origin map[orderEdge]orderConstraint,
) error {
	onCycle := make(map[string]bool)
	for node, deg := range remaining {
		if deg > 0 {
			onCycle[node] = true
		}
	}
	// peel off nodes downstream of the cycle(s): a remaining node with no
	// remaining successor cannot itself be on a cycle
	for {
		var sink string
		for node := range onCycle {
			hasCycleSucc := false
			for _, next := range succ[node] {
				if onCycle[next] {
					hasCycleSucc = true
					break
				}
			}
			if !hasCycleSucc {
				sink = node
				break
			}
		}
		if sink == "" {
			break
		}
		delete(onCycle, sink)
	}

	start := ""
	for _, node := range nodes {
		if onCycle[node] && (start == "" || rankLess(node, start)) {
			start = node
		}
	}

	// deterministic walk; it may close on a node other than the start when
	// several cycles share nodes, in which case rotate to the repeated node
	walk := []string{start}
	var walked []orderEdge
	index := map[string]int{start: 0}
	cur := start
	for {
		next := ""
		for _, candidate := range succ[cur] {
			if !onCycle[candidate] {
				continue
			}
			if next == "" || rankLess(candidate, next) {
				next = candidate
			}
		}
		walked = append(walked, orderEdge{cur, next})
		if at, repeated := index[next]; repeated {
			walk = append(walk, next)
			walk = walk[at:]
			walked = walked[at:]
			break
		}
		index[next] = len(walk)
		walk = append(walk, next)
		cur = next
	}

	var locs []string
	seen := make(map[orderEdge]bool)
	for _, e := range walked {
		if seen[e] {
			continue
		}
		seen[e] = true
		if c, ok := origin[e]; ok {
			locs = append(locs, fmt.Sprintf("%s:%d", c.file, c.line))
		}
	}
	sort.Strings(locs)
	msg := "cyclic order constraint detected: " + strings.Join(walk, " -> ")
	if len(locs) > 0 {
		msg += " (declared at " + strings.Join(locs, ", ") + ")"
	}
	return errors.New(msg)
}

// nestedDirectiveOrderFor returns the order used inside nested scopes
// (route, handle and other subroutes). Outer "order" constraints must not
// rearrange items within those scopes, so their directives keep the
// registered baseline positions; directives absent from the baseline
// (plugins without a registered default position) are appended so they
// remain usable inside nested scopes without changing local ordering.
func nestedDirectiveOrderFor(adaptOrder []string) []string {
	nested := append([]string{}, directiveOrder...)
	for _, dir := range adaptOrder {
		if !slices.Contains(nested, dir) {
			nested = append(nested, dir)
		}
	}
	return nested
}

// Option gets the option keyed by name.
func (h Helper) Option(name string) any {
	return h.options[name]
}

// effectiveDirectiveOrder returns the directive order carried by this
// Helper. Site top-level Helpers carry the per-adaptation order merged from
// the "order" global option; the method itself never resolves or mutates
// shared state.
func (h Helper) effectiveDirectiveOrder() []string {
	if len(h.directiveOrder) > 0 {
		return h.directiveOrder
	}
	return directiveOrder
}

// Caddyfiles returns the list of config files from
// which tokens in the current server block were loaded.
func (h Helper) Caddyfiles() []string {
	// first obtain set of names of files involved
	// in this server block, without duplicates
	files := make(map[string]struct{})
	for _, segment := range h.parentBlock.Segments {
		for _, token := range segment {
			files[token.File] = struct{}{}
		}
	}
	// then convert the set into a slice
	filesSlice := make([]string, 0, len(files))
	for file := range files {
		filesSlice = append(filesSlice, file)
	}
	sort.Strings(filesSlice)
	return filesSlice
}

// JSON converts val into JSON. Any errors are added to warnings.
func (h Helper) JSON(val any) json.RawMessage {
	return caddyconfig.JSON(val, h.warnings)
}

// MatcherToken assumes the next argument token is (possibly) a matcher,
// and if so, returns the matcher set along with a true value. If the next
// token is not a matcher, nil and false is returned. Note that a true
// value may be returned with a nil matcher set if it is a catch-all.
func (h Helper) MatcherToken() (caddy.ModuleMap, bool, error) {
	if !h.NextArg() {
		return nil, false, nil
	}
	return matcherSetFromMatcherToken(h.Dispenser.Token(), h.matcherDefs, h.warnings)
}

// ExtractMatcherSet is like MatcherToken, except this is a higher-level
// method that returns the matcher set described by the matcher token,
// or nil if there is none, and deletes the matcher token from the
// dispenser and resets it as if this look-ahead never happened. Useful
// when wrapping a route (one or more handlers) in a user-defined matcher.
func (h Helper) ExtractMatcherSet() (caddy.ModuleMap, error) {
	matcherSet, hasMatcher, err := h.MatcherToken()
	if err != nil {
		return nil, err
	}
	if hasMatcher {
		// strip matcher token; we don't need to
		// use the return value here because a
		// new dispenser should have been made
		// solely for this directive's tokens,
		// with no other uses of same slice
		h.Dispenser.Delete()
	}
	h.Dispenser.Reset() // pretend this lookahead never happened
	return matcherSet, nil
}

// NewRoute returns config values relevant to creating a new HTTP route.
func (h Helper) NewRoute(matcherSet caddy.ModuleMap,
	handler caddyhttp.MiddlewareHandler,
) []ConfigValue {
	mod, err := caddy.GetModule(caddy.GetModuleID(handler))
	if err != nil {
		*h.warnings = append(*h.warnings, caddyconfig.Warning{
			File:    h.File(),
			Line:    h.Line(),
			Message: err.Error(),
		})
		return nil
	}
	var matcherSetsRaw []caddy.ModuleMap
	if matcherSet != nil {
		matcherSetsRaw = append(matcherSetsRaw, matcherSet)
	}
	return []ConfigValue{
		{
			Class: "route",
			Value: caddyhttp.Route{
				MatcherSetsRaw: matcherSetsRaw,
				HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(handler, "handler", mod.ID.Name(), h.warnings)},
			},
		},
	}
}

// GroupRoutes adds the routes (caddyhttp.Route type) in vals to the
// same group, if there is more than one route in vals.
func (h Helper) GroupRoutes(vals []ConfigValue) {
	// ensure there's at least two routes; group of one is pointless
	var count int
	for _, v := range vals {
		if _, ok := v.Value.(caddyhttp.Route); ok {
			count++
			if count > 1 {
				break
			}
		}
	}
	if count < 2 {
		return
	}

	// now that we know the group will have some effect, do it
	groupName := h.groupCounter.nextGroup()
	for i := range vals {
		if route, ok := vals[i].Value.(caddyhttp.Route); ok {
			route.Group = groupName
			vals[i].Value = route
		}
	}
}

// WithDispenser returns a new instance based on d. All others Helper
// fields are copied, so typically maps are shared with this new instance.
func (h Helper) WithDispenser(d *caddyfile.Dispenser) Helper {
	h.Dispenser = d
	return h
}

// ParseSegmentAsSubroute parses the segment such that its subdirectives
// are themselves treated as directives, from which a subroute is built
// and returned.
//
// This is the entry point for nested handler scopes (route, handle,
// handle_errors, and handler modules such as reverse_proxy's
// handle_response). "order" constraints from the global options block
// apply only to each site's top-level chain, so nested scopes sort their
// directives by the registered baseline order, never by the adaptation's
// reordered one; an outer "order" can therefore never rearrange items
// within a nested handler.
func ParseSegmentAsSubroute(h Helper) (caddyhttp.MiddlewareHandler, error) {
	return parseSegmentAsSubrouteWithOrder(h, nestedDirectiveOrderFor(h.effectiveDirectiveOrder()))
}

// parseSegmentAsSubrouteWithOrder builds a subroute using the given
// directive order. Named route blocks, which are top-level handler chains
// rather than nested handler scopes, reuse the adaptation's order.
func parseSegmentAsSubrouteWithOrder(h Helper, order []string) (caddyhttp.MiddlewareHandler, error) {
	allResults, err := parseSegmentAsConfig(h)
	if err != nil {
		return nil, err
	}

	return buildSubroute(allResults, h.groupCounter, true, order)
}

// parseSegmentAsConfig parses the segment such that its subdirectives
// are themselves treated as directives, including named matcher definitions,
// and the raw Config structs are returned.
func parseSegmentAsConfig(h Helper) ([]ConfigValue, error) {
	var allResults []ConfigValue

	for h.Next() {
		// don't allow non-matcher args on the first line
		if h.NextArg() {
			return nil, h.ArgErr()
		}

		// slice the linear list of tokens into top-level segments
		var segments []caddyfile.Segment
		for nesting := h.Nesting(); h.NextBlock(nesting); {
			segments = append(segments, h.NextSegment())
		}

		// copy existing matcher definitions so we can augment
		// new ones that are defined only in this scope
		matcherDefs := make(map[string]caddy.ModuleMap, len(h.matcherDefs))
		maps.Copy(matcherDefs, h.matcherDefs)

		// find and extract any embedded matcher definitions in this scope
		for i := 0; i < len(segments); i++ {
			seg := segments[i]
			if strings.HasPrefix(seg.Directive(), matcherPrefix) {
				// parse, then add the matcher to matcherDefs
				err := parseMatcherDefinitions(caddyfile.NewDispenser(seg), matcherDefs)
				if err != nil {
					return nil, err
				}
				// remove the matcher segment (consumed), then step back the loop
				segments = append(segments[:i], segments[i+1:]...)
				i--
			}
		}

		// clone BlockState once for the entire block so sibling directives
		// can share state, but changes don't leak to the parent scope
		subBlockState := make(map[string]any, len(h.BlockState))
		maps.Copy(subBlockState, h.BlockState)

		// with matchers ready to go, evaluate each directive's segment
		for _, seg := range segments {
			dir := seg.Directive()
			dirFunc, ok := registeredDirectives[dir]
			if !ok {
				return nil, h.Errf("unrecognized directive: %s - are you sure your Caddyfile structure (nesting and braces) is correct?", dir)
			}

			subHelper := h
			subHelper.Dispenser = caddyfile.NewDispenser(seg)
			subHelper.matcherDefs = matcherDefs
			subHelper.BlockState = subBlockState

			results, err := dirFunc(subHelper)
			if err != nil {
				return nil, h.Errf("parsing caddyfile tokens for '%s': %v", dir, err)
			}

			dir = normalizeDirectiveName(dir)

			for _, result := range results {
				result.directive = dir
				allResults = append(allResults, result)
			}
		}
	}

	return allResults, nil
}

// ConfigValue represents a value to be added to the final
// configuration, or a value to be consulted when building
// the final configuration.
type ConfigValue struct {
	// The kind of value this is. As the config is
	// being built, the adapter will look in the
	// "pile" for values belonging to a certain
	// class when it is setting up a certain part
	// of the config. The associated value will be
	// type-asserted and placed accordingly.
	Class string

	// The value to be used when building the config.
	// Generally its type is associated with the
	// name of the Class.
	Value any

	directive string
}

func sortRoutes(routes []ConfigValue, order []string) {
	dirPositions := make(map[string]int)
	for i, dir := range order {
		dirPositions[dir] = i
	}

	sort.SliceStable(routes, func(i, j int) bool {
		// if the directives are different, just use the established directive order
		iDir, jDir := routes[i].directive, routes[j].directive
		if iDir != jDir {
			return dirPositions[iDir] < dirPositions[jDir]
		}

		// directives are the same; sub-sort by path matcher length if there's
		// only one matcher set and one path (this is a very common case and
		// usually -- but not always -- helpful/expected, oh well; user can
		// always take manual control of order using handler or route blocks)
		iRoute, ok := routes[i].Value.(caddyhttp.Route)
		if !ok {
			return false
		}
		jRoute, ok := routes[j].Value.(caddyhttp.Route)
		if !ok {
			return false
		}

		// decode the path matchers if there is just one matcher set
		var iPM, jPM caddyhttp.MatchPath
		if len(iRoute.MatcherSetsRaw) == 1 {
			_ = json.Unmarshal(iRoute.MatcherSetsRaw[0]["path"], &iPM)
		}
		if len(jRoute.MatcherSetsRaw) == 1 {
			_ = json.Unmarshal(jRoute.MatcherSetsRaw[0]["path"], &jPM)
		}

		// if there is only one path in the path matcher, sort by longer path
		// (more specific) first; missing path matchers or multi-matchers are
		// treated as zero-length paths
		var iPathLen, jPathLen int
		if len(iPM) == 1 {
			iPathLen = len(iPM[0])
		}
		if len(jPM) == 1 {
			jPathLen = len(jPM[0])
		}

		sortByPath := func() bool {
			// we can only confidently compare path lengths if both
			// directives have a single path to match (issue #5037)
			if iPathLen > 0 && jPathLen > 0 {
				// trim the trailing wildcard if there is one
				iPathTrimmed := strings.TrimSuffix(iPM[0], "*")
				jPathTrimmed := strings.TrimSuffix(jPM[0], "*")

				// if both paths are the same except for a trailing wildcard,
				// sort by the shorter path first (which is more specific)
				if iPathTrimmed == jPathTrimmed {
					return iPathLen < jPathLen
				}

				// we use the trimmed length to compare the paths
				// https://github.com/caddyserver/caddy/issues/7012#issuecomment-2870142195
				// credit to https://github.com/Hellio404
				// for sorts with many items, mixing matchers w/ and w/o wildcards will confuse the sort and result in incorrect orders
				iPathLen = len(iPathTrimmed)
				jPathLen = len(jPathTrimmed)

				// if both paths have the same length, sort lexically
				// https://github.com/caddyserver/caddy/pull/7015#issuecomment-2871993588
				if iPathLen == jPathLen {
					return iPathTrimmed < jPathTrimmed
				}

				// sort most-specific (longest) path first
				return iPathLen > jPathLen
			}

			// if both directives don't have a single path to compare,
			// sort whichever one has a matcher first; if both have
			// a matcher, sort equally (stable sort preserves order)
			return len(iRoute.MatcherSetsRaw) > 0 && len(jRoute.MatcherSetsRaw) == 0
		}()

		// some directives involve setting values which can overwrite
		// each other, so it makes most sense to reverse the order so
		// that the least-specific matcher is first, allowing the last
		// matching one to win
		if iDir == "vars" {
			return !sortByPath
		}

		// everything else is most-specific matcher first
		return sortByPath
	})
}

// serverBlock pairs a Caddyfile server block with
// a "pile" of config values, keyed by class name,
// as well as its parsed keys for convenience.
type serverBlock struct {
	block      caddyfile.ServerBlock
	pile       map[string][]ConfigValue // config values obtained from directives
	parsedKeys []Address
}

// hostsFromKeys returns a list of all the non-empty hostnames found in
// the keys of the server block sb. If logger mode is false, a key with
// an empty hostname portion will return an empty slice, since that
// server block is interpreted to effectively match all hosts. An empty
// string is never added to the slice.
//
// If loggerMode is true, then the non-standard ports of keys will be
// joined to the hostnames. This is to effectively match the Host
// header of requests that come in for that key.
//
// The resulting slice is not sorted but will never have duplicates.
func (sb serverBlock) hostsFromKeys(loggerMode bool) []string {
	// ensure each entry in our list is unique
	hostMap := make(map[string]struct{})
	for _, addr := range sb.parsedKeys {
		if addr.Host == "" {
			if !loggerMode {
				// server block contains a key like ":443", i.e. the host portion
				// is empty / catch-all, which means to match all hosts
				return []string{}
			}
			// never append an empty string
			continue
		}
		if loggerMode &&
			addr.Port != "" &&
			addr.Port != strconv.Itoa(caddyhttp.DefaultHTTPPort) &&
			addr.Port != strconv.Itoa(caddyhttp.DefaultHTTPSPort) {
			hostMap[net.JoinHostPort(addr.Host, addr.Port)] = struct{}{}
		} else {
			hostMap[addr.Host] = struct{}{}
		}
	}

	// convert map to slice
	sblockHosts := make([]string, 0, len(hostMap))
	for host := range hostMap {
		sblockHosts = append(sblockHosts, host)
	}

	return sblockHosts
}

func (sb serverBlock) hostsFromKeysNotHTTP(httpPort string) []string {
	// ensure each entry in our list is unique
	hostMap := make(map[string]struct{})
	for _, addr := range sb.parsedKeys {
		if addr.Host == "" {
			continue
		}
		if addr.Scheme != "http" && addr.Port != httpPort {
			hostMap[addr.Host] = struct{}{}
		}
	}

	// convert map to slice
	sblockHosts := make([]string, 0, len(hostMap))
	for host := range hostMap {
		sblockHosts = append(sblockHosts, host)
	}

	return sblockHosts
}

// hasHostCatchAllKey returns true if sb has a key that
// omits a host portion, i.e. it "catches all" hosts.
func (sb serverBlock) hasHostCatchAllKey() bool {
	return slices.ContainsFunc(sb.parsedKeys, func(addr Address) bool {
		return addr.Host == ""
	})
}

// isAllHTTP returns true if all sb keys explicitly specify
// the http:// scheme
func (sb serverBlock) isAllHTTP() bool {
	return !slices.ContainsFunc(sb.parsedKeys, func(addr Address) bool {
		return addr.Scheme != "http"
	})
}

// Positional are the supported modes for ordering directives.
type Positional string

const (
	Before Positional = "before"
	After  Positional = "after"
	First  Positional = "first"
	Last   Positional = "last"
)

type (
	// UnmarshalFunc is a function which can unmarshal Caddyfile
	// tokens into zero or more config values using a Helper type.
	// These are passed in a call to RegisterDirective.
	UnmarshalFunc func(h Helper) ([]ConfigValue, error)

	// UnmarshalHandlerFunc is like UnmarshalFunc, except the
	// output of the unmarshaling is an HTTP handler. This
	// function does not need to deal with HTTP request matching
	// which is abstracted away. Since writing HTTP handlers
	// with Caddyfile support is very common, this is a more
	// convenient way to add a handler to the chain since a lot
	// of the details common to HTTP handlers are taken care of
	// for you. These are passed to a call to
	// RegisterHandlerDirective.
	UnmarshalHandlerFunc func(h Helper) (caddyhttp.MiddlewareHandler, error)

	// UnmarshalGlobalFunc is a function which can unmarshal Caddyfile
	// tokens from a global option. It is passed the tokens to parse and
	// existing value from the previous instance of this global option
	// (if any). It returns the value to associate with this global option.
	UnmarshalGlobalFunc func(d *caddyfile.Dispenser, existingVal any) (any, error)
)

var registeredDirectives = make(map[string]UnmarshalFunc)

var registeredGlobalOptions = make(map[string]UnmarshalGlobalFunc)
