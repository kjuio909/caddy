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

// graphNode pairs a node's canonical identity with a human-readable
// label.
//
// The identity is used for every graph comparison: it is built from the
// real, symlink-resolved path, folded according to the filesystem's case
// rules, and namespaced by snippet name when applicable. The same
// physical file reached through aliases, ".." segments, or different
// case spellings therefore maps to one identity.
//
// The label is only used in diagnostics and keeps the path the way it was
// addressed (the root file stays "Caddyfile"; imported files carry their
// real path), so error chains are stable across machines and do not leak
// case folding. The literal pattern that closes a loop through an alias
// is reported separately by the parser ("while importing ...").
type graphNode struct {
	id    string
	label string
}

type importGraph struct {
	// labels maps a node identity to its display label.
	labels map[string]string
	edges  adjacency
}

func (i *importGraph) addNode(node graphNode) {
	if i.labels == nil {
		i.labels = make(map[string]string)
	}
	if _, exists := i.labels[node.id]; exists {
		return
	}
	i.labels[node.id] = node.label
}

func (i *importGraph) addNodes(nodes []graphNode) {
	for _, node := range nodes {
		i.addNode(node)
	}
}

// removeNode deletes a node introduced only for an edge that failed to be
// added. Nodes already known from an earlier edge are left untouched.
func (i *importGraph) removeNode(node graphNode) {
	if current, exists := i.labels[node.id]; !exists || current != node.label {
		return
	}
	delete(i.labels, node.id)
	delete(i.edges, node.id)
}

func (i *importGraph) removeNodes(nodes []graphNode) {
	for _, node := range nodes {
		i.removeNode(node)
	}
}

func (i *importGraph) addEdge(from, to graphNode) error {
	if !i.exists(from.id) || !i.exists(to.id) {
		return fmt.Errorf("one of the nodes does not exist")
	}

	// A cycle is formed if `to` can already reach `from`; adding the edge
	// would close the loop. Surface the full chain of files involved so the
	// caller can diagnose the cycle rather than its depth.
	if cycle := i.findPath(to.id, from.id); cycle != nil {
		chain := append(cycle, to.id)
		return fmt.Errorf("import cycle detected: %s", strings.Join(i.labelsOf(chain), " -> "))
	}

	if i.areConnected(from.id, to.id) {
		// if connected, there's nothing to do
		return nil
	}

	if i.edges == nil {
		i.edges = make(adjacency)
	}

	i.edges[from.id] = append(i.edges[from.id], to.id)
	return nil
}

func (i *importGraph) addEdges(from graphNode, tos []graphNode) error {
	for _, to := range tos {
		if err := i.addEdge(from, to); err != nil {
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
// returning the chain of node identities (including both ends). It
// returns nil when the two nodes are not connected. Edge traversal order
// follows edge insertion order so the reported chain is stable across
// repeated runs.
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

// labelsOf maps node identities to their display labels, falling back to
// the identity itself when a label is unavailable.
func (i *importGraph) labelsOf(ids []string) []string {
	labels := make([]string, len(ids))
	for idx, id := range ids {
		if label, ok := i.labels[id]; ok {
			labels[idx] = label
		} else {
			labels[idx] = id
		}
	}
	return labels
}

func (i *importGraph) exists(key string) bool {
	_, exists := i.labels[key]
	return exists
}
