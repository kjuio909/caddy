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

package txloadtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	// the http app and its built-in handlers (static_response, ...)
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// atomicFailApp is a test-only app module which fails at a chosen
// lifecycle stage. It lets us exercise provision/validate/start failures
// without depending on real resources.
type atomicFailApp struct {
	// Stage is "provision", "validate", or "start".
	Stage string `json:"stage,omitempty"`
}

func (atomicFailApp) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "atomicfailapp",
		New: func() caddy.Module { return new(atomicFailApp) },
	}
}

func (a *atomicFailApp) Provision(caddy.Context) error {
	if a.Stage == "provision" {
		return errors.New("synthetic provision failure")
	}
	return nil
}

func (a *atomicFailApp) Validate() error {
	if a.Stage == "validate" {
		return errors.New("synthetic validation failure")
	}
	return nil
}

func (a *atomicFailApp) Start() error {
	if a.Stage == "start" {
		return errors.New("synthetic start failure")
	}
	return nil
}

func (a *atomicFailApp) Stop() error { return nil }

func init() {
	caddy.RegisterModule(atomicFailApp{})
}

// TestMain isolates global on-disk state (the autosaved config) in a
// temp directory so tests never touch the developer's real Caddy data.
func TestMain(m *testing.M) {
	origAutosave := caddy.ConfigAutosavePath
	caddy.ConfigAutosavePath = filepath.Join(os.TempDir(),
		fmt.Sprintf("caddy-tx-test-%d-autosave.json", os.Getpid()))
	_ = os.Remove(caddy.ConfigAutosavePath)
	code := m.Run()
	_ = caddy.Stop()
	_ = os.Remove(caddy.ConfigAutosavePath)
	caddy.ConfigAutosavePath = origAutosave
	os.Exit(code)
}

// getFreePort asks the OS for a free loopback TCP port and returns it.
// The listener is closed immediately, so the port is only very likely
// free; callers that need a guaranteed-occupied port keep the listener.
func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// occupyPort binds (and keeps) a loopback TCP port without SO_REUSEPORT,
// so a subsequent Caddy listener on the same address must fail.
func occupyPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying port: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// siteConfig builds a JSON config with an admin endpoint and one HTTP
// server exposing two interrelated paths; the response bodies carry the
// version tag so any mixing of versions is directly observable.
func siteConfig(adminPort, httpPort int, tag string) []byte {
	return []byte(fmt.Sprintf(`{
	"admin": {"listen": "localhost:%d", "disabled": false},
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": ["127.0.0.1:%d"],
					"routes": [
						{"match": [{"path": ["/a"]}], "handle": [{"handler": "static_response", "body": "%s-a"}]},
						{"match": [{"path": ["/b"]}], "handle": [{"handler": "static_response", "body": "%s-b"}]}
					]
				}
			}
		}
	}
}`, adminPort, httpPort, tag, tag))
}

// configWithFailApp builds a config containing both the real HTTP server
// (version tag) and the failing app at the given stage.
func configWithFailApp(adminPort, httpPort int, stage string) []byte {
	return []byte(fmt.Sprintf(`{
	"admin": {"listen": "localhost:%d"},
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": ["127.0.0.1:%d"],
					"routes": [
						{"match": [{"path": ["/a"]}], "handle": [{"handler": "static_response", "body": "old-a"}]}
					]
				}
			}
		},
		"atomicfailapp": {"stage": %q}
	}
}`, adminPort, httpPort, stage))
}

func configWithAppListen(adminPort int, httpListen []string) []byte {
	listens := ""
	for i, l := range httpListen {
		if i > 0 {
			listens += ","
		}
		listens += fmt.Sprintf("%q", l)
	}
	return []byte(fmt.Sprintf(`{
	"admin": {"listen": "localhost:%d"},
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": [%s],
					"routes": [
						{"match": [{"path": ["/a"]}], "handle": [{"handler": "static_response", "body": "new-a"}]}
					]
				}
			}
		}
	}
}`, adminPort, listens))
}

