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
// immutable after package initialization: the "order" global option produces
// a per-adaptation order (stored in the adaptation's options and Helper)
// instead of mutating this package variable, so concurrent adaptations never
// share or leak directive ordering.
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
	// directiveOrder is the directive ordering in effect for this adaptation.
	// It starts from the package baseline and is adjusted, per adaptation, by
	// the "order" global option. It is never shared across adaptations.
	directiveOrder []string
}

// resolveAdaptationDirectiveOrder merges the "order" constraints collected
// for an adaptation with the package baseline directive order and returns
// the resulting total order. It is called once, before any site is parsed,
// so contradictory or cyclic constraints fail the whole adaptation with no
// partial JSON instead of affecting some sites and not others.
func resolveAdaptationDirectiveOrder(options map[string]any) ([]string, error) {
	if constraints, ok := options["order"].([]directiveOrderConstraint); ok && len(constraints) > 0 {
		return resolveDirectiveOrder(directiveOrder, constraints)
	}
	return directiveOrder, nil
}

// effectiveDirectiveOrder returns the directive order for the current
// adaptation, falling back to the registered package baseline when the
// adaptation did not carry its own order. It is only consulted when a
// site's top-level processing chain is assembled; nested scopes keep their
// own local order instead.
func (h Helper) effectiveDirectiveOrder() []string {
	if len(h.directiveOrder) > 0 {
		return h.directiveOrder
	}
	return directiveOrder
}

