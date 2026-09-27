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

//go:build !windows

package caddyfile

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Importing a non-regular file must fail rather than block: a FIFO opens
// successfully, so without an explicit regular-file check reading it would
// hang the whole adaptation.
func TestImportNonRegularFileFails(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.conf")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := parseInDir(t, dir, "import "+fifo+"\n")
		done <- result{err: err}
	}()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("expected an error importing a FIFO, got nil")
		}
		if !strings.Contains(res.err.Error(), "not a regular file") {
			t.Errorf("expected a non-regular-file error category, got: %v", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("importing a FIFO blocked instead of failing")
	}
}

// A non-regular file reached through a glob fails the whole adaptation as
// well, rather than being silently skipped or hanging.
func TestImportNonRegularFileViaGlobFails(t *testing.T) {
	dir := t.TempDir()
	writeImportFile(t, filepath.Join(dir, "a.conf"), site("host-a"))
	fifo := filepath.Join(dir, "z-pipe.conf")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := parseInDir(t, dir, "import "+filepath.Join(dir, "*.conf")+"\n")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when a glob matches a FIFO, got nil")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("expected a non-regular-file error category, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("importing a glob with a FIFO blocked instead of failing")
	}
}
