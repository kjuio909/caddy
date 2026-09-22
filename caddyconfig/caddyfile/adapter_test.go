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
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// testServerType is a no-op ServerType so the adapter can be
// exercised without a real server type implementation.
type testServerType struct{}

func (testServerType) Setup([]ServerBlock, map[string]any) (*caddy.Config, []caddyconfig.Warning, error) {
	return &caddy.Config{}, nil, nil
}

func TestAdaptImportedFileFormatting(t *testing.T) {
	dir := t.TempDir()

	writeFile := func(name, contents string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// well-formatted root file; imports exercise normal, glob,
	// recursive, duplicate, snippet, empty, and unmatched-glob cases
	writeFile("Caddyfile", "(snippet1) {\n\trespond \"s\"\n}\n\nexample.com {\n\timport snippet1\n\timport imported.caddy\n\timport sites/*\n\timport imported.caddy\n\timport empty.caddy\n\timport nothing-*.caddy\n}\n")
	writeFile("imported.caddy", "imported.example.com {\n        respond \"imported\"\n}\n")
	writeFile("sites/a.caddy", "a.example.com {\n\trespond \"a\"\n}\n")
	writeFile("sites/b.caddy", "b.example.com {\n      respond \"b\"\n\timport ../deep/deep.caddy\n}\n")
	writeFile("deep/deep.caddy", "deep.example.com {\n            respond \"deep\"\n}\n")
	writeFile("empty.caddy", "")

	adapter := Adapter{ServerType: testServerType{}}
	body, err := os.ReadFile(filepath.Join(dir, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}

	_, warnings, err := adapter.Adapt(body, map[string]any{"filename": filepath.Join(dir, "Caddyfile")})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}

	expectedFiles := []string{
		filepath.Join(dir, "imported.caddy"),
		filepath.Join(dir, "sites", "b.caddy"),
		filepath.Join(dir, "deep", "deep.caddy"),
	}
	if len(warnings) != len(expectedFiles) {
		t.Fatalf("expected %d warnings, got %d: %v", len(expectedFiles), len(warnings), warnings)
	}
	for i, warning := range warnings {
		if warning.File != expectedFiles[i] {
			t.Errorf("warning %d: expected file %s, got %s", i, expectedFiles[i], warning.File)
		}
		if warning.Line != 2 {
			t.Errorf("warning %d: expected line 2, got %d", i, warning.Line)
		}
		if warning.Directive != "" {
			t.Errorf("warning %d: expected empty directive, got %s", i, warning.Directive)
		}
		if warning.Message == "" {
			t.Errorf("warning %d: expected non-empty message", i)
		}
	}
}

func TestAdaptImportedFileFormattingRootFirst(t *testing.T) {
	dir := t.TempDir()

	// both the root file and the imported file are unformatted;
	// the root file's warning must come first
	root := "example.com {\n      respond \"root\"\n\timport imported.caddy\n}\n"
	imported := "imported.example.com {\n        respond \"imported\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "imported.caddy"), []byte(imported), 0o644); err != nil {
		t.Fatal(err)
	}

	adapter := Adapter{ServerType: testServerType{}}
	_, warnings, err := adapter.Adapt([]byte(root), map[string]any{"filename": filepath.Join(dir, "Caddyfile")})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}

	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
	if warnings[0].File != filepath.Join(dir, "Caddyfile") {
		t.Errorf("expected first warning for root file, got %s", warnings[0].File)
	}
	if warnings[1].File != filepath.Join(dir, "imported.caddy") {
		t.Errorf("expected second warning for imported file, got %s", warnings[1].File)
	}
}

func TestAdaptImportedFileCRLFNormalized(t *testing.T) {
	dir := t.TempDir()

	// CRLF line endings alone must not trigger a warning
	imported := "imported.example.com {\r\n\trespond \"ok\"\r\n}\r\n"
	if err := os.WriteFile(filepath.Join(dir, "imported.caddy"), []byte(imported), 0o644); err != nil {
		t.Fatal(err)
	}

	adapter := Adapter{ServerType: testServerType{}}
	root := "example.com {\n\timport imported.caddy\n}\n"
	_, warnings, err := adapter.Adapt([]byte(root), map[string]any{"filename": filepath.Join(dir, "Caddyfile")})
	if err != nil {
		t.Fatalf("Adapt returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}