// resolveDirectiveOrder produces a stable total ordering of the baseline
// directives satisfying the given constraints.
//
// Constraints are treated as a directed graph: "a before b" adds edge a->b,
// "a after b" adds edge b->a, and the baseline order contributes an edge
// between every adjacent pair of unrelated directives, so unconstrained
// directives keep their default relative order. First/Last anchors are
// applied deterministically: subjects anchored first sort before every
// other directive, those anchored last after every other directive, and
// ties within each anchor group keep the baseline order. A cycle in the
// constraint graph is reported as an error naming the directives involved,
// together with the location of a constraint that participates in the cycle.
func resolveDirectiveOrder(baseline []string, constraints []directiveOrderConstraint) ([]string, error) {
	// Every directive named by a constraint participates in the graph,
	// including directives registered without a default position (e.g.
	// plugin directives the user is ordering for the first time). Baseline
	// directives come first in their default order; extras are appended
	// sorted by name so their tiebreak rank is independent of the order in
	// which the constraint lines were written.
	known := make(map[string]struct{}, len(baseline))
	for _, dir := range baseline {
		known[dir] = struct{}{}
	}
	var extras []string
	seenExtra := make(map[string]struct{})
	addKnown := func(dir string) {
		if _, ok := known[dir]; ok {
			return
		}
		if _, ok := seenExtra[dir]; ok {
			return
		}
		seenExtra[dir] = struct{}{}
		extras = append(extras, dir)
	}
	for _, c := range constraints {
		addKnown(c.subject)
		if c.anchor != "" {
			addKnown(c.anchor)
		}
	}
	sort.Strings(extras)

	// allDirectives is the baseline followed by the name-sorted extras.
	allDirectives := append(append([]string{}, baseline...), extras...)

	// ranks give every directive a stable tiebreak position.
	ranks := make(map[string]int, len(allDirectives))
	for i, dir := range allDirectives {
		ranks[dir] = i
	}

	// anchors group directives into first / unanchored / last while keeping
	// baseline order within each group.
	var first, last, middle []string
	firstSet := make(map[string]struct{})
	lastSet := make(map[string]struct{})
	for _, dir := range allDirectives {
		switch {
		case isAnchored(constraints, dir, First):
			first = append(first, dir)
			firstSet[dir] = struct{}{}
		case isAnchored(constraints, dir, Last):
			last = append(last, dir)
			lastSet[dir] = struct{}{}
		default:
			middle = append(middle, dir)
		}
	}

	// nodes traverses the groups in first/middle/last order.
	nodes := append(append(append([]string{}, first...), middle...), last...)
	indegree := make(map[string]int, len(nodes))
	successors := make(map[string]map[string]struct{}, len(nodes))
	addEdge := func(from, to string) {
		if from == to {
			return
		}
		if successors[from] == nil {
			successors[from] = make(map[string]struct{})
		}
		if _, ok := successors[from][to]; !ok {
			successors[from][to] = struct{}{}
			indegree[to]++
		}
	}

	// A directive relocated by a Before/After constraint is taken out of
	// the baseline chain: the chain reconnects around it, so the
	// declaration moves it instead of contradicting its default position.
	// The rank-based tiebreak below then lands it as close to its anchor
	// as the constraints allow, matching "insert next to the anchor"
	// semantics while being independent of declaration order.
	relocated := make(map[string]struct{})
	for _, c := range constraints {
		if c.position == Before || c.position == After {
			relocated[c.subject] = struct{}{}
		}
	}

	// The baseline contributes edges between consecutive directives that
	// both stay in place, but only within one anchor group; the anchors
	// themselves enforce cross-group ordering below.
	inSameGroup := func(a, b string) bool {
		_, aFirst := firstSet[a]
		_, bFirst := firstSet[b]
		_, aLast := lastSet[a]
		_, bLast := lastSet[b]
		return aFirst == bFirst && aLast == bLast
	}
	prevKept := ""
	for _, dir := range allDirectives {
		if _, isRelocated := relocated[dir]; isRelocated {
			continue
		}
		if prevKept != "" && inSameGroup(prevKept, dir) {
			addEdge(prevKept, dir)
		}
		prevKept = dir
	}

	// Every first-anchored directive precedes every non-first directive,
	// and every non-last directive precedes every last-anchored directive.
	// These edges fully realize the anchors, including against plugin
	// directives that have no baseline position.
	for _, f := range first {
		for _, n := range nodes {
			if _, isFirst := firstSet[n]; !isFirst {
				addEdge(f, n)
			}
		}
	}
	for _, l := range last {
		for _, n := range nodes {
			if _, isLast := lastSet[n]; !isLast {
				addEdge(n, l)
			}
		}
	}

	// user constraints; remember one declaration per edge so a cycle can be
	// reported at a location the user can fix. Duplicate declarations keep
	// the deterministically earliest location so line order never changes
	// the reported position.
	edgeOrigin := make(map[[2]string]directiveOrderConstraint)
	for _, c := range constraints {
		if c.position != Before && c.position != After {
			continue
		}
		from, to := c.subject, c.anchor
		if c.position == After {
			from, to = c.anchor, c.subject
		}
		key := [2]string{from, to}
		if existing, ok := edgeOrigin[key]; !ok || c.file < existing.file || (c.file == existing.file && c.line < existing.line) {
			edgeOrigin[key] = c
		}
		addEdge(from, to)
	}

	// Desired positions are used only as a deterministic tiebreak among
	// directives the graph leaves mutually unordered. Directives that stay
	// in place keep their integer baseline position; a relocated directive
	// is parked half a slot beside its anchor (chains of Before/After
	// constraints land on fractions of fractions), so a plugin directive
	// ordered relative to a standard one is positioned next to it rather
	// than at the end of the list.
	//
	// The positions are solved as a fixpoint with Jacobi-style iterations
	// (each pass reads the previous pass's positions), so the result is a
	// function of the graph alone: swapping declaration lines or changing
	// import traversal order cannot change it. The constraint graph being
	// acyclic guarantees feasibility; len(nodes) passes outrun the longest
	// dependency chain.
	intKeys := make(map[string]float64, len(allDirectives))
	for i, dir := range allDirectives {
		intKeys[dir] = float64(i)
	}
	// Anchored directives live outside the baseline range: first-anchored
	// directives occupy negative positions in their baseline order and
	// last-anchored directives occupy positions past the end, so a
	// directive parked beside an anchored one clusters with it rather than
	// beside an unrelated default directive.
	for i, dir := range first {
		intKeys[dir] = float64(i) - float64(len(first)) - 1
	}
	for i, dir := range last {
		intKeys[dir] = float64(len(allDirectives)) + float64(i) + 1
	}
	// parkedSlot is how far beside its anchor a relocated directive is
	// parked. Smaller than a baseline slot lets short chains of
	// constraints (e.g. "x after vars" then "y after x") cluster next to
	// the anchor instead of interleaving with neighboring default
	// directives.
	const parkedSlot = 0.25
	prev := intKeys
	for pass := 0; pass < len(nodes); pass++ {
		next := maps.Clone(intKeys)
		for subject := range relocated {
			var lo, hi float64
			hasLo, hasHi := false, false
			for _, c := range constraints {
				if c.subject != subject {
					continue
				}
				switch c.position {
				case After:
					if v := prev[c.anchor] + parkedSlot; !hasLo || v > lo {
						lo = v
						hasLo = true
					}
				case Before:
					if v := prev[c.anchor] - parkedSlot; !hasHi || v < hi {
						hi = v
						hasHi = true
					}
				}
			}
			switch {
			case hasLo && hasHi:
				next[subject] = (lo + hi) / 2
			case hasLo:
				next[subject] = lo
			case hasHi:
				next[subject] = hi
			}
		}
		prev = next
	}
	keys := prev

	// Kahn's algorithm. Among ready directives, the one with the smallest
	// desired position is emitted; exact key ties fall back to baseline
	// rank. Both are pure functions of the graph and the baseline, so the
	// output is identical for equivalent inputs regardless of declaration,
	// site-body or import order.
	ready := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if indegree[n] == 0 {
			ready = append(ready, n)
		}
	}
	less := func(a, b string) bool {
		if keys[a] != keys[b] {
			return keys[a] < keys[b]
		}
		return ranks[a] < ranks[b]
	}

	var ordered []string
	for len(ready) > 0 {
		// pick the ready node with the smallest baseline rank
		next := ready[0]
		nextIdx := 0
		for i := 1; i < len(ready); i++ {
			if less(ready[i], next) {
				next = ready[i]
				nextIdx = i
			}
		}
		ready = append(ready[:nextIdx], ready[nextIdx+1:]...)
		ordered = append(ordered, next)

		succs := make([]string, 0, len(successors[next]))
		for s := range successors[next] {
			succs = append(succs, s)
		}
		sort.Slice(succs, func(i, j int) bool { return less(succs[i], succs[j]) })
		for _, s := range succs {
			indegree[s]--
			if indegree[s] == 0 {
				ready = append(ready, s)
			}
		}
	}

	if len(ordered) != len(nodes) {
		return nil, cyclicOrderError(nodes, indegree, successors, edgeOrigin)
	}

	return ordered, nil
}

