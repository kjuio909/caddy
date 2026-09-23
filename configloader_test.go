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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// dynTestApp is a trivial app whose Start/Stop calls are recorded so
// tests can observe which config is actually running.
type dynTestApp struct {
	Value string `json:"value,omitempty"`
}

func (dynTestApp) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "dyn_test_app",
		New: func() Module { return new(dynTestApp) },
	}
}

func (a dynTestApp) Start() error {
	dynAppRec.start(a.Value)
	return nil
}

func (a dynTestApp) Stop() error {
	dynAppRec.stop(a.Value)
	return nil
}

var dynAppRec = newDynAppRecorder()

type dynAppRecorder struct {
	mu            sync.Mutex
	startCount    int
	stopCount     int
	startedValues []string
	currentValue  string
}

func newDynAppRecorder() *dynAppRecorder { return &dynAppRecorder{} }

func (r *dynAppRecorder) start(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCount++
	r.startedValues = append(r.startedValues, value)
	r.currentValue = value
}

func (r *dynAppRecorder) stop(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopCount++
	if r.currentValue == value {
		r.currentValue = ""
	}
}

func (r *dynAppRecorder) snapshot() (starts, stops int, values []string, current string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startCount, r.stopCount, append([]string(nil), r.startedValues...), r.currentValue
}

func (r *dynAppRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCount = 0
	r.stopCount = 0
	r.startedValues = nil
	r.currentValue = ""
}

// dynTestConfigLoader is a scriptable caddy.ConfigLoader. It returns the
// configured response, optionally blocking until releaseCh is closed or
// the context is canceled.
type dynTestConfigLoader struct{}

func (dynTestConfigLoader) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "caddy.config_loaders.dyn_test",
		New: func() Module { return new(dynTestConfigLoader) },
	}
}

func (dynTestConfigLoader) LoadConfig(ctx Context) ([]byte, error) {
	dynTestScript.mu.Lock()
	cfg, err := dynTestScript.cfg, dynTestScript.err
	block := dynTestScript.blockCh
	dynTestScript.mu.Unlock()

	dynTestScript.calls.Add(1)

	// simulate a slow pull; when released, re-read the configured
	// response so tests can stage a "late result" that arrives after
	// the context was canceled
	if block != nil {
		<-block
		dynTestScript.mu.Lock()
		cfg, err = dynTestScript.cfg, dynTestScript.err
		dynTestScript.mu.Unlock()
	}
	return cfg, err
}

var dynTestScript = struct {
	mu      sync.Mutex
	cfg     []byte
	err     error
	blockCh chan struct{}
	calls   atomic.Int64
}{}

func resetDynTestScript() {
	dynTestScript.mu.Lock()
	dynTestScript.cfg = nil
	dynTestScript.err = nil
	dynTestScript.blockCh = nil
	dynTestScript.mu.Unlock()
	dynTestScript.calls.Store(0)
}

func setDynTestResponse(cfg []byte, err error) {
	dynTestScript.mu.Lock()
	defer dynTestScript.mu.Unlock()
	dynTestScript.cfg = cfg
	dynTestScript.err = err
	dynTestScript.blockCh = nil
}

func setDynTestBlocking() chan struct{} {
	ch := make(chan struct{})
	dynTestScript.mu.Lock()
	dynTestScript.cfg = nil
	dynTestScript.err = nil
	dynTestScript.blockCh = ch
	dynTestScript.mu.Unlock()
	return ch
}

func init() {
	RegisterModule(dynTestApp{})
	RegisterModule(dynTestConfigLoader{})
}