// httpGet performs a GET and returns the status code and body.
func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test traffic to localhost only
	if err != nil {
		return -1, ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// eventuallyGet polls url until it returns wantStatus/wantBody or the
// timeout elapses.
func eventuallyGet(t *testing.T, url string, wantStatus int, wantBody string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := httpGet(t, url)
		if status == wantStatus && body == wantBody {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, body := httpGet(t, url)
	t.Fatalf("GET %s: got %d %q, want %d %q", url, status, body, wantStatus, wantBody)
}

// assertBaselineV1 verifies that both interrelated routes of the v1
// config are served. Unmatched paths get Caddy's default empty 200, so
// we confirm v2-specific bodies are absent rather than checking status.
func assertBaselineV1(t *testing.T, httpPort int) {
	t.Helper()
	eventuallyGet(t, fmt.Sprintf("http://127.0.0.1:%d/a", httpPort), http.StatusOK, "v1-a")
	eventuallyGet(t, fmt.Sprintf("http://127.0.0.1:%d/b", httpPort), http.StatusOK, "v1-b")
	// a v2 body must never appear on either route while v1 is active
	_, bodyA := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/a", httpPort))
	if bodyA != "v1-a" {
		t.Fatalf("expected v1 to remain active, /a served %q", bodyA)
	}
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decoding config: %v", err)
	}
	out, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-encoding config: %v", err)
	}
	return out
}