// isAnchored reports whether any constraint anchors dir in the given
// First/Last position.
func isAnchored(constraints []directiveOrderConstraint, dir string, pos Positional) bool {
	for _, c := range constraints {
		if c.subject == dir && c.position == pos {
			return true
		}
	}
	return false
}

// cyclicOrderError builds a locatable error for a cyclic ordering by
// extracting one directed cycle from the still-unresolved nodes.
func cyclicOrderError(
	nodes []string,
	indegree map[string]int,
	successors map[string]map[string]struct{},
	edgeOrigin map[[2]string]directiveOrderConstraint,
) error {
	// nodes with a remaining edge participate in, or lead into, a cycle.
	// Prune sinks, then sources, within that subgraph until it stabilizes:
	// what remains is exactly the set of nodes that sit on cycles.
	onCycle := make(map[string]struct{})
	for _, n := range nodes {
		if indegree[n] > 0 {
			onCycle[n] = struct{}{}
		}
	}
	for {
		changed := false
		for n := range onCycle {
			hasOut := false
			for s := range successors[n] {
				if _, ok := onCycle[s]; ok {
					hasOut = true
					break
				}
			}
			if !hasOut {
				delete(onCycle, n)
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for {
		changed := false
		for n := range onCycle {
			hasIn := false
			for _, m := range nodes {
				if _, ok := onCycle[m]; !ok {
					continue
				}
				if _, ok := successors[m][n]; ok {
					hasIn = true
					break
				}
			}
			if !hasIn {
				delete(onCycle, n)
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// start from the lexicographically smallest node on a cycle for a stable
	// message, then follow the smallest successor that stays in the set.
	start := ""
	for n := range onCycle {
		if start == "" || n < start {
			start = n
		}
	}
	var cycle []string
	visited := make(map[string]int) // node -> index in cycle
	cur := start
	for {
		if idx, ok := visited[cur]; ok {
			cycle = cycle[idx:]
			break
		}
		visited[cur] = len(cycle)
		cycle = append(cycle, cur)

		next := ""
		for s := range successors[cur] {
			if _, ok := onCycle[s]; !ok {
				continue
			}
			if next == "" || s < next {
				next = s
			}
		}
		if next == "" {
			break
		}
		cur = next
	}

	// find a user-declared edge on the cycle so the error points at source
	var origin *directiveOrderConstraint
	for i := range cycle {
		edge := [2]string{cycle[i], cycle[(i+1)%len(cycle)]}
		if c, ok := edgeOrigin[edge]; ok {
			c := c
			origin = &c
			break
		}
	}

	chain := strings.Join(append(cycle, cycle[0]), " -> ")
	if origin != nil {
		return fmt.Errorf("%s:%d: cyclic directive order constraint: %s",
			origin.file, origin.line, chain)
	}
	return fmt.Errorf("cyclic directive order constraint: %s", chain)
}

// Option gets the option keyed by name.
func (h Helper) Option(name string) any {
	return h.options[name]
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
// The subdirectives keep the local order in which they were written across
// different directives: the global "order" option only arranges each
// site's top-level processing chain and never reaches into a nested scope.
// Sibling routes of the same directive (typically handle) are still
// ordered by matcher specificity. Every directive must be a recognized
// ordered HTTP handler.
func ParseSegmentAsSubroute(h Helper) (caddyhttp.MiddlewareHandler, error) {
	allResults, err := parseSegmentAsConfig(h)
	if err != nil {
		return nil, err
	}

	return buildSubroute(allResults, h.groupCounter, sortNestedKeepLocal, true, h.effectiveDirectiveOrder())
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

		return sameDirectiveRoutesLess(iDir, routes[i], routes[j])
	})
}

// sortNestedRoutes arranges routes inside a nested scope (handle and its
// children). Different directives keep their written order; routes of the
// same directive are still ordered by matcher specificity, so e.g. a
// handle matching "/en/*" is evaluated before a catch-all handle even when
// the catch-all was written first. The global "order" option never reaches
// here: it only arranges a site's top-level chain.
func sortNestedRoutes(routes []ConfigValue) {
	sort.SliceStable(routes, func(i, j int) bool {
		iDir, jDir := routes[i].directive, routes[j].directive
		if iDir != jDir {
			return false
		}
		return sameDirectiveRoutesLess(iDir, routes[i], routes[j])
	})
}

// sameDirectiveRoutesLess reports whether route i should sort before route
// j when both were produced by the same directive.
func sameDirectiveRoutesLess(iDir string, iVal, jVal ConfigValue) bool {
	// sub-sort by path matcher length if there's
	// only one matcher set and one path (this is a very common case and
	// usually -- but not always -- helpful/expected, oh well; user can
	// always take manual control of order using handler or route blocks)
	iRoute, ok := iVal.Value.(caddyhttp.Route)
	if !ok {
		return false
	}
	jRoute, ok := jVal.Value.(caddyhttp.Route)
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
