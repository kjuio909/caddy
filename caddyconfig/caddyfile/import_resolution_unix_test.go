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

// Importing a non-regular file such as a FIFO must fail with the existing
// error family and, crucially, must not block while opening it.
func TestImportNonRegularFileFails(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.conf")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := parseInDir(t, dir, "import "+fifo+"\n")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error importing a FIFO, got nil")
		}
		if !strings.Contains(err.Error(), "Could not import") {
			t.Errorf("expected the existing import error category, got: %v", err)
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("expected a non-regular-file diagnostic, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("importing a FIFO blocked instead of being rejected")
	}
}
