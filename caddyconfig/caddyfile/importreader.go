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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// ImportReader abstracts read access to the files reachable from
// import directives. A single Parse call reads through one fixed
// snapshot of the reader, so swapping the underlying data while a
// parse is in flight cannot mix versions into the result.
type ImportReader interface {
	// ReadFile returns the contents of the named file.
	ReadFile(name string) ([]byte, error)

	// Stat returns the FileInfo for the named file.
	Stat(name string) (fs.FileInfo, error)

	// Glob returns the names of all files matching pattern.
	Glob(pattern string) ([]string, error)
}

// ParseOption customizes the behavior of Parse.
type ParseOption func(*parseOptions)

type parseOptions struct {
	importReader ImportReader
}

// WithImportReader resolves import directives against reader instead
// of the operating system filesystem. Paths handed to reader are
// cleaned absolute paths; relative imports are joined with the
// directory of the file that declares them first.
func WithImportReader(reader ImportReader) ParseOption {
	return func(o *parseOptions) {
		o.importReader = reader
	}
}

// osImportReader reads files from the operating system filesystem.
// It preserves the historical behavior of Parse.
type osImportReader struct{}

func (osImportReader) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (osImportReader) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

func (osImportReader) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// MapImportReader is an in-memory ImportReader backed by a map of
// cleaned file paths to their contents. It is safe for ReadFile,
// Stat, and Glob to be invoked concurrently on the same reader, as
// long as the map is not being mutated at the same time; each Parse
// copies anything it reads, so later mutations never affect an
// in-flight or completed parse.
type MapImportReader struct {
	files map[string]string
	dirs  map[string]struct{}
}

// NewMapImportReader returns a MapImportReader holding files. The
// keys are file paths and the values are their contents; parent
// directories of every key are synthesized for Stat and Glob.
func NewMapImportReader(files map[string]string) *MapImportReader {
	r := &MapImportReader{
		files: make(map[string]string, len(files)),
		dirs:  make(map[string]struct{}),
	}
	for name, contents := range files {
		r.add(filepath.Clean(name), contents)
	}
	return r
}

// Snapshot returns a deep copy of the tree. Parse snapshots its
// reader at the very start, so the returned tree is fixed for the
// whole call even when the original map is mutated concurrently.
func (r *MapImportReader) Snapshot() ImportReader {
	frozen := &MapImportReader{
		files: make(map[string]string, len(r.files)),
		dirs:  make(map[string]struct{}, len(r.dirs)),
	}
	for name, contents := range r.files {
		frozen.files[name] = contents
	}
	for d := range r.dirs {
		frozen.dirs[d] = struct{}{}
	}
	return frozen
}

// add stores a file and records its parent directories.
func (r *MapImportReader) add(name, contents string) {
	r.files[name] = contents
	for d := filepath.Dir(name); ; d = filepath.Dir(d) {
		r.dirs[d] = struct{}{}
		if filepath.Dir(d) == d {
			break
		}
	}
}

func (r *MapImportReader) ReadFile(name string) ([]byte, error) {
	if contents, ok := r.files[filepath.Clean(name)]; ok {
		return []byte(contents), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (r *MapImportReader) Stat(name string) (fs.FileInfo, error) {
	name = filepath.Clean(name)
	if contents, ok := r.files[name]; ok {
		return mapFileInfo{name: filepath.Base(name), size: int64(len(contents))}, nil
	}
	if _, ok := r.dirs[name]; ok {
		return mapFileInfo{name: filepath.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (r *MapImportReader) Glob(pattern string) ([]string, error) {
	pattern = filepath.Clean(pattern)
	var matches []string
	if _, ok := r.files[pattern]; ok {
		matches = []string{pattern}
	} else {
		for name := range r.files {
			matched, err := path.Match(filepath.ToSlash(pattern), filepath.ToSlash(name))
			if err != nil {
				return nil, err
			}
			if matched {
				matches = append(matches, name)
			}
		}
		sort.Strings(matches)
	}
	return matches, nil
}

// mapFileInfo is a minimal fs.FileInfo for files held by a
// MapImportReader; only Name, Size, Mode, and IsDir are meaningful.
type mapFileInfo struct {
	name string
	size int64
	dir  bool
}

func (i mapFileInfo) Name() string { return i.name }
func (i mapFileInfo) Size() int64  { return i.size }

func (i mapFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (i mapFileInfo) ModTime() time.Time { return time.Time{} }
func (i mapFileInfo) IsDir() bool        { return i.dir }
func (i mapFileInfo) Sys() any           { return nil }

// cachedImport is the frozen view of a single imported file for one
// Parse call: its stat info and a private copy of its bytes.
type cachedImport struct {
	info fs.FileInfo
	data []byte
}

// importSnapshot pins the files observed by one Parse call. The
// first time a path is read, its info and contents are copied out
// of the reader; every later use (including another import
// statement referencing the same file) sees the same bytes even if
// the reader changes underneath. Snapshots are per-parse, so
// concurrent parses never share state.
type importSnapshot struct {
	reader ImportReader
	cache  map[string]*cachedImport
}

func newImportSnapshot(reader ImportReader) *importSnapshot {
	return &importSnapshot{
		reader: reader,
		cache:  make(map[string]*cachedImport),
	}
}

// glob returns the cleaned, lexicographically sorted matches for
// pattern. The returned files are not fetched yet; callers pin the
// survivors with pin before splicing any tokens, so the file set and
// its contents are fixed together.
func (s *importSnapshot) glob(pattern string) ([]string, error) {
	matches, err := s.reader.Glob(pattern)
	if err != nil {
		return nil, err
	}
	for i := range matches {
		matches[i] = filepath.Clean(matches[i])
	}
	sort.Strings(matches)
	return matches, nil
}

// pin fetches and caches every named file in one pass, fixing the
// whole batch before expansion begins. Files already cached keep
// their earlier bytes.
func (s *importSnapshot) pin(names []string) error {
	for _, name := range names {
		if _, err := s.ensure(name); err != nil {
			return err
		}
	}
	return nil
}

// ensure returns the cached file, fetching and copying it from the
// reader on first use. Directories are cached without contents.
func (s *importSnapshot) ensure(name string) (*cachedImport, error) {
	if c, ok := s.cache[name]; ok {
		return c, nil
	}
	info, err := s.reader.Stat(name)
	if err != nil {
		return nil, err
	}
	c := &cachedImport{info: info}
	if !info.IsDir() {
		data, err := s.reader.ReadFile(name)
		if err != nil {
			return nil, err
		}
		c.data = append([]byte(nil), data...)
	}
	s.cache[name] = c
	return c, nil
}

// resolveImportPattern resolves pattern against the directory of
// the file whose token declares the import and cleans dot segments.
func resolveImportPattern(declaringFile, pattern string) (string, error) {
	if filepath.IsAbs(pattern) {
		return filepath.Clean(pattern), nil
	}
	absDeclaring, err := caddy.FastAbs(declaringFile)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(filepath.Dir(absDeclaring), pattern)), nil
}
