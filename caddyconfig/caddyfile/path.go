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
	"os"
	"path/filepath"
)

// Case sensitivity of path comparisons is a property of the filesystem a
// file resides on, not of the operating system or build target: a
// case-insensitive volume may be mounted on a Unix host (for example the
// default macOS data volume) and a case-sensitive volume on Windows. It is
// therefore observed at runtime against the directory in question rather
// than fixed with a build tag.

// probeCaseSensitive reports whether identity comparisons under dir are
// case-sensitive by creating a throwaway file whose name contains an ASCII
// letter and checking whether the same name with that letter's case
// flipped addresses the same file. Flipping onto the same inode means the
// filesystem ignores case; a missing flipped name means it does not.
//
// dir should be an existing directory. When it cannot be probed (read-only
// directory, permission error, ...) it conservatively reports
// case-sensitive, which only disables case folding and never masks a real
// I/O error.
func probeCaseSensitive(dir string) bool {
	f, err := os.CreateTemp(dir, ".caddy-case-*")
	if err != nil {
		return true
	}
	origName := f.Name()
	f.Close()
	defer os.Remove(origName)

	flippedName := filepath.Join(dir, flipLetter(filepath.Base(origName)))
	origInfo, err := os.Stat(origName)
	if err != nil {
		return true
	}
	flippedInfo, err := os.Stat(flippedName)
	if err != nil {
		return true
	}
	return !os.SameFile(origInfo, flippedInfo)
}

// flipLetter returns name with the case of its first ASCII letter toggled,
// or name unchanged when it contains no letter.
func flipLetter(name string) string {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			return name[:i] + string(r-'a'+'A') + name[i+1:]
		case r >= 'A' && r <= 'Z':
			return name[:i] + string(r-'A'+'a') + name[i+1:]
		}
	}
	return name
}
