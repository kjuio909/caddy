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
	"fmt"
	"slices"
	"strings"
)

type adjacency map[string][]string

type importGraph struct {
	nodes map[string]struct{}
	edges adjacency
}

func (i *importGraph) addNode(name string) {
	if i.nodes == nil {
		i.nodes = make(map[string]struct{})
	}
	if _, exists := i.nodes[name]; exists {
		return
	}
	i.nodes[name] = struct{}{}
}

func (i *importGraph) addNodes(names []string) {
	for _, name := range names {
		i.addNode(name)
	}
}

func (i *importGraph) removeNode(name string) {
	delete(i.nodes, name)
}

func (i *importGraph) removeNodes(names []string) {
	for _, name := range names {
		i.removeNode(name)
	}
}

func (i *importGraph) addEdge(from, to string) error {
	if !i.exists(from) || !i.exists(to) {
		return fmt.Errorf("one of the nodes does not exist")
	}

	if path, ok := i.pathBetween(to, from); ok {
		// path runs from `to` back to `from`; appending `to`
		// renders the complete closed loop. Edges are recorded in
		// parse order and the search follows that order, so the same
		// input always reports the same cycle sequence.
		loop := append(append([]string{}, path...), to)
		return fmt.Errorf("a cycle of imports exists between %s and %s: %s",
			from, to, strings.Join(loop, " -> "))
	}

	if i.areConnected(from, to) {
		// if connected, there's nothing to do
		return nil
	}

	if i.nodes == nil {
		i.nodes = make(map[string]struct{})
	}
	if i.edges == nil {
		i.edges = make(adjacency)
	}

	i.edges[from] = append(i.edges[from], to)
	return nil
}

func (i *importGraph) addEdges(from string, tos []string) error {
	for _, to := range tos {
		err := i.addEdge(from, to)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *importGraph) areConnected(from, to string) bool {
	al, ok := i.edges[from]
	if !ok {
		return false
	}
	return slices.Contains(al, to)
}

// pathBetween searches the recorded edges (in parse order) for a path
// from start to target. The returned slice starts with start and ends
// with target. Edges are appended while parsing, never reordered, so
// the same input yields the same path across parses.
func (i *importGraph) pathBetween(start, target string) ([]string, bool) {
	var dfs func(string, []string) ([]string, bool)
	dfs = func(node string, trail []string) ([]string, bool) {
		trail = append(trail, node)
		if node == target {
			return trail, true
		}
		for _, next := range i.edges[node] {
			// the graph is acyclic at this point (a cycle is what
			// we are about to reject), so a trail guard is enough
			if slices.Contains(trail, next) {
				continue
			}
			if path, ok := dfs(next, trail); ok {
				return path, true
			}
		}
		return nil, false
	}
	return dfs(start, nil)
}

func (i *importGraph) exists(key string) bool {
	_, exists := i.nodes[key]
	return exists
}