// dynTestConfig builds a JSON config running dynTestApp with the given
// value. If withLoader is true, it also configures the scriptable config
// loader; withDelay controls whether a load delay is set.
func dynTestConfig(t *testing.T, value string, withLoader, withDelay bool) []byte {
	t.Helper()
	cfg := map[string]any{
		"admin": map[string]any{"disabled": true},
		"apps": map[string]any{
			"dyn_test_app": map[string]any{"value": value},
		},
	}
	if withLoader {
		loaderCfg := map[string]any{"load": map[string]any{"module": "dyn_test"}}
		if withDelay {
			loaderCfg["load_delay"] = "20ms"
		}
		cfg["admin"].(map[string]any)["config"] = loaderCfg
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return b
}

// currentRawConfig returns the JSON of the currently running raw config.
func currentRawConfig(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := readConfig("/"+rawConfigKey, &buf); err != nil {
		t.Fatalf("reading config: %v", err)
	}
	return buf.Bytes()
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newTestDynLoader builds a polling dynamicConfigLoader wired to the
// active context, with an observer-backed logger for assertions.
func newTestDynLoader(t *testing.T, delay time.Duration) (*dynamicConfigLoader, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core).
		Named("config_loader").
		With(zap.String("module", "dyn_test"), zap.Duration("load_delay", delay))

	d := &dynamicConfigLoader{
		module:    dynTestConfigLoader{},
		loader:    dynTestConfigLoader{},
		ctx:       ActiveContext(),
		loadDelay: delay,
		logger:    logger,
	}
	return d, logs
}

// TestDynConfigSyncPullFailureFailsStartup verifies that without a load
// delay, the config is pulled once synchronously and a pull failure
// makes startup fail.
func TestDynConfigSyncPullFailureFailsStartup(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	_ = Stop()
	t.Cleanup(func() { _ = Stop() })

	setDynTestResponse(nil, errors.New("synthetic pull failure"))

	err := Load(dynTestConfig(t, "A", true, false), true)
	if err == nil {
		t.Fatal("expected startup to fail when synchronous pull fails, but it succeeded")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("loading dynamic config")) {
		t.Fatalf("expected dynamic config loading error, got: %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("synthetic pull failure")) {
		t.Fatalf("expected underlying loader error in failure, got: %v", err)
	}

	// no config is left running after the failed startup
	if got := currentRawConfig(t); len(bytes.TrimSpace(got)) > 0 && !bytes.Equal(bytes.TrimSpace(got), []byte("null")) {
		t.Fatalf("expected no running config after failed startup, got %s", got)
	}
}

// TestDynConfigSyncPullSuccessAppliedAsync verifies that a successful
// synchronous pull replaces the config after startup.
func TestDynConfigSyncPullSuccessAppliedAsync(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)

	if err := Load(dynTestConfig(t, "A", true, false), true); err != nil {
		t.Fatalf("startup: %v", err)
	}

	waitFor(t, "pulled config B to be applied", func() bool {
		return bytes.Contains(currentRawConfig(t), []byte(`"value":"B"`))
	})

	starts, _, values, _ := dynAppRec.snapshot()
	if starts != 2 {
		t.Fatalf("expected A and B each started once (2 starts), got %d (%v)", starts, values)
	}
}

// TestDynConfigEndToEndPollFailureThenRecovery drives the full JSON
// wiring (admin.config.load + positive load_delay) through Load: the
// production polling goroutine is started, pulls fail at first, and a
// later valid config is applied exactly once.
func TestDynConfigEndToEndPollFailureThenRecovery(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	_ = Stop()
	t.Cleanup(func() { _ = Stop() })

	setDynTestResponse(nil, errors.New("temporary outage"))

	if err := Load(dynTestConfig(t, "A", true, true), true); err != nil {
		t.Fatalf("loading bootstrap config with polling loader: %v", err)
	}

	// failures must not dislodge the bootstrap config
	time.Sleep(60 * time.Millisecond)
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"A"`)) {
		t.Fatalf("expected A to remain during pull failures, got %s", got)
	}

	// recovery is picked up on a later tick
	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)
	waitFor(t, "production poller to apply B", func() bool {
		return bytes.Contains(currentRawConfig(t), []byte(`"value":"B"`))
	})

	// B must be applied exactly once despite ongoing polling
	time.Sleep(60 * time.Millisecond)
	var bStarts int
	_, _, values, current := dynAppRec.snapshot()
	for _, v := range values {
		if v == "B" {
			bStarts++
		}
	}
	if current != "B" || bStarts != 1 {
		t.Fatalf("expected B applied exactly once (current=%q), got %d B-starts (%v)", current, bStarts, values)
	}
}

// TestDynConfigPollPullFailureThenSuccess verifies that pull failures
// are logged with the module name, pull stage, and underlying error, and
// are retried on every tick until a valid config is applied once.
func TestDynConfigPollPullFailureThenSuccess(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)
	setDynTestResponse(nil, errors.New("synthetic pull failure"))

	go d.runPolling()
	t.Cleanup(func() { setDynTestResponse(nil, errors.New("stop")) })

	// wait for at least two failed pull attempts
	waitFor(t, "repeated pull failures", func() bool { return dynTestScript.calls.Load() >= 2 })

	// the previously running config is untouched while pulls fail
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"A"`)) {
		t.Fatalf("expected config A to remain during pull failures, got %s", got)
	}
	starts, stops, _, _ := dynAppRec.snapshot()
	if starts != 1 || stops != 0 {
		t.Fatalf("expected only A running with no restarts during pull failures, got starts=%d stops=%d", starts, stops)
	}

	// error log must carry module, pull stage, and underlying error
	pullErrs := logs.FilterMessage("failed loading dynamic config; will retry").All()
	if len(pullErrs) == 0 {
		t.Fatal("expected a pull failure log entry")
	}
	e := pullErrs[0]
	if e.LoggerName != "config_loader" {
		t.Fatalf("expected logger name config_loader, got %q", e.LoggerName)
	}
	ctxMap := e.ContextMap()
	if ctxMap["module"] != "dyn_test" {
		t.Fatalf("expected module=dyn_test in log, got %v", ctxMap["module"])
	}
	if ctxMap["stage"] != "pull" {
		t.Fatalf("expected stage=pull in log, got %v", ctxMap["stage"])
	}
	if !bytes.Contains([]byte(fmt.Sprintf("%v", ctxMap["error"])), []byte("synthetic pull failure")) {
		t.Fatalf("expected underlying error in log, got %#v", ctxMap["error"])
	}

	// loader recovers: a valid config is applied exactly once
	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)
	waitFor(t, "pulled config B to be applied", func() bool {
		return bytes.Contains(currentRawConfig(t), []byte(`"value":"B"`))
	})

	// give the poller time to make sure B is not applied more than once
	time.Sleep(5 * delay)
	starts, _, values, current := dynAppRec.snapshot()
	if current != "B" {
		t.Fatalf("expected B to be the running app value, got %q", current)
	}
	var bStarts int
	for _, v := range values {
		if v == "B" {
			bStarts++
		}
	}
	if bStarts != 1 {
		t.Fatalf("expected B to be applied exactly once, got %d (%v)", bStarts, values)
	}
}

