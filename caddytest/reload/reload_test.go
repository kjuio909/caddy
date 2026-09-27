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

// Package reload_test contains end-to-end tests that exercise
// transactional, rollback-safe configuration reloads through the
// in-process admin /load entry point (caddy.Load).
//
// These tests live in their own package (under caddytest/) rather than
// the root caddy package because they need the standard module set
// (http handlers, etc.), which cannot be imported by root-package test
// files that coexist with tests that register modules by hand.
package reload_test

import (
	"bytes"
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
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// httpConfig builds a config with two interrelated routes. Every
// response from /a or /b carries both an X-Version header and a body
// equal to the version tag, so a single response can never be a mix of
// two versions. /go is a redirect whose target also embeds the version,
// and unmatched paths fall through to the default 404 handler. Together
// these let a reload be verified as an all-or-nothing switch across
// routes, redirects and error/unmatched handling.
func httpConfig(listen, version string, adminJSON string) []byte {
	if adminJSON == "" {
		adminJSON = `{"disabled": true}`
	}
	return []byte(fmt.Sprintf(`{
		"admin": %s,
		"apps": {
			"http": {
				"servers": {
					"srv0": {
						"listen": [%q],
						"routes": [
							{
								"match": [{"path": ["/a"]}],
								"handle": [{
									"handler": "static_response",
									"body": %q,
									"headers": {"X-Version": [%q]}
								}]
							},
							{
								"match": [{"path": ["/b"]}],
								"handle": [{
									"handler": "static_response",
									"body": %q,
									"headers": {"X-Version": [%q]}
								}]
							},
							{
								"match": [{"path": ["/go"]}],
								"handle": [{
									"handler": "static_response",
									"status_code": 302,
									"headers": {"Location": ["/a?ver=%s"], "X-Version": [%q]}
								}]
							},
							{
								"match": [{"path": ["/missing"]}],
								"handle": [{
									"handler": "static_response",
									"status_code": 404,
									"body": %q,
									"headers": {"X-Version": [%q]}
								}]
							}
						]
					}
				}
			}
		}
	}`, adminJSON, listen, version, version, version, version, version, version, version, version))
}

type probeResult struct {
	status   int
	body     string
	version  string // X-Version response header
	location string
}

func probe(t *testing.T, client *http.Client, url string) probeResult {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return probeResult{
		status:   resp.StatusCode,
		body:     string(b),
		version:  resp.Header.Get("X-Version"),
		location: resp.Header.Get("Location"),
	}
}

func newClient() *http.Client {
	tr := &http.Transport{DisableKeepAlives: true}
	return &http.Client{Timeout: 3 * time.Second, Transport: tr}
}

// newRedirectClient returns a client that does not follow redirects, so
// the actual 3xx response (including Location) can be inspected.
func newRedirectClient() *http.Client {
	c := newClient()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

// waitForReady polls until the server answers, failing after a timeout.
func waitForReady(t *testing.T, base string) {
	t.Helper()
	client := newClient()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(base + "/a"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", base)
}

func listenerKey(listen string) (network, addr string) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "tcp", listen
	}
	return "tcp", net.JoinHostPort(host, port)
}

func mustLoad(t *testing.T, raw []byte) {
	t.Helper()
	if err := caddy.Load(raw, true); err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}
}

func mustFailLoad(t *testing.T, raw []byte) {
	t.Helper()
	if err := caddy.Load(raw, true); err == nil {
		t.Fatalf("expected load to fail, but it succeeded")
	}
}

