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

	// A cycle is formed if `to` can already reach `from`; adding the edge
	// would close the loop. Surface the full chain of files involved so the
	// caller can diagnose the cycle rather than its depth. The closing
	// endpoints are also named with the established wording so existing
	// diagnostics (e.g. snippet cycles reported against "Caddyfile") keep
	// matching.
	if cycle := i.findPath(to, from); cycle != nil {
		chain := strings.Join(append(cycle, to), " -> ")
		return fmt.Errorf(
			"import cycle detected: %s; a cycle of imports exists between %s and %s",
			chain, from, to)
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

// findPath searches for any path through the graph from start to target,
// returning the chain of node names (including both ends). It returns nil
// when the two nodes are not connected. Edge traversal order follows edge
// insertion order so the reported chain is stable across repeated runs.
func (i *importGraph) findPath(start, target string) []string {
	if start == target {
		return []string{start}
	}

	parent := map[string]string{start: ""}
	queue := []string{start}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, next := range i.edges[current] {
			if _, visited := parent[next]; visited {
				continue
			}
			parent[next] = current
			if next == target {
				// reconstruct the chain from target back to start
				chain := []string{target}
				for node := target; parent[node] != ""; node = parent[node] {
					chain = append(chain, parent[node])
				}
				slices.Reverse(chain)
				return chain
			}
			queue = append(queue, next)
		}
	}

	return nil
}

func (i *importGraph) exists(key string) bool {
	_, exists := i.nodes[key]
	return exists
}
