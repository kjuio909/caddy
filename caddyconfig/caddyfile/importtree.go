// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version, 2.0 (the "License");
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
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// errImportIsDir marks an import target that exists but is a directory.
var errImportIsDir = errors.New("is a directory")

// FileTree is a readable, in-memory snapshot of files that import
// directives can resolve while parsing. Keys identify files by their
// normalized, forward-slash paths (see normalizeTreePath); keys are
// normalized when the tree is queried, so callers do not have to
// normalize them ahead of time. The bytes of each entry are the
// complete file contents: parsing never reads imported files from
// anywhere else, so replacing files on disk between calls cannot
// affect the result.
//
// A nil FileTree means imports are resolved from the operating
// system's filesystem instead; this preserves the behavior of
// callers that do not provide a tree.
type FileTree map[string][]byte

// importSource reads files and expands import glob patterns for a
// single Parse call. Every implementation is self-contained: it
// must not share state with other Parse calls.
type importSource interface {
	// readFile returns the contents of the file at normalized path
	// name. A non-nil error means the target does not exist or is
	// otherwise unreadable (for example, it is a directory).
	readFile(name string) ([]byte, error)

	// globPattern returns the normalized paths of every file
	// matching pattern, in deterministic order. Directories are
	// not returned; they are reported as unreadable targets only
	// when a non-glob import names them directly.
	globPattern(pattern string) ([]string, error)
}

// openImportSource returns the configured tree, or a source backed
// by the operating system filesystem when no tree was given.
func openImportSource(tree FileTree) importSource {
	if tree != nil {
		return tree.normalize()
	}
	return diskSource{}
}

// normalizedTree is a FileTree whose keys have been normalized.
type normalizedTree map[string][]byte

func (t FileTree) normalize() normalizedTree {
	// copy every entry up front: the result is an immutable snapshot
	// for the whole parse, so reusing or mutating the caller's map
	// or backing arrays between (or during) Parse calls cannot mix
	// versions into a successful expansion
	nt := make(normalizedTree, len(t))
	for name, contents := range t {
		cp := make([]byte, len(contents))
		copy(cp, contents)
		nt[normalizeTreePath(name)] = cp
	}
	return nt
}

func (nt normalizedTree) readFile(name string) ([]byte, error) {
	key := normalizeTreePath(name)
	contents, ok := nt[key]
	if !ok {
		// importing a directory is an error, just like on disk
		for existing := range nt {
			if existing == key || strings.HasPrefix(existing, key+"/") {
				return nil, errImportIsDir
			}
		}
		return nil, os.ErrNotExist
	}
	// a copy guards against callers mutating the slice mid-parse
	out := make([]byte, len(contents))
	copy(out, contents)
	return out, nil
}

func (nt normalizedTree) globPattern(pattern string) ([]string, error) {
	pattern = normalizeTreePath(pattern)
	var matches []string
	for name := range nt {
		ok, err := path.Match(pattern, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// skip directory entries, which the map may contain if a
		// caller chose to register an explicit entry for a folder
		if strings.HasSuffix(name, "/") {
			continue
		}
		matches = append(matches, name)
	}
	sort.Strings(matches)
	return matches, nil
}

// diskSource resolves imports from the operating system filesystem.
// It holds no state of its own; each method call opens files
// afresh, which is why callers that need snapshot semantics should
// provide a FileTree instead.
type diskSource struct{}

func (diskSource) readFile(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("is a directory")
	}

	return io.ReadAll(file)
}

func (diskSource) globPattern(pattern string) ([]string, error) {
	return filepath.Glob(pattern)
}

var driveLetterPattern = regexp.MustCompile(`^([A-Za-z]):[\\/]`)

// normalizeTreePath canonicalizes a path used inside a FileTree:
// Windows drive letters and backslashes are rewritten to forward
// slashes, the path is cleaned lexically, and a single leading slash
// is dropped so every key is tree-relative. Normalization never
// consults the working directory.
func normalizeTreePath(name string) string {
	s := strings.ReplaceAll(name, "\\", "/")
	if m := driveLetterPattern.FindStringSubmatch(s); m != nil {
		s = m[1] + "/" + s[len(m[0]):]
	}
	s = path.Clean(s)
	s = strings.TrimPrefix(s, "/")
	if s == "." {
		return ""
	}
	return s
}

// isAbsolutePathPattern reports whether pattern names a location
// independently of the importing file's directory.
func isAbsolutePathPattern(pattern string) bool {
	if strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "\\") {
		return true
	}
	if driveLetterPattern.MatchString(pattern) {
		return true
	}
	return filepath.IsAbs(pattern)
}

// resolveImportPath joins a possibly-relative import pattern with
// the directory of the importing file, normalizes the result, and
// rejects targets that escape the root of the file tree.
func resolveImportPath(importingFile, pattern string) (string, error) {
	base := normalizeTreePath(importingFile)
	patternNorm := normalizeTreePath(pattern)
	if !isAbsolutePathPattern(pattern) {
		patternNorm = path.Join(path.Dir(base), patternNorm)
	}
	if patternNorm == ".." || strings.HasPrefix(patternNorm, "../") {
		return "", fmt.Errorf("import path %s resolves outside of the file tree", pattern)
	}
	return patternNorm, nil
}