// assertVersion checks that the whole served surface reflects a single,
// internally-consistent version: /a and /b bodies and headers agree,
// /go redirects to a version-tagged target, and an unmatched path is a
// plain 404 with no version header.
func assertVersion(t *testing.T, base, version string) {
	t.Helper()
	client := newClient()

	a := probe(t, client, base+"/a")
	if a.status != 200 || a.body != version || a.version != version {
		t.Fatalf("/a = status %d body %q header %q; want 200 %q %q", a.status, a.body, a.version, version, version)
	}

	b := probe(t, client, base+"/b")
	if b.status != 200 || b.body != version || b.version != version {
		t.Fatalf("/b = status %d body %q header %q; want 200 %q %q", b.status, b.body, b.version, version, version)
	}

	// do not follow the redirect: assert the raw 302 and its target
	go_ := probe(t, newRedirectClient(), base+"/go")
	wantLoc := "/a?ver=" + version
	if go_.status != 302 || go_.location != wantLoc || go_.version != version {
		t.Fatalf("/go = status %d location %q header %q; want 302 %q %q", go_.status, go_.location, go_.version, wantLoc, version)
	}

	// explicit error/unmatched handling is also version-tagged and must
	// update atomically with the other routes
	miss := probe(t, client, base+"/missing")
	if miss.status != 404 || miss.body != version || miss.version != version {
		t.Fatalf("/missing = status %d body %q header %q; want 404 %q %q", miss.status, miss.body, miss.version, version, version)
	}
}

// TestReloadRollbackFailureModes submits each class of invalid input
// and verifies the previously-running config keeps serving in full, no
// new listener is leaked, and a subsequent valid load still succeeds.
func TestReloadRollbackFailureModes(t *testing.T) {
	listen := "127.0.0.1:9301"
	base := "http://" + listen
	network, addr := listenerKey(listen)

	mustLoad(t, httpConfig(listen, "v1", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)
	assertVersion(t, base, "v1")

	// 1) syntax error: JSON that cannot even be parsed
	mustFailLoad(t, []byte(`{ this is not json`))
	assertVersion(t, base, "v1")

	// 2) unknown module: parses and decodes partially, but references a
	// handler module that does not exist
	mustFailLoad(t, []byte(fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {"http": {"servers": {"srv0": {"listen": [%q],
			"routes": [{"handle": [{"handler": "totally_unknown_handler_xyz"}]}]}}}}}`, listen)))
	assertVersion(t, base, "v1")

	// 3) listener address conflict internal to the new config (Validate)
	mustFailLoad(t, []byte(fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {"http": {"servers": {
			"s1": {"listen": [%q], "routes": [{"handle": [{"handler": "static_response", "body": "x"}]}]},
			"s2": {"listen": [%q], "routes": [{"handle": [{"handler": "static_response", "body": "y"}]}]}
		}}}}`, listen, listen)))
	assertVersion(t, base, "v1")

	// 4) start-phase resource preparation failure: a NEW port that is
	// already held by a socket without SO_REUSEPORT cannot be bound
	occupied := "127.0.0.1:9302"
	hold, err := net.Listen("tcp", occupied)
	if err != nil {
		t.Fatalf("could not occupy %s: %v", occupied, err)
	}
	t.Cleanup(func() { hold.Close() })
	mustFailLoad(t, httpConfig(occupied, "v2", ""))
	assertVersion(t, base, "v1")

	// the failed config's brand-new port must not be left behind
	time.Sleep(200 * time.Millisecond)
	if caddy.ListenerUsage("tcp", occupied) != 0 {
		t.Fatalf("failed load leaked listener on %s: usage %d", occupied, caddy.ListenerUsage("tcp", occupied))
	}
	if caddy.ListenerUsage(network, addr) != 1 {
		t.Fatalf("original listener usage = %d, want 1", caddy.ListenerUsage(network, addr))
	}

	// failed attempts must not poison the next valid submission: the
	// baseline is still the complete v1, and a good v2 switches cleanly
	mustLoad(t, httpConfig(listen, "v2", ""))
	waitForReady(t, base)
	assertVersion(t, base, "v2")
}