// TestDynConfigPollApplyFailureKeepsOldAndRecovers verifies that when a
// pulled config fails to apply, the old config remains available (e.g.
// via GET /config/) and a later valid config recovers the channel.
func TestDynConfigPollApplyFailureKeepsOldAndRecovers(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)
	// valid JSON, but the app module does not exist, so applying it
	// fails after decoding and the old config must be restored
	setDynTestResponse([]byte(`{"admin":{"disabled":true},"apps":{"no_such_app":{}}}`), nil)

	go d.runPolling()

	// wait for an apply failure
	waitFor(t, "an apply failure", func() bool {
		return len(logs.FilterField(zap.String("stage", "apply")).
			FilterMessage("failed to apply dynamically-loaded config; keeping previous config and will retry").All()) > 0
	})

	// old config is preserved and still served
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"A"`)) {
		t.Fatalf("expected config A to remain after failed apply, got %s", got)
	}
	starts, stops, _, current := dynAppRec.snapshot()
	if current != "A" || starts != 1 || stops != 0 {
		t.Fatalf("expected A to keep running after failed apply, got current=%q starts=%d stops=%d", current, starts, stops)
	}

	// a later valid config recovers
	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)
	waitFor(t, "pulled config B to be applied after recovery", func() bool {
		return bytes.Contains(currentRawConfig(t), []byte(`"value":"B"`))
	})
	starts, _, _, current = dynAppRec.snapshot()
	if current != "B" {
		t.Fatalf("expected B running after recovery, got %q", current)
	}
	if starts != 2 {
		t.Fatalf("expected exactly one successful transition to B (starts=2), got %d", starts)
	}
}

// TestDynConfigPollSameConfigUnchanged verifies that repeatedly pulling
// the same config is logged as unchanged, does not restart anything,
// and polling continues.
func TestDynConfigPollSameConfigUnchanged(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	aCfg := dynTestConfig(t, "A", false, false)
	if err := Load(aCfg, true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)
	// the loader keeps returning exactly the running config
	setDynTestResponse(aCfg, nil)

	go d.runPolling()

	// several identical returns across ticks
	waitFor(t, "multiple identical pulls", func() bool { return dynTestScript.calls.Load() >= 3 })
	time.Sleep(3 * delay)

	starts, stops, _, current := dynAppRec.snapshot()
	if current != "A" || starts != 1 || stops != 0 {
		t.Fatalf("identical config must not restart apps, got current=%q starts=%d stops=%d", current, starts, stops)
	}
	unchanged := logs.FilterMessage("dynamically-loaded config was unchanged; will continue polling").All()
	if len(unchanged) == 0 {
		t.Fatal("expected an unchanged-config log entry")
	}
	if unchanged[0].ContextMap()["stage"] != "apply" {
		t.Fatalf("expected stage=apply on unchanged log, got %v", unchanged[0].ContextMap())
	}

	// polling must still be going (calls keep increasing)
	callsBefore := dynTestScript.calls.Load()
	waitFor(t, "polling to continue after unchanged configs", func() bool {
		return dynTestScript.calls.Load() > callsBefore
	})
}

// TestDynConfigPollNilIsNoOp verifies nil (empty) pulls are logged as
// no-ops and never alter the running config.
func TestDynConfigPollNilIsNoOp(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)
	setDynTestResponse(nil, nil)

	go d.runPolling()

	waitFor(t, "multiple nil pulls", func() bool { return dynTestScript.calls.Load() >= 3 })
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"A"`)) {
		t.Fatalf("nil pull must not change the config, got %s", got)
	}
	if len(logs.FilterMessage("dynamically-loaded config was nil; will retry").All()) == 0 {
		t.Fatal("expected a nil-config log entry")
	}
	starts, stops, _, _ := dynAppRec.snapshot()
	if starts != 1 || stops != 0 {
		t.Fatalf("nil pull must not restart apps, got starts=%d stops=%d", starts, stops)
	}
}

