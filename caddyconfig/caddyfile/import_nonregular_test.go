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
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// TestAdapterConcurrentNonRegularIsolation mixes adaptations whose import
// target is a FIFO (must fail fast with the non-regular-file category, never
// block) with successful adaptations of a separate tree, all through one
// reused public Adapter. Every result is compared against a fresh-Adapter
// oracle and checked for cross-tree contamination; launch order is shuffled
// and the batch repeated.
func TestAdapterConcurrentNonRegularIsolation(t *testing.T) {
	base := t.TempDir()
	goodDir := filepath.Join(base, "treeGood")
	badDir := filepath.Join(base, "treeBad")

	goodTree := buildImportTree(t, goodDir, "host-good", true)

	fifo := filepath.Join(badDir, "pipe.conf")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", badDir, err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	// direct FIFO import and a glob that matches the FIFO alongside a normal
	// file; both must fail with the non-regular category.
	writeImportFile(t, filepath.Join(badDir, "a-regular.conf"), site("host-bad-normal"))
	directBody := []byte("import " + fifo + "\n")
	globBody := []byte("import " + filepath.Join(badDir, "*.conf") + "\n")

	var calls atomic.Int64
	shared := Adapter{ServerType: recordingServerType{calls: &calls}}
	fresh := func() Adapter { return Adapter{ServerType: recordingServerType{}} }

	oracleGood := adaptNow(t, fresh(), goodTree.body, goodTree.root)
	if oracleGood.errText != "" {
		t.Fatalf("good tree oracle should succeed, got: %s", oracleGood.errText)
	}

	mustFailNonRegular := func(label string, res adaptation) {
		t.Helper()
		if res.errText == "" {
			t.Fatalf("%s: expected a non-regular-file error, got success: %s", label, res.out)
		}
		if !strings.Contains(res.errText, "not a regular file") {
			t.Errorf("%s: expected a non-regular-file category, got: %s", label, res.errText)
		}
		if res.out != nil {
			t.Errorf("%s: failure must not return a partial body (%d bytes)", label, len(res.out))
		}
		if strings.Contains(res.errText, goodDir) {
			t.Errorf("%s: error leaks the good tree's path: %s", label, res.errText)
		}
	}

	oracleDirect := adaptNow(t, fresh(), directBody, filepath.Join(badDir, "Caddyfile"))
	mustFailNonRegular("oracle direct", oracleDirect)
	oracleGlob := adaptNow(t, fresh(), globBody, filepath.Join(badDir, "Caddyfile"))
	mustFailNonRegular("oracle glob", oracleGlob)

	type job struct {
		label  string
		body   []byte
		root   string
		oracle adaptation
	}
	jobs := []job{
		{"good#1", goodTree.body, goodTree.root, oracleGood},
		{"good#2", goodTree.body, goodTree.root, oracleGood},
		{"fifo-direct", directBody, filepath.Join(badDir, "Caddyfile"), oracleDirect},
		{"fifo-glob", globBody, filepath.Join(badDir, "Caddyfile"), oracleGlob},
	}

	const rounds = 6
	for r := 0; r < rounds; r++ {
		results := make([]adaptation, len(jobs))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, idx := range rand.Perm(len(jobs)) {
			wg.Add(1)
			go func(j job, slot int) {
				defer wg.Done()
				<-start
				results[slot] = adaptNow(t, shared, j.body, j.root)
			}(jobs[idx], idx)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		close(start)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("round %d: an adaptation blocked (likely on the FIFO)", r)
		}

		for idx, j := range jobs {
			res := results[idx]
			label := "round " + strconv.Itoa(r) + " " + j.label
			assertSameResult(t, label, res, j.oracle)
			if res.errText == "" {
				assertResultContainedInTree(t, label, badDir, nil, res)
				if res.out == nil {
					t.Errorf("%s: success returned no body", label)
				}
			} else {
				mustFailNonRegular(label, res)
			}
		}
	}

	// only the good adaptations may have reached Setup, exactly once each.
	if calls.Load() != int64(rounds*2) {
		t.Errorf("Setup invocation count = %d, want %d", calls.Load(), rounds*2)
	}
}
