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

package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeConfigLoader implements ConfigLoader for tests.
type fakeConfigLoader struct {
	loadConfig func(Context) ([]byte, error)
}

func (f fakeConfigLoader) LoadConfig(ctx Context) ([]byte, error) {
	return f.loadConfig(ctx)
}

// startWatcher runs watchConfigLoader with a short polling interval in a
// goroutine and returns a channel that is closed when the watcher returns.
func startWatcher(ctx Context, loader ConfigLoader, apply func([]byte) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		watchConfigLoader(ctx, loader, apply, time.Millisecond, zap.NewNop())
		close(done)
	}()
	return done
}

func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", msg)
	}
}

// TestWatchConfigLoaderPullFailureRecovery ensures that pull failures and
// nil (no-op) configs are retried until a valid config is pulled and applied.
func TestWatchConfigLoaderPullFailureRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var pulls atomic.Int32
	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		switch pulls.Add(1) {
		case 1, 2:
			return nil, errors.New("pull failed")
		case 3:
			return nil, nil // no-op load
		default:
			return []byte(`{"apps":{}}`), nil
		}
	}}

	applied := make(chan []byte, 1)
	done := startWatcher(Context{Context: ctx}, loader, func(cfg []byte) error {
		applied <- cfg
		return nil
	})

	select {
	case cfg := <-applied:
		if string(cfg) != `{"apps":{}}` {
			t.Errorf("expected pulled config to be applied, got %s", cfg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pulled config to be applied")
	}

	// a successful apply switches once, then the watcher stops
	waitClosed(t, done, "watcher to stop after successful apply")

	if n := pulls.Load(); n != 4 {
		t.Errorf("expected 4 pulls (2 errors, 1 no-op, 1 success), got %d", n)
	}
}

// TestWatchConfigLoaderApplyFailureRecovery ensures that a failed apply
// leaves the running config alone and is retried on the next tick.
func TestWatchConfigLoaderApplyFailureRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		return []byte(`{"apps":{}}`), nil
	}}

	var applies atomic.Int32
	done := startWatcher(Context{Context: ctx}, loader, func([]byte) error {
		if applies.Add(1) == 1 {
			return errors.New("apply failed")
		}
		return nil
	})

	waitClosed(t, done, "watcher to stop after recovered apply")

	if n := applies.Load(); n != 2 {
		t.Errorf("expected 2 apply attempts (1 failure, 1 success), got %d", n)
	}
}

// TestWatchConfigLoaderUnchangedConfig ensures that a pulled config
// identical to the running one is a no-op and polling continues.
func TestWatchConfigLoaderUnchangedConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pulled := make(chan struct{}, 10)
	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		pulled <- struct{}{}
		return []byte(`{"apps":{}}`), nil
	}}

	var applies atomic.Int32
	done := startWatcher(Context{Context: ctx}, loader, func([]byte) error {
		applies.Add(1)
		return errSameConfig
	})

	// polling must continue across repeated unchanged configs
	for i := 0; i < 3; i++ {
		select {
		case <-pulled:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pull %d", i+1)
		}
	}

	// the watcher must still be running; only cancellation stops it
	select {
	case <-done:
		t.Fatal("watcher stopped even though no new config was applied")
	default:
	}

	cancel()
	waitClosed(t, done, "watcher to stop after cancellation")

	if n := applies.Load(); n < 3 {
		t.Errorf("expected at least 3 apply attempts, got %d", n)
	}
}

// TestWatchConfigLoaderCancellation ensures that canceling the context
// stops the timer and the polling loop.
func TestWatchConfigLoaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var pulls atomic.Int32
	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		pulls.Add(1)
		return []byte(`{"apps":{}}`), nil
	}}

	done := make(chan struct{})
	go func() {
		// a long delay so the timer never fires before cancellation
		watchConfigLoader(Context{Context: ctx}, loader, func([]byte) error { return nil }, time.Hour, zap.NewNop())
		close(done)
	}()

	cancel()
	waitClosed(t, done, "watcher to stop after cancellation")

	if n := pulls.Load(); n != 0 {
		t.Errorf("expected no pulls after cancellation, got %d", n)
	}
}