// TestTransactionalConfigLoad verifies that a config load through
// caddy.Load is an atomic, rollback-capable transaction: failed loads
// keep the previous complete config, listeners and admin endpoint
// serving and leave no residue; successful loads switch all routes as
// one; and repeated/sequential loads stay consistent.
func TestTransactionalConfigLoad(t *testing.T) {
	adminPort := getFreePort(t)
	httpPort := getFreePort(t)
	siteURL := func(p string) string {
		return fmt.Sprintf("http://127.0.0.1:%d%s", httpPort, p)
	}
	adminConfigURL := fmt.Sprintf("http://localhost:%d/config/", adminPort)

	t.Cleanup(func() { _ = caddy.Stop() })

	autosavePath := caddy.ConfigAutosavePath
	v1 := siteConfig(adminPort, httpPort, "v1")
	v2 := siteConfig(adminPort, httpPort, "v2")

	// establish the v1 baseline
	if err := caddy.Load(v1, true); err != nil {
		t.Fatalf("loading v1: %v", err)
	}
	assertBaselineV1(t, httpPort)

	// the successful version must have been persisted
	waitForFile(t, autosavePath)
	if got := readFile(t, autosavePath); string(compactJSON(t, got)) != string(compactJSON(t, v1)) {
		t.Fatalf("autosave does not match v1 after successful load")
	}

	t.Run("syntax_error_keeps_v1", func(t *testing.T) {
		err := caddy.Load([]byte(`{ "apps": {`), true)
		if err == nil {
			t.Fatal("expected error for malformed JSON, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)
	})

	t.Run("unknown_module_keeps_v1", func(t *testing.T) {
		err := caddy.Load([]byte(fmt.Sprintf(`{
			"admin": {"listen": "localhost:%d"},
			"apps": {"definitely_not_a_real_app_xyz": {}}
		}`, adminPort)), true)
		if err == nil {
			t.Fatal("expected error for unknown module, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)
	})

	t.Run("provision_failure_keeps_v1", func(t *testing.T) {
		if err := caddy.Load(configWithFailApp(adminPort, httpPort, "provision"), true); err == nil {
			t.Fatal("expected provision error, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)
	})

	t.Run("validation_failure_keeps_v1", func(t *testing.T) {
		if err := caddy.Load(configWithFailApp(adminPort, httpPort, "validate"), true); err == nil {
			t.Fatal("expected validation error, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)
	})

	t.Run("start_failure_keeps_v1_and_releases_ports", func(t *testing.T) {
		before := caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
		if err := caddy.Load(configWithFailApp(adminPort, httpPort, "start"), true); err == nil {
			t.Fatal("expected start error, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)

		// the failed load must not leave an extra listener reference
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort)) == before {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got := caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort)); got != before {
			t.Fatalf("listener usage changed after failed load: before=%d after=%d", before, got)
		}

		// a subsequent valid load must still succeed on the same port,
		// proving the failed attempt left nothing behind
		if err := caddy.Load(v1, true); err != nil {
			t.Fatalf("reloading v1 after failed start: %v", err)
		}
		assertBaselineV1(t, httpPort)
	})

	t.Run("listen_address_conflict_keeps_v1", func(t *testing.T) {
		busyPort := occupyPort(t)
		// point the new HTTP server at a port held by an ordinary
		// (non-SO_REUSEPORT) listener; Start must fail
		err := caddy.Load(configWithAppListen(adminPort,
			[]string{fmt.Sprintf("127.0.0.1:%d", busyPort)}), true)
		if err == nil {
			t.Fatal("expected listen conflict error, got nil")
		}
		assertBaselineV1(t, httpPort)
		assertActiveConfig(t, adminConfigURL, v1)

		// once the conflicting listener goes away, a valid load works
		if err := caddy.Load(v2, true); err != nil {
			t.Fatalf("loading v2 after conflict: %v", err)
		}
		assertVersion(t, siteURL, "v2")
	})

	t.Run("failed_load_does_not_overwrite_autosave", func(t *testing.T) {
		// currently running v2; a broken load must leave the v2 autosave
		if err := caddy.Load([]byte(`not json at all`), true); err == nil {
			t.Fatal("expected syntax error, got nil")
		}
		if got := readFile(t, autosavePath); string(compactJSON(t, got)) != string(compactJSON(t, v2)) {
			t.Fatalf("failed load overwrote autosave; expected v2 to be retained")
		}
	})

	t.Run("idempotent_reload", func(t *testing.T) {
		usage := caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
		for i := 0; i < 3; i++ {
			if err := caddy.Load(v2, true); err != nil {
				t.Fatalf("forced reload %d: %v", i, err)
			}
		}
		assertVersion(t, siteURL, "v2")
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort)) == usage {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("listener usage grew after idempotent reloads: expected %d, got %d",
			usage, caddy.ListenerUsage("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort)))
	})

	t.Run("successful_switch_is_all_or_nothing", func(t *testing.T) {
		if err := caddy.Load(v1, true); err != nil {
			t.Fatalf("loading v1: %v", err)
		}
		assertVersion(t, siteURL, "v1")
		if err := caddy.Load(v2, true); err != nil {
			t.Fatalf("loading v2: %v", err)
		}
		// after the success boundary, both stages must come from v2;
		// v1 routes must be gone (same paths, new bodies)
		assertVersion(t, siteURL, "v2")
		assertActiveConfig(t, adminConfigURL, v2)
	})

	t.Run("serialized_loads_form_clean_version_chain", func(t *testing.T) {
		v3 := siteConfig(adminPort, httpPort, "v3")
		v4 := siteConfig(adminPort, httpPort, "v4")
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			cfg := v3
			if i%2 == 1 {
				cfg = v4
			}
			go func() {
				defer wg.Done()
				_ = caddy.Load(cfg, true)
			}()
		}
		wg.Wait()

		// whatever the final request-ordering was, both routes must
		// belong to the same complete version
		_, bodyA := httpGet(t, siteURL("/a"))
		_, bodyB := httpGet(t, siteURL("/b"))
		if bodyA != "v3-a" && bodyA != "v4-a" {
			t.Fatalf("unexpected /a body after concurrent loads: %q", bodyA)
		}
		wantTag := bodyA[:2] // "v3" or "v4"
		if bodyB != wantTag+"-b" {
			t.Fatalf("version mixing detected: /a served %q but /b served %q", bodyA, bodyB)
		}

		// one more valid load after the burst must still land on a
		// single complete version, unaffected by earlier attempts
		if err := caddy.Load(v1, true); err != nil {
			t.Fatalf("loading v1 after concurrent burst: %v", err)
		}
		assertVersion(t, siteURL, "v1")
	})
}

// TestTransactionalAdminEndpointMove verifies the admin endpoint itself
// participates in the transaction: a failed load that wants to move the
// admin listener to a conflicting port keeps the old admin endpoint
// fully serviceable, and the new port is not left behind.
func TestTransactionalAdminEndpointMove(t *testing.T) {
	adminPort := getFreePort(t)
	httpPort := getFreePort(t)
	t.Cleanup(func() { _ = caddy.Stop() })

	v1 := siteConfig(adminPort, httpPort, "v1")
	if err := caddy.Load(v1, true); err != nil {
		t.Fatalf("loading v1: %v", err)
	}
	assertBaselineV1(t, httpPort)
	adminCfgURL := fmt.Sprintf("http://localhost:%d/config/", adminPort)
	assertActiveConfig(t, adminCfgURL, v1)

	busyAdmin := occupyPort(t)
	moved := []byte(fmt.Sprintf(`{
	"admin": {"listen": "localhost:%d"},
	"apps": {
		"http": {
			"servers": {
				"srv0": {
					"listen": ["127.0.0.1:%d"],
					"routes": [
						{"match": [{"path": ["/a"]}], "handle": [{"handler": "static_response", "body": "v2-a"}]}
					]
				}
			}
		}
	}
}`, busyAdmin, httpPort))

	if err := caddy.Load(moved, true); err == nil {
		t.Fatal("expected admin listen conflict error, got nil")
	}

	// the old admin endpoint must still serve and report v1
	assertActiveConfig(t, adminCfgURL, v1)
	assertBaselineV1(t, httpPort)

	// and a later valid admin-port move must succeed exactly once
	newAdmin := getFreePort(t)
	v3 := siteConfig(newAdmin, httpPort, "v3")
	if err := caddy.Load(v3, true); err != nil {
		t.Fatalf("moving admin endpoint: %v", err)
	}
	eventuallyGet(t, fmt.Sprintf("http://127.0.0.1:%d/a", httpPort), http.StatusOK, "v3-a")
	assertActiveConfig(t, fmt.Sprintf("http://localhost:%d/config/", newAdmin), v3)
	// the old admin address must no longer answer
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := &http.Client{Timeout: 200 * time.Millisecond}
		resp, err := c.Get(fmt.Sprintf("http://localhost:%d/config/", adminPort))
		if err != nil {
			break
		}
		_ = resp.Body.Close()
		time.Sleep(30 * time.Millisecond)
	}
	c := &http.Client{Timeout: 300 * time.Millisecond}
	if resp, err := c.Get(fmt.Sprintf("http://localhost:%d/config/", adminPort)); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("old admin endpoint on %d still answers after successful move", adminPort)
	}
}

func assertVersion(t *testing.T, siteURL func(string) string, tag string) {
	t.Helper()
	eventuallyGet(t, siteURL("/a"), http.StatusOK, tag+"-a")
	eventuallyGet(t, siteURL("/b"), http.StatusOK, tag+"-b")
}

func assertActiveConfig(t *testing.T, adminConfigURL string, want []byte) {
	t.Helper()
	wantCompact := compactJSON(t, want)
	deadline := time.Now().Add(3 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminConfigURL) //nolint:gosec // localhost
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				last = compactJSON(t, body)
				if string(last) == string(wantCompact) {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("active config mismatch:\nwant: %s\n got: %s", wantCompact, last)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear", path)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return raw
}
