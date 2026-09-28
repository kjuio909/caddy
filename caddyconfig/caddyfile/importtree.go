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
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// errImportIsDir marks an import target that exists but is a directory.
var errImportIsDir = errors.New("is a directory")

// FileReaderFunc is a function that opens an imported file by name.
// It is the functional form of FileReaders: a plain function can be
// passed straight to Parse without any wrapper type.
//
// Implementations are used through FileReaders.Open semantics: the
// returned reader is always fully consumed and closed by the parser,
// and names are normalized, tree-relative paths (see
// normalizeImportPath).
type FileReaderFunc func(name string) (io.ReadCloser, error)

// Open implements FileReaders.
func (f FileReaderFunc) Open(name string) (io.ReadCloser, error) {
	return f(name)
}

// FileReaders abstracts the files an import directive may resolve.
//
// Parsing only ever reads imported files through the value given to
// one Parse call: a single parse never consults the operating system
// behind the caller's back, and replacing or reusing readers between
// (or during) calls cannot mix versions into a result.
//
// A plain open function (FileReaderFunc) is enough for exact, named
// imports. Use FromFS to adapt an io/fs.FS (which also carries Glob
// and ReadDir) when import directives use wildcard patterns. A
// FileReaders value that additionally implements Glob(pattern)
// ([]string, error) provides glob matches directly.
type FileReaders interface {
	Open(name string) (io.ReadCloser, error)
}

// fileGlobber is the optional glob capability a FileReaders value may
// provide. Matches must use the same normalized, tree-relative path
// space as Open.
type fileGlobber interface {
	Glob(pattern string) ([]string, error)
}

// FromFS adapts an io/fs.FS to FileReaders. Because io/fs declares
// Open with a different return type, an fs.FS does not satisfy
// FileReaders structurally; FromFS performs the adaptation. When the
// underlying FS supports it, the returned value also resolves import
// globs (fs.GlobFS or directory listing); otherwise glob patterns
// simply have no matches.
func FromFS(fsys fs.FS) FileReaders {
	return fsReader{fsys: fsys}
}

// fsReader adapts an io/fs.FS.
type fsReader struct {
	fsys fs.FS
}