// TestWatchConfigLoaderLateResultDiscarded ensures that a pull that returns
// after the context was canceled does not overwrite the running config.
func TestWatchConfigLoaderLateResultDiscarded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	entered := make(chan struct{})
	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		close(entered)
		<-ctx.Done() // block until canceled, then return a late result
		return []byte(`{"apps":{}}`), nil
	}}

	var applies atomic.Int32
	done := startWatcher(Context{Context: ctx}, loader, func([]byte) error {
		applies.Add(1)
		return nil
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for loader to be called")
	}
	cancel()
	waitClosed(t, done, "watcher to stop after cancellation")

	if n := applies.Load(); n != 0 {
		t.Errorf("expected late pull result to be discarded, but got %d applies", n)
	}
}

// TestWatchConfigLoaderConsecutiveFailuresThenSuccess ensures that a mix of
// consecutive pull and apply failures is recovered from automatically.
func TestWatchConfigLoaderConsecutiveFailuresThenSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var pulls atomic.Int32
	loader := fakeConfigLoader{loadConfig: func(Context) ([]byte, error) {
		if pulls.Add(1) <= 2 {
			return nil, errors.New("pull failed")
		}
		return []byte(`{"apps":{}}`), nil
	}}

	var applies atomic.Int32
	done := startWatcher(Context{Context: ctx}, loader, func(cfg []byte) error {
		if applies.Add(1) == 1 {
			return errors.New("apply failed")
		}
		return nil
	})

	waitClosed(t, done, "watcher to stop after eventual success")

	if n := pulls.Load(); n != 4 {
		t.Errorf("expected 4 pulls (2 failures, 1 failed apply, 1 success), got %d", n)
	}
	if n := applies.Load(); n != 2 {
		t.Errorf("expected 2 apply attempts, got %d", n)
	}
}

// TestPulledConfigApplyFailureKeepsRunningConfig ensures that when a pulled
// config fails to apply, the previous config keeps running and remains
// visible through the config API, and that a pulled config may not pull
// yet another config without a positive load_delay.
func TestPulledConfigApplyFailureKeepsRunningConfig(t *testing.T) {
	initial := []byte(`{"admin":{"listen":"localhost:2998","config":{"persist":false}}}`)
	if err := Load(initial, true); err != nil {
		t.Fatalf("loading initial config: %v", err)
	}
	defer Stop()

	assertConfig := func(expected []byte, msg string) {
		t.Helper()
		var buf bytes.Buffer
		if err := readConfig("/"+rawConfigKey, &buf); err != nil {
			t.Fatalf("reading config: %v", err)
		}
		var expectedCfg, actualCfg any
		if err := json.Unmarshal(expected, &expectedCfg); err != nil {
			t.Fatalf("unmarshaling expected config: %v", err)
		}
		if err := json.Unmarshal(buf.Bytes(), &actualCfg); err != nil {
			t.Fatalf("unmarshaling actual config: %v", err)
		}
		if !reflect.DeepEqual(expectedCfg, actualCfg) {
			t.Fatalf("running config changed after %s\nexpected: %s\nactual:   %s", msg, expected, buf.Bytes())
		}
	}

	// a pulled config that cannot be decoded and run must fail to apply
	// and leave the previous config in place
	badConfig := []byte(`{"apps":123}`)
	err := changeConfig(http.MethodPost, "/"+rawConfigKey, badConfig, "", false, false)
	if err == nil {
		t.Fatal("expected error applying invalid pulled config")
	}
	assertConfig(initial, "failed apply")

	// a pulled config that would itself pull another config without a
	// positive load_delay must be rejected to prevent a tight loop
	recursiveConfig := []byte(`{"admin":{"config":{"load":{"module":"http"}}}}`)
	err = changeConfig(http.MethodPost, "/"+rawConfigKey, recursiveConfig, "", false, false)
	if err == nil || !strings.Contains(err.Error(), "recursive config loading") {
		t.Fatalf("expected recursive config loading error, got: %v", err)
	}
	assertConfig(initial, "rejected recursive pull")

	// a valid pulled config is applied and replaces the current one
	newConfig := []byte(`{"admin":{"listen":"localhost:2998","config":{"persist":false}},"apps":{}}`)
	if err := changeConfig(http.MethodPost, "/"+rawConfigKey, newConfig, "", false, false); err != nil {
		t.Fatalf("expected valid pulled config to be applied, got: %v", err)
	}
	assertConfig(newConfig, "successful apply")

	// pulling the same config again is reported as unchanged
	err = changeConfig(http.MethodPost, "/"+rawConfigKey, newConfig, "", false, false)
	if !errors.Is(err, errSameConfig) {
		t.Fatalf("expected errSameConfig for identical pulled config, got: %v", err)
	}
}
