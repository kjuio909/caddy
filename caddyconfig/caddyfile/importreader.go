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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ImportFileReader reads files named by import directives during a
// single Parse call. The name argument is always a normalized path:
// backslashes are treated as separators, "." and ".." segments are
// resolved lexically, and the path never depends on the process
// working directory. ReadFile returns the complete file contents; a
// non-nil error means the target does not exist or is otherwise
// unreadable (for example, it is a directory).
//
// Glob returns the normalized paths of every file matching pattern,
// in deterministic (lexicographic) order. It must return
// filepath.ErrBadPattern for a malformed pattern so the whole parse
// fails rather than silently importing nothing.
//
// Both methods are called from a single Parse invocation only; an
// implementation never has to serve concurrent parses. Parse itself
// keeps no state between calls, so each invocation receives a fresh
// reader and arguments, caches, and errors from one parse can never
// be observed by another.
type ImportFileReader interface {
	ReadFile(name string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

// ImportFiles is an ImportFileReader backed by an in-memory snapshot
// of file contents. Keys identify files by their normalized,
// forward-slash paths (see NormalizeImportPath); keys are normalized
// when the map is queried, so callers do not have to normalize them
// ahead of time. The bytes of each entry are the complete file
// contents: parsing never reads imported files from anywhere else, so
// replacing files on disk between calls cannot affect the result.
type ImportFiles map[string][]byte

// ReadFile implements ImportFileReader.
func (f ImportFiles) ReadFile(name string) ([]byte, error) {
	key := normalizeImportPath(name)
	contents, ok := f[key]
	if !ok {
		// importing a directory is an error, just like on disk
		for existing := range f {
			norm := normalizeImportPath(existing)
			if norm == key || strings.HasPrefix(norm, key+"/") {
				return nil, errImportIsDir
			}
		}
		return nil, os.ErrNotExist
	}
	// return a copy so callers can never mutate the stored snapshot
	out := make([]byte, len(contents))
	copy(out, contents)
	return out, nil
}

// Glob implements ImportFileReader. Matches are sorted by normalized
// path for a deterministic expansion order.
func (f ImportFiles) Glob(pattern string) ([]string, error) {
	pattern = normalizeImportPath(pattern)
	var matches []string
	for name := range f {
		normalized := normalizeImportPath(name)
		// skip entries that stand for directories rather than files
		if strings.HasSuffix(normalized, "/") {
			continue
		}
		ok, err := path.Match(pattern, normalized)
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, normalized)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// FileTree is an alias of ImportFiles, kept as a convenience for
// callers that build their imported configuration as a tree of files
// keyed by normalized path.
type FileTree = ImportFiles

// ParseOption customizes a Parse call.
type ParseOption func(*parseConfig)

// parseConfig holds the options applied to a single Parse call.
type parseConfig struct {
	importReader ImportFileReader
}

// WithImportFileReader provides the reader used to resolve every
// import directive, including glob patterns. An ImportFiles value
// can be passed directly. When a reader is given, Parse never
// consults the operating system filesystem, which makes the result
// independent of the working directory and stable across calls.
func WithImportFileReader(reader ImportFileReader) ParseOption {
	return func(cfg *parseConfig) {
		cfg.importReader = reader
	}
}

// errImportIsDir marks an import target that exists but is a directory.
var errImportIsDir = errors.New("is a directory")

// importSource reads files and expands import glob patterns for a
// single Parse call. Every implementation is self-contained: it never
// shares state with other Parse calls.
type importSource interface {
	// readFile returns the contents of the file at normalized path
	// name. A non-nil error means the target does not exist or is
	// otherwise unreadable (for example, it is a directory).
	readFile(name string) ([]byte, error)

	// globPattern returns the normalized paths of every file
	// matching pattern, in deterministic order. Directories are not
	// returned; they are reported as unreadable targets only when a
	// non-glob import names them directly.
	globPattern(pattern string) ([]string, error)
}

// snapshotTree is an ImportFiles whose keys have been normalized and
// whose contents have been copied, forming an immutable snapshot for
// the whole parse: reusing or mutating the caller's map or backing
// arrays between (or during) Parse calls cannot mix versions into a
// successful expansion.
type snapshotTree map[string][]byte

// openImportSource returns a snapshot of files passed as a map (so
// later mutation by the caller cannot affect the parse), a reader
// built directly from any other ImportFileReader, or a source backed
// by the operating system filesystem when no reader was configured.
func openImportSource(reader ImportFileReader) importSource {
	switch r := reader.(type) {
	case nil:
		return diskSource{}
	case ImportFiles:
		return r.snapshot()
	default:
		return readerSource{reader: r}
	}
}

func (f ImportFiles) snapshot() snapshotTree {
	snap := make(snapshotTree, len(f))
	for name, contents := range f {
		cp := make([]byte, len(contents))
		copy(cp, contents)
		snap[normalizeImportPath(name)] = cp
	}
	return snap
}

func (s snapshotTree) readFile(name string) ([]byte, error) {
	key := normalizeImportPath(name)
	contents, ok := s[key]
	if !ok {
		// importing a directory is an error, just like on disk
		for existing := range s {
			if existing == key || strings.HasPrefix(existing, key+"/") {
				return nil, errImportIsDir
			}
		}
		return nil, os.ErrNotExist
	}
	out := make([]byte, len(contents))
	copy(out, contents)
	return out, nil
}

func (s snapshotTree) globPattern(pattern string) ([]string, error) {
	pattern = normalizeImportPath(pattern)
	var matches []string
	for name := range s {
		if strings.HasSuffix(name, "/") {
			continue
		}
		ok, err := path.Match(pattern, name)
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// readerSource adapts a caller-provided ImportFileReader to the
// internal importSource. Results are not cached: each import is read
// afresh, so a reader whose backing data changes between calls cannot
// leak one call's contents into another.
type readerSource struct {
	reader ImportFileReader
}

func (s readerSource) readFile(name string) ([]byte, error) {
	return s.reader.ReadFile(normalizeImportPath(name))
}

func (s readerSource) globPattern(pattern string) ([]string, error) {
	matches, err := s.reader.Glob(normalizeImportPath(pattern))
	if err != nil {
		return nil, err
	}
	normalized := make([]string, len(matches))
	for i, m := range matches {
		normalized[i] = normalizeImportPath(m)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// diskSource resolves imports from the operating system filesystem.
// It holds no state of its own; each method call opens files afresh.
type diskSource struct{}

func (diskSource) readFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (diskSource) globPattern(pattern string) ([]string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	// filepath.Glob already sorts matches, but normalize the order
	// explicitly so the expansion sequence never depends on platform
	// quirks
	sort.Strings(matches)
	return matches, nil
}

var driveLetterPattern = regexp.MustCompile(`^([A-Za-z]):[\\/]`)

// NormalizeImportPath canonicalizes a path used to identify imported
// contents: Windows drive letters and backslashes are rewritten to
// forward slashes, the path is cleaned lexically (resolving every "."
// and ".." segment), and a single leading slash is dropped so every
// path is reader-relative. Normalization never consults the working
// directory.
func NormalizeImportPath(name string) string {
	return normalizeImportPath(name)
}

func normalizeImportPath(name string) string {
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

// resolveImportPath joins a possibly-relative import pattern with the
// directory of the importing file, normalizes the result, and rejects
// targets that escape the root of the provided files.
func resolveImportPath(importingFile, pattern string) (string, error) {
	base := normalizeImportPath(importingFile)
	patternNorm := normalizeImportPath(pattern)
	if !isAbsolutePathPattern(pattern) {
		patternNorm = path.Join(path.Dir(base), patternNorm)
	}
	if patternNorm == ".." || strings.HasPrefix(patternNorm, "../") {
		return "", fmt.Errorf("import path %s resolves outside of the file tree", pattern)
	}
	return patternNorm, nil
}

// fsImportReader adapts an fs.FS to the ImportFileReader interface.
// Prefer providing ImportFiles directly when the contents are already
// in memory.
type fsImportReader struct {
	fsys fs.FS
}

// NewImportFileReader returns an ImportFileReader rooted at fsys.
// Glob results are sorted by normalized path for deterministic
// expansion.
func NewImportFileReader(fsys fs.FS) ImportFileReader {
	return fsImportReader{fsys: fsys}
}

func (r fsImportReader) ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(r.fsys, normalizeImportPath(name))
}

func (r fsImportReader) Glob(pattern string) ([]string, error) {
	matches, err := fs.Glob(r.fsys, normalizeImportPath(pattern))
	if err != nil {
		return nil, err
	}
	normalized := make([]string, len(matches))
	for i, m := range matches {
		normalized[i] = normalizeImportPath(m)
	}
	sort.Strings(normalized)
	return normalized, nil
}
