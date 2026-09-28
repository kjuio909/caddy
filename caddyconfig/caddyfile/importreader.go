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
	"io"
	"os"
	"path/filepath"
	"slices"
)

// ImportReader abstracts access to files pulled in by import directives.
// All paths handed to an ImportReader are absolute and have been
// cleaned of any "." or ".." segments.
type ImportReader interface {
	// ReadFile returns the contents of the file at absPath.
	ReadFile(absPath string) ([]byte, error)

	// Glob returns the paths matching pattern, which is an absolute,
	// cleaned filepath.Glob pattern.
	Glob(pattern string) ([]string, error)
}

// OSImportReader reads imported files from the local filesystem.
type OSImportReader struct{}

// ReadFile reads the file at absPath from the local filesystem,
// returning an error if it is a directory.
func (OSImportReader) ReadFile(absPath string) ([]byte, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", absPath)
	}
	file, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// Glob reports the filesystem entries matching absPattern.
func (OSImportReader) Glob(absPattern string) ([]string, error) {
	return filepath.Glob(absPattern)
}

// importSnapshot is the per-Parse-call view of the filesystem. The
// contents of every imported file and every glob result are pinned on
// first use under their normalized absolute path, so replacing, reusing
// or concurrently triggering the underlying reader later in the same
// call cannot mix different versions into the result. A snapshot is
// created per Parse call and is never shared between calls.
type importSnapshot struct {
	reader ImportReader

	files map[string][]byte
	globs map[string][]string
}

// newImportSnapshot returns a snapshot backed by reader, falling back
// to the local filesystem when reader is nil.
func newImportSnapshot(reader ImportReader) *importSnapshot {
	if reader == nil {
		reader = OSImportReader{}
	}
	return &importSnapshot{
		reader: reader,
		files:  make(map[string][]byte),
		globs:  make(map[string][]string),
	}
}

// read returns a copy of the contents of absPath, pinned to the first
// time the path was read within this call. A copy is returned because
// parsing mutates the byte slice while expanding environment variables.
func (s *importSnapshot) read(absPath string) ([]byte, error) {
	key := filepath.Clean(absPath)
	if contents, ok := s.files[key]; ok {
		out := make([]byte, len(contents))
		copy(out, contents)
		return out, nil
	}
	contents, err := s.reader.ReadFile(key)
	if err != nil {
		return nil, err
	}
	s.files[key] = contents
	out := make([]byte, len(contents))
	copy(out, contents)
	return out, nil
}

// glob returns the normalized matches of absPattern in lexicographic
// order, pinned on first use within this call.
func (s *importSnapshot) glob(absPattern string) ([]string, error) {
	key := filepath.Clean(absPattern)
	if matches, ok := s.globs[key]; ok {
		return matches, nil
	}
	matches, err := s.reader.Glob(key)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(matches))
	normalized := make([]string, 0, len(matches))
	for _, match := range matches {
		name := filepath.Clean(match)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	slices.Sort(normalized)
	s.globs[key] = normalized
	return normalized, nil
}