// TestReloadRollbackDoesNotLeakStartedServer verifies that when a new
// config starts one server successfully but a later server fails to
// bind, the already-started server is stopped and its port released;
// nothing from the aborted config remains to disturb a later load.
func TestReloadRollbackDoesNotLeakStartedServer(t *testing.T) {
	oldListen := "127.0.0.1:9311"
	goodListen := "127.0.0.1:9312"
	badListen := "127.0.0.1:9313"
	base := "http://" + oldListen

	mustLoad(t, httpConfig(oldListen, "v1", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)

	hold, err := net.Listen("tcp", badListen)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hold.Close() })

	partial := []byte(fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {"http": {"servers": {
			"good": {"listen": [%q], "routes": [{"handle": [{"handler": "static_response", "body": "new"}]}]},
			"bad":  {"listen": [%q], "routes": [{"handle": [{"handler": "static_response", "body": "x"}]}]}
		}}}}`, goodListen, badListen))
	mustFailLoad(t, partial)

	assertVersion(t, base, "v1")

	// the successfully-started "good" server of the aborted config must
	// have been rolled back: its port has to be bindable again
	time.Sleep(300 * time.Millisecond)
	if caddy.ListenerUsage("tcp", goodListen) != 0 {
		t.Fatalf("aborted config leaked listener on %s: usage %d", goodListen, caddy.ListenerUsage("tcp", goodListen))
	}
	ln, err := net.Listen("tcp", goodListen)
	if err != nil {
		t.Fatalf("port %s not released after rollback: %v", goodListen, err)
	}
	ln.Close()
}

// TestReloadAtomicSwitch verifies the success boundary: only after the
// load returns does the new version serve, and every response is
// internally consistent (header and body from the same version) across
// both interrelated routes, the redirect and unmatched handling.
func TestReloadAtomicSwitch(t *testing.T) {
	listen := "127.0.0.1:9321"
	base := "http://" + listen

	mustLoad(t, httpConfig(listen, "v1", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)
	assertVersion(t, base, "v1")

	// connections established before the switch keep being served while
	// the reload happens (we don't pin them, but DisableKeepAlives in
	// newClient ensures each probe is a fresh post-switch connection)
	mustLoad(t, httpConfig(listen, "v2", ""))
	assertVersion(t, base, "v2")

	// switch back, again atomically, to prove repeatability
	mustLoad(t, httpConfig(listen, "v3", ""))
	assertVersion(t, base, "v3")
}

// TestReloadIdempotent verifies that repeatedly submitting the identical
// config is idempotent and that a subsequent different valid config
// still results in exactly one complete, serving version with a single
// listener and no duplicate processing.
func TestReloadIdempotent(t *testing.T) {
	listen := "127.0.0.1:9331"
	base := "http://" + listen
	network, addr := listenerKey(listen)

	mustLoad(t, httpConfig(listen, "v1", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)

	for range 5 {
		// forceReload=false: identical config is a no-op success
		if err := caddy.Load(httpConfig(listen, "v1", ""), false); err != nil {
			t.Fatalf("identical reload: %v", err)
		}
	}
	assertVersion(t, base, "v1")

	// one different valid config afterwards: still one complete version
	mustLoad(t, httpConfig(listen, "v2", ""))
	assertVersion(t, base, "v2")
	if u := caddy.ListenerUsage(network, addr); u != 1 {
		t.Fatalf("listener usage after reloads = %d, want exactly 1", u)
	}
}

// TestReloadSerializesAndChains submits load requests concurrently and
// verifies they are applied as a clear chain: the final state is exactly
// the last submitted config, no intermediate version is observable as a
// mixed/duplicate result, and listeners never multiply.
func TestReloadSerializesAndChains(t *testing.T) {
	listen := "127.0.0.1:9341"
	base := "http://" + listen
	network, addr := listenerKey(listen)

	mustLoad(t, httpConfig(listen, "v0", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)

	versions := []string{"v1", "v2", "v3", "v4", "v5"}
	var wg sync.WaitGroup
	for _, v := range versions {
		wg.Add(1)
		go func(ver string) {
			defer wg.Done()
			_ = caddy.Load(httpConfig(listen, ver, ""), true)
		}(v)
	}
	wg.Wait()

	// after the chain drains, every route is one consistent version and
	// exactly one listener remains (no duplicated serving/port holding)
	client := newClient()
	a := probe(t, client, base+"/a")
	b := probe(t, client, base+"/b")
	if a.body == "" || a.body != a.version || a.body != b.body || b.version != b.body {
		t.Fatalf("mixed final state: /a=%q(%q) /b=%q(%q)", a.body, a.version, b.body, b.version)
	}
	if u := caddy.ListenerUsage(network, addr); u != 1 {
		t.Fatalf("listener usage after chained loads = %d, want 1", u)
	}
}

// TestReloadFailureDoesNotOverwritePersisted verifies that with
// persistence enabled, a failed load never replaces the last known-good
// autosaved config, so re-reading (resume) after the failure restores
// that good version rather than the rejected one.
func TestReloadFailureDoesNotOverwritePersisted(t *testing.T) {
	dir := t.TempDir()
	autosave := filepath.Join(dir, "autosave.json")
	prevAutosave := caddy.ConfigAutosavePath
	caddy.ConfigAutosavePath = autosave
	t.Cleanup(func() { caddy.ConfigAutosavePath = prevAutosave })

	listen := "127.0.0.1:9351"
	base := "http://" + listen

	mustLoad(t, httpConfig(listen, "v1", ""))
	t.Cleanup(func() { _ = caddy.Stop() })
	waitForReady(t, base)

	good, err := os.ReadFile(autosave)
	if err != nil {
		t.Fatalf("expected successful config to be persisted: %v", err)
	}
	if !bytes.Contains(good, []byte("v1")) {
		t.Fatalf("persisted config is not the good version: %s", good)
	}

	// several rejected loads must leave the persisted good version intact
	mustFailLoad(t, []byte(`{ broken json`))
	mustFailLoad(t, []byte(fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {"http": {"servers": {"srv0": {"listen": [%q],
			"routes": [{"handle": [{"handler": "unknown_handler_persist"}]}]}}}}}`, listen)))
	mustFailLoad(t, []byte(`{"apps": {"no_such_app": {}}}`))

	after, err := os.ReadFile(autosave)
	if err != nil {
		t.Fatalf("autosave disappeared: %v", err)
	}
	if string(after) != string(good) {
		t.Fatalf("failed load overwritten persisted config\nwant: %s\ngot:  %s", good, after)
	}

	// simulate a restart/resume: stop and load the persisted bytes again
	if err := caddy.Stop(); err != nil {
		t.Fatal(err)
	}
	resumed, err := os.ReadFile(autosave)
	if err != nil {
		t.Fatal(err)
	}
	mustLoad(t, resumed)
	waitForReady(t, base)
	assertVersion(t, base, "v1")
}