// Open implements FileReaders: an fs.File always provides Read and
// Close, so it doubles as an io.ReadCloser.
func (f fsReader) Open(name string) (io.ReadCloser, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Glob enumerates glob matches when the underlying FS can.
func (f fsReader) Glob(pattern string) ([]string, error) {
	if g, ok := f.fsys.(fs.GlobFS); ok {
		return g.Glob(pattern)
	}
	// fs.Glob needs directory listing; an FS that cannot list simply
	// yields no matches rather than an error.
	if _, ok := f.fsys.(fs.ReadDirFS); !ok {
		return nil, nil
	}
	return fs.Glob(f.fsys, pattern)
}

var _ FileReaders = FileReaderFunc(nil)
var _ FileReaders = fsReader{}

// importSource is one Parse call's self-contained view of importable
// files. It holds the caller's readers and caches every read and glob
// result for the duration of the call, so:
//
//   - each imported file is opened at most once, and every later use
//     (a repeated import, another glob hit, a nested import) expands
//     the same bytes;
//   - a reader that returns different contents on a second open
//     cannot change tokens already produced by the call;
//   - concurrent Parse calls have separate importSource values, so
//     readers may be reused freely without shared state.
//
// A nil readers value selects the operating system filesystem and the
// historical import behavior.
type importSource struct {
	readers FileReaders

	// files caches the snapshot bytes of every opened file by its
	// normalized path.
	files map[string][]byte

	// globs caches every resolved match list by normalized pattern so
	// the same pattern observed twice in a call expands identically.
	globs map[string][]string
}

// newImportSource returns a fresh source for one Parse call. Nothing
// is read eagerly; the first import drives the snapshot.
func newImportSource(readers FileReaders) *importSource {
	return &importSource{
		readers: readers,
		files:   make(map[string][]byte),
		globs:   make(map[string][]string),
	}
}

// onDisk reports whether imports resolve against the operating system
// filesystem, which keeps historical path and error behavior.
func (s *importSource) onDisk() bool { return s.readers == nil }

// readFile returns the contents of name, which is a normalized path
// (virtual) or an on-disk path. The first successful read is cached and
// copied, so callers cannot mutate the snapshot and a reused reader
// cannot smuggle new bytes into the call.
func (s *importSource) readFile(name string) ([]byte, error) {
	if cached, ok := s.files[name]; ok {
		out := make([]byte, len(cached))
		copy(out, cached)
		return out, nil
	}

	var input []byte
	if s.readers == nil {
		data, err := readDiskFile(name)
		if err != nil {
			return nil, err
		}
		input = data
	} else {
		rc, err := s.readers.Open(name)
		if err != nil {
			return nil, err
		}
		data, readErr := readAllAndClose(rc)
		if readErr != nil {
			return nil, readErr
		}
		input = data
	}

	// cache a private copy of the snapshot bytes
	cached := make([]byte, len(input))
	copy(cached, input)
	s.files[name] = cached
	return input, nil
}

// readAllAndClose drains f, treating a directory opened through any
// reader the same way os.File reports one, and always closing it.
func readAllAndClose(f io.ReadCloser) ([]byte, error) {
	defer f.Close()
	// Stat is optional: os.File and map-backed files provide it, but a
	// bare io.ReadCloser does not. A directory that cannot be statted
	// still fails when read.
	if st, ok := f.(interface{ Stat() (fs.FileInfo, error) }); ok {
		if info, err := st.Stat(); err == nil && info.IsDir() {
			return nil, errImportIsDir
		}
	}
	return io.ReadAll(f)
}

// readDiskFile opens, stats, and reads an on-disk import target.
func readDiskFile(name string) ([]byte, error) {
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
		return nil, errImportIsDir
	}
	return io.ReadAll(file)
}

// glob enumerates matches for an already-resolved, normalized (or
// on-disk) pattern. Results are cached per call and, for virtual
// readers, deterministically sorted.
func (s *importSource) glob(pattern string) ([]string, error) {
	if matches, ok := s.globs[pattern]; ok {
		return matches, nil
	}

	var matches []string
	var err error
	if s.readers == nil {
		matches, err = filepath.Glob(pattern)
	} else {
		matches, err = globWithReader(s.readers, pattern)
	}
	if err != nil {
		return nil, err
	}

	// normalize, deduplicate, and sort virtual matches so expansion
	// order is stable regardless of the reader's enumeration order or
	// path spelling; filepath.Glob already returns clean, sorted names
	if s.readers != nil {
		seen := make(map[string]struct{}, len(matches))
		normalized := make([]string, 0, len(matches))
		for _, m := range matches {
			n := normalizeImportPath(m)
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			normalized = append(normalized, n)
		}
		sort.Strings(normalized)
		matches = normalized
	}

	// cache a private copy of the name list
	cached := make([]string, len(matches))
	copy(cached, matches)
	s.globs[pattern] = cached
	return matches, nil
}

// globWithReader enumerates matches using whichever optional glob
// capability the readers value exposes. An open-only reader cannot be
// enumerated, so a pattern has no matches there.
func globWithReader(readers FileReaders, pattern string) ([]string, error) {
	if g, ok := readers.(fileGlobber); ok {
		return g.Glob(pattern)
	}
	return nil, nil
}

var driveLetterPattern = regexp.MustCompile(`^([A-Za-z]):[\\/]`)

// normalizeImportPath canonicalizes a path used with FileReaders:
// Windows drive letters and backslashes are rewritten to forward
// slashes, the path is cleaned lexically, and a single leading slash is
// dropped so every name is tree-relative. Normalization never consults
// the working directory.
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

// resolveVirtualImport joins a possibly-relative import pattern with
// the directory of the importing virtual file, normalizes the result,
// and rejects targets that escape the root of the file tree.
func resolveVirtualImport(importingFile, pattern string) (string, error) {
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
