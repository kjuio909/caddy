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

package integration

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// txLoadClient is used for raw admin /load requests.
var txLoadClient = &http.Client{Timeout: 10 * time.Second}

// postLoad submits rawConfig to the admin /load endpoint and returns
// the HTTP status code and response body.
func postLoad(t *testing.T, rawConfig, contentType string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"http://localhost:2999/load", strings.NewReader(rawConfig))
	if err != nil {
		t.Fatalf("creating load request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := txLoadClient.Do(req)
	if err != nil {
		t.Fatalf("posting load: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// occupyTestPort binds and keeps a loopback port so a Caddy listener
// on the same address must fail to start.
func occupyTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying port: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// txVersionCaddyfile is a config with several interrelated stages --
// a redirect (/old -> a route), two normal routes (/a, /b), and an
// error response (/e with a 404) -- all carrying the version tag, so
// mixing two versions is directly observable through status codes,
// Location headers and bodies.
func txVersionCaddyfile(tag string) string {
	return fmt.Sprintf(`
{
	admin localhost:2999
	http_port 9080
	grace_period 1ns
}

localhost:9080 {
	redir /old /a 301
	respond /a "%[1]s-a"
	respond /b "%[1]s-b"
	respond /e "%[1]s-e" 404
}
`, tag)
}

// assertVersion checks every stage coherently belongs to tag.
func assertVersion(t *testing.T, tester *caddytest.Tester, tag string) {
	t.Helper()

	tester.AssertGetResponse("http://localhost:9080/a", http.StatusOK, tag+"-a")
	tester.AssertGetResponse("http://localhost:9080/b", http.StatusOK, tag+"-b")

	// the redirect must point at this version's target
	tester.AssertRedirect("http://localhost:9080/old",
		"http://localhost:9080/a", http.StatusMovedPermanently)

	// error responses are part of the version too
	tester.AssertGetResponse("http://localhost:9080/e", http.StatusNotFound, tag+"-e")
}

// assertNotVersion proves that no route of otherTag is observable.
func assertNotVersion(t *testing.T, tester *caddytest.Tester, otherTag string) {
	t.Helper()
	for _, p := range []string{"/a", "/b"} {
		resp, err := tester.Client.Get("http://localhost:9080" + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) == otherTag+"-"+p[1:] {
			t.Fatalf("route %s unexpectedly served the other version: %q (status %d)", p, string(body), resp.StatusCode)
		}
	}
}

// TestTransactionalConfigReload verifies end-to-end through the admin
// /load endpoint that a config reload is an atomic, rollback-capable
// transaction.
func TestTransactionalConfigReload(t *testing.T) {
	tester := caddytest.NewTester(t)

	// ---- establish version 1 ----
	tester.InitServer(txVersionCaddyfile("v1"), "caddyfile")
	assertVersion(t, tester, "v1")

	// ---- every kind of bad input must be rejected while v1 keeps serving ----

	// 1. JSON syntax error
	if status, body := postLoad(t, `{ "apps": {`, "application/json"); status == http.StatusOK {
		t.Fatalf("syntax error load reported success (200): %s", body)
	}
	assertVersion(t, tester, "v1")

	// 2. Caddyfile adapter syntax error
	if status, body := postLoad(t, "localhost:9080 {\n\tnot_a_real_directive_xyz\n}\n",
		"text/caddyfile"); status == http.StatusOK {
		t.Fatalf("caddyfile syntax error load reported success (200): %s", body)
	}
	assertVersion(t, tester, "v1")

	// 3. unknown module
	if status, body := postLoad(t, `{"apps":{"definitely_not_a_real_app_xyz":{}}}`,
		"application/json"); status == http.StatusOK {
		t.Fatalf("unknown-module load reported success (200): %s", body)
	}
	assertVersion(t, tester, "v1")

	// 4. listen address conflict during startup
	busy := occupyTestPort(t)
	conflictCfg := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {"http": {"servers": {"srv": {
		"listen": ["127.0.0.1:%d"],
		"routes": [{"handle": [{"handler": "static_response", "body": "v2-a"}]}]
	}}}}
}`, busy)
	if status, body := postLoad(t, conflictCfg, "application/json"); status == http.StatusOK {
		t.Fatalf("listen-conflict load reported success (200): %s", body)
	}
	assertVersion(t, tester, "v1")

	// the failed attempt must not block a subsequent valid load on the
	// original site port, and v1 results must still be intact
	assertVersion(t, tester, "v1")

	// ---- successful switch: all stages change together ----
	tester.InitServer(txVersionCaddyfile("v2"), "caddyfile")
	assertVersion(t, tester, "v2")
	assertNotVersion(t, tester, "v1")

	// the redirect behavior is part of the switch: point it elsewhere
	// in v3 and make sure /old follows the new version, not the old one
	tester.InitServer(fmt.Sprintf(`
{
	admin localhost:2999
	http_port 9080
	grace_period 1ns
}

localhost:9080 {
	redir /old /b 302
	respond /a "v3-a"
	respond /b "v3-b"
	respond /e "v3-e" 200
}
`), "caddyfile")
	tester.AssertGetResponse("http://localhost:9080/a", http.StatusOK, "v3-a")
	tester.AssertGetResponse("http://localhost:9080/b", http.StatusOK, "v3-b")
	tester.AssertRedirect("http://localhost:9080/old",
		"http://localhost:9080/b", http.StatusFound)
	// error handling changed as a whole: 404 became 200 with the new body
	tester.AssertGetResponse("http://localhost:9080/e", http.StatusOK, "v3-e")

	// ---- idempotent resubmission: the exact running config again ----
	v3Running := fmt.Sprintf(`
{
	admin localhost:2999
	http_port 9080
	grace_period 1ns
}

localhost:9080 {
	redir /old /b 302
	respond /a "v3-a"
	respond /b "v3-b"
	respond /e "v3-e" 200
}
`)
	for i := 0; i < 3; i++ {
		status, body := postLoad(t, v3Running, "text/caddyfile")
		if status != http.StatusOK {
			t.Fatalf("idempotent resubmission %d failed: %d %s", i, status, body)
		}
	}
	tester.AssertGetResponse("http://localhost:9080/a", http.StatusOK, "v3-a")
	tester.AssertRedirect("http://localhost:9080/old",
		"http://localhost:9080/b", http.StatusFound)

	// ---- one more distinct valid load lands as a single version ----
	tester.InitServer(txVersionCaddyfile("v4"), "caddyfile")
	assertVersion(t, tester, "v4")
	assertNotVersion(t, tester, "v3")
}