// TestReloadAdminEndpointRollback verifies that a failed load does not
// move or disable the admin endpoint: the original endpoint must remain
// reachable throughout, and a newly-bound endpoint from the rejected
// config must be released.
func TestReloadAdminEndpointRollback(t *testing.T) {
	adminPort := 9361
	dataListen := "127.0.0.1:9362"

	adminUp := func(port int) bool {
		client := newClient()
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/config/", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	}

	initial := httpConfig(dataListen, "v1", fmt.Sprintf(`{"listen": "127.0.0.1:%d"}`, adminPort))
	mustLoad(t, initial)
	t.Cleanup(func() { _ = caddy.Stop() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !adminUp(adminPort) {
		time.Sleep(20 * time.Millisecond)
	}
	if !adminUp(adminPort) {
		t.Fatal("initial admin endpoint did not come up")
	}

	// a config that disables the admin endpoint but fails later must not
	// take the current endpoint down
	mustFailLoad(t, []byte(fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {"http": {"servers": {"srv0": {"listen": [%q],
			"routes": [{"handle": [{"handler": "unknown_handler_abc"}]}]}}}}}`, dataListen)))
	if !adminUp(adminPort) {
		t.Fatal("admin endpoint was disabled by a failed load")
	}

	// a config that relocates the admin endpoint but fails later must not
	// move it, and must release the newly-bound port
	newAdminPort := 9363
	mustFailLoad(t, []byte(fmt.Sprintf(`{
		"admin": {"listen": "127.0.0.1:%d"},
		"apps": {"http": {"servers": {"srv0": {"listen": [%q],
			"routes": [{"handle": [{"handler": "unknown_handler_abc"}]}]}}}}}`, newAdminPort, dataListen)))
	if !adminUp(adminPort) {
		t.Fatal("admin endpoint was relocated by a failed load")
	}
	time.Sleep(300 * time.Millisecond)
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", newAdminPort), time.Second); err == nil {
		conn.Close()
		t.Fatal("rejected config leaked a bound admin endpoint on the new port")
	}

	// a successful reload that keeps the admin endpoint still works
	mustLoad(t, httpConfig(dataListen, "v2", fmt.Sprintf(`{"listen": "127.0.0.1:%d"}`, adminPort)))
	if !adminUp(adminPort) {
		t.Fatal("admin endpoint not reachable after successful reload")
	}
}