// TestDynConfigPollConsecutiveFailuresThenSuccess combines several
// failed pull rounds followed by a successful config application.
func TestDynConfigPollConsecutiveFailuresThenSuccess(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)
	setDynTestResponse(nil, errors.New("outage"))

	go d.runPolling()

	waitFor(t, "several consecutive failures", func() bool {
		return len(logs.FilterMessage("failed loading dynamic config; will retry").All()) >= 3
	})

	setDynTestResponse(nil, nil) // one no-op round
	waitFor(t, "a nil no-op round", func() bool {
		return len(logs.FilterMessage("dynamically-loaded config was nil; will retry").All()) >= 1
	})

	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)
	waitFor(t, "recovery config B to be applied", func() bool {
		return bytes.Contains(currentRawConfig(t), []byte(`"value":"B"`))
	})
	if _, _, _, current := dynAppRec.snapshot(); current != "B" {
		t.Fatalf("expected B after consecutive failures then success, got %q", current)
	}
}

// TestDynConfigPollCancellationStops verifies that canceling the
// context stops the ticker, that late pull results never overwrite the
// running config, and that polls stop being made.
func TestDynConfigPollCancellationStops(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	delay := 20 * time.Millisecond
	d, logs := newTestDynLoader(t, delay)

	// stage a pull that blocks in flight and will, once released,
	// return config B as a "late result"
	release := setDynTestBlocking()
	go d.runPolling()

	// wait until the blocked pull is in flight
	waitFor(t, "blocked pull in flight", func() bool { return dynTestScript.calls.Load() >= 1 })

	// switch the running config to C; this cancels the context the
	// poller was using
	if err := Load(dynTestConfig(t, "C", false, false), true); err != nil {
		t.Fatalf("loading replacement config: %v", err)
	}

	// stage the late result (B) from the canceled pull, then release it
	setDynTestResponse(dynTestConfig(t, "B", false, false), nil)
	close(release)

	// wait for poller to acknowledge shutdown
	waitFor(t, "poller shutdown", func() bool {
		return len(logs.FilterMessage("stopping dynamic config loading").All()) > 0
	})

	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"C"`)) {
		t.Fatalf("late result must not overwrite running config C, got %s", got)
	}
	if _, _, _, current := dynAppRec.snapshot(); current != "C" {
		t.Fatalf("expected C to remain the running app, got %q", current)
	}

	// direct apply after cancellation must be a no-op
	if err := d.apply(dynTestConfig(t, "B", false, false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from stale apply, got %v", err)
	}
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"C"`)) {
		t.Fatalf("stale apply must not overwrite config C, got %s", got)
	}

	// no further pulls happen after shutdown
	callsAfterShutdown := dynTestScript.calls.Load()
	time.Sleep(3 * delay)
	if got := dynTestScript.calls.Load(); got != callsAfterShutdown {
		t.Fatalf("poller must stop pulling after cancellation, calls went %d -> %d", callsAfterShutdown, got)
	}
}

// TestDynConfigPulledConfigRecursionGuard verifies that a pulled config
// cannot synchronously pull another config (no load delay).
func TestDynConfigPulledConfigRecursionGuard(t *testing.T) {
	dynAppRec.reset()
	resetDynTestScript()
	t.Cleanup(func() { _ = Stop() })

	if err := Load(dynTestConfig(t, "A", false, false), true); err != nil {
		t.Fatalf("loading bootstrap config: %v", err)
	}

	// pulled config itself pulls synchronously (no delay) -> recursion error
	pulled := dynTestConfig(t, "B", true, false)
	d, _ := newTestDynLoader(t, time.Millisecond)

	err := d.apply(pulled)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("recursive config loading detected")) {
		t.Fatalf("expected recursive config loading error, got %v", err)
	}
	if got := currentRawConfig(t); !bytes.Contains(got, []byte(`"value":"A"`)) {
		t.Fatalf("rejected recursive pulled config must leave A running, got %s", got)
	}
}
