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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// resetConfigStateForVersionTests replaces the global config state
// with an empty configuration so version-protection tests start from
// a clean slate. It returns the current token of that empty config.
func resetConfigStateForVersionTests(t *testing.T) string {
	t.Helper()

	ensureFooModuleRegistered()

	rawCfgMu.Lock()
	rawCfg = map[string]any{rawConfigKey: nil}
	rawCfgJSON = nil
	rawCfgIndex = nil
	rawCfgVersion = newConfigVersion()
	version := rawCfgVersion
	rawCfgMu.Unlock()

	t.Cleanup(func() {
		rawCfgMu.Lock()
		rawCfg = map[string]any{rawConfigKey: nil}
		rawCfgJSON = nil
		rawCfgIndex = nil
		rawCfgVersion = newConfigVersion()
		rawCfgMu.Unlock()
	})

	return version
}

// validVersionTestConfig is a full config that provisions cleanly:
// it only contains the already-registered "foo" test app. The admin
// endpoint is disabled because these tests drive a directly-built
// handler rather than a network listener.
func validVersionTestConfig(intField int) []byte {
	return []byte(fmt.Sprintf(
		`{"admin": {"disabled": true}, "apps": {"foo": {"strField": "abc", "intField": %d}}}`,
		intField))
}

// ensureFooModuleRegistered registers the no-op foo test app if some
// other test has not already done so.
func ensureFooModuleRegistered() {
	if _, err := GetModule("foo"); err != nil {
		RegisterModule(fooModule{})
	}
}

// configHandlerForVersionTests builds an admin handler backed by the
// real package-level config state, so handleConfig exercises the same
// locking and version logic as a live admin endpoint.
func configHandlerForVersionTests(t *testing.T) http.Handler {
	t.Helper()
	cfg := &Config{Admin: &AdminConfig{Listen: "localhost:2019"}}
	addr, err := ParseNetworkAddress("localhost:2019")
	if err != nil {
		t.Fatalf("parse address: %v", err)
	}
	h, err := cfg.Admin.newAdminHandler(addr, false, Context{})
	if err != nil {
		t.Fatalf("create admin handler: %v", err)
	}
	return h
}

// readFullConfigViaHandler issues GET /config/ and returns the decoded
// config body together with the version token returned by the server.
func readFullConfigViaHandler(t *testing.T, h http.Handler) (string, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+rawConfigKey+"/", nil)
	req.Host = "localhost:2019"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(rr.Body.Bytes()), &out); err != nil {
		t.Fatalf("decoding config body %q: %v", rr.Body.String(), err)
	}
	version := rr.Header().Get(ConfigVersionHeader)
	if version == "" {
		t.Fatal("GET /config/: response is missing the version token header")
	}
	return version, out
}

// mutateViaHandler issues a mutating request under /config/ with the
// given version token and returns the recorded response.
func mutateViaHandler(t *testing.T, h http.Handler, method, subPath, version string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "/"+rawConfigKey+subPath, reader)
	req.Host = "localhost:2019"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if version != "" {
		req.Header.Set(ConfigVersionHeader, version)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// apiStatus is a small helper asserting the response carries our
// structured API error with the expected HTTP status.
func apiStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("expected status %d, got %d: %s", want, rr.Code, rr.Body.String())
	}
}

// TestConfigVersionReadReturnsToken ensures every read surfaces an
// opaque token that is valid for exactly the config that was read.
func TestConfigVersionReadReturnsToken(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	v1, body1 := readFullConfigViaHandler(t, h)
	if body1 != nil && len(body1) != 0 {
		t.Fatalf("expected initially empty config, got %#v", body1)
	}

	// two reads of the same config return the same token
	v2, _ := readFullConfigViaHandler(t, h)
	if v1 != v2 {
		t.Fatalf("expected identical reads to return the same token; got %q then %q", v1, v2)
	}
}

// TestConfigVersionFullReplaceThenStaleSubtreeMutation covers the
// headline scenario: after a full replacement succeeds with one
// caller's token, a second caller holding the old token must fail
// every form of subtree mutation with 412, while the fresh token
// continues to work.
func TestConfigVersionFullReplaceThenStaleSubtreeMutation(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	oldVersion, _ := readFullConfigViaHandler(t, h)

	// caller A replaces the whole config
	rr := mutateViaHandler(t, h, http.MethodPost, "/", oldVersion, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)
	newVersion := rr.Header().Get(ConfigVersionHeader)
	if newVersion == "" || newVersion == oldVersion {
		t.Fatalf("full replacement must return a new, non-empty token; got %q (old %q)", newVersion, oldVersion)
	}

	// caller B still holds the old token: POST/PUT/PATCH/DELETE on a
	// subtree must all fail the precondition and change nothing
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		var body []byte
		sub := "/apps/foo/intField"
		if method == http.MethodPost {
			body = []byte(`7`)
		} else if method == http.MethodPut || method == http.MethodPatch {
			body = []byte(`7`)
		}
		rr := mutateViaHandler(t, h, method, sub, oldVersion, body)
		apiStatus(t, rr, http.StatusPreconditionFailed)
	}

	// a read after the failed attempts shows the successful config and token
	readVersion, readBody := readFullConfigViaHandler(t, h)
	if readVersion != newVersion {
		t.Fatalf("stale mutations must not change the token; got %q, want %q", readVersion, newVersion)
	}
	apps, _ := readBody["apps"].(map[string]any)
	foo, _ := apps["foo"].(map[string]any)
	if foo["intField"].(float64) != 1 {
		t.Fatalf("stale mutations changed the config: %#v", readBody)
	}

	// the fresh token allows a subtree replacement
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", newVersion, []byte(`2`))
	apiStatus(t, rr, http.StatusOK)
	if rr.Header().Get(ConfigVersionHeader) == "" || rr.Header().Get(ConfigVersionHeader) == newVersion {
		t.Fatal("successful subtree mutation must return a new token")
	}
}

// TestConfigVersionSubtreeMutationThenStaleFullReplace is the reverse
// direction: after a subtree change advances the token, a full replace
// sent with the pre-subtree token must fail.
func TestConfigVersionSubtreeMutationThenStaleFullReplace(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	v0, _ := readFullConfigViaHandler(t, h)

	// establish a full config
	rr := mutateViaHandler(t, h, http.MethodPost, "/", v0, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)
	v1 := rr.Header().Get(ConfigVersionHeader)

	// subtree mutation advances the token
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", v1, []byte(`3`))
	apiStatus(t, rr, http.StatusOK)
	v2 := rr.Header().Get(ConfigVersionHeader)
	if v2 == v1 {
		t.Fatal("subtree mutation must advance the token")
	}

	// retrying the full replace with v1 must now fail the precondition
	rr = mutateViaHandler(t, h, http.MethodPost, "/", v1, validVersionTestConfig(9))
	apiStatus(t, rr, http.StatusPreconditionFailed)

	// config still reflects the subtree mutation, not the rejected replace
	version, body := readFullConfigViaHandler(t, h)
	if version != v2 {
		t.Fatalf("rejected replace must leave the token unchanged; got %q, want %q", version, v2)
	}
	apps, _ := body["apps"].(map[string]any)
	foo, _ := apps["foo"].(map[string]any)
	if foo["intField"].(float64) != 3 {
		t.Fatalf("rejected replace must not alter the config: %#v", body)
	}
}

// TestConfigVersionMissingTokenRejected ensures mutations without a
// token fail before the body is even considered, while reads remain
// open (they are how the token is obtained).
func TestConfigVersionMissingTokenRejected(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	version, _ := readFullConfigViaHandler(t, h)

	// seed a config so subsequent inputs would otherwise be valid
	rr := mutateViaHandler(t, h, http.MethodPost, "/", version, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		var body []byte
		if method != http.MethodDelete {
			body = []byte(`1`)
		}
		rr := mutateViaHandler(t, h, method, "/apps/foo/intField", "", body)
		apiStatus(t, rr, http.StatusPreconditionFailed)
	}

	// reads continue to work without a token
	req := httptest.NewRequest(http.MethodGet, "/"+rawConfigKey+"/", nil)
	req.Host = "localhost:2019"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reads without a token must still succeed, got %d", rec.Code)
	}
}

// TestConfigVersionUnrecognizedTokenRejected covers tokens that never
// belonged to this configuration (as opposed to a token that was once
// valid but is now stale; both must be 412).
func TestConfigVersionUnrecognizedTokenRejected(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	version, _ := readFullConfigViaHandler(t, h)
	rr := mutateViaHandler(t, h, http.MethodPost, "/", version, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)

	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", "definitely-not-a-real-token", []byte(`4`))
	apiStatus(t, rr, http.StatusPreconditionFailed)
}

// TestConfigVersionIdempotentRetry submits the same request twice with
// the same, still-current token. The second submission is a no-op
// retry: no second reload side effect, success status, and the same
// token remains usable afterwards.
func TestConfigVersionIdempotentRetry(t *testing.T) {
	resetConfigStateForVersionTests(t)

	// use the core function directly so errSameConfig is observable;
	// the handler folds that sentinel into a 200 response
	v0 := currentConfigVersion()
	v1, err := changeConfig(http.MethodPost, "/"+rawConfigKey, validVersionTestConfig(1), "", v0, false)
	if err != nil {
		t.Fatalf("initial replace: %v", err)
	}
	if v1 == v0 {
		t.Fatal("successful replace must mint a new token")
	}

	// identical retry against the same token: errSameConfig, same token
	vRetry, err := changeConfig(http.MethodPost, "/"+rawConfigKey, validVersionTestConfig(1), "", v1, false)
	if err != errSameConfig {
		t.Fatalf("identical retry should return errSameConfig, got %v", err)
	}
	if vRetry != v1 {
		t.Fatalf("idempotent retry must keep the same token; got %q, want %q", vRetry, v1)
	}

	// the token is still usable for a real follow-up change
	v2, err := changeConfig(http.MethodPatch, "/"+rawConfigKey+"/apps/foo/intField", []byte(`5`), "", v1, false)
	if err != nil {
		t.Fatalf("follow-up change after retry: %v", err)
	}
	if v2 == v1 {
		t.Fatal("a real change after a no-op retry must advance the token")
	}

	// and at the HTTP layer the same retry pair is 200 both times with
	// a stable token
	h := configHandlerForVersionTests(t)
	rr := mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", v2, []byte(`5`))
	apiStatus(t, rr, http.StatusOK)
	if got := rr.Header().Get(ConfigVersionHeader); got != v2 {
		t.Fatalf("HTTP idempotent retry should return the same token %q, got %q", v2, got)
	}
}

// TestConfigVersionInvalidCandidateKeepsOldState drives malformed and
// invalid candidates through the full prepare pipeline and asserts the
// old config, old token, and running apps are untouched on failure.
func TestConfigVersionInvalidCandidateKeepsOldState(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	version, _ := readFullConfigViaHandler(t, h)

	// establish a valid config
	rr := mutateViaHandler(t, h, http.MethodPost, "/", version, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)
	goodVersion := rr.Header().Get(ConfigVersionHeader)

	// malformed JSON body: 400 input error
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", goodVersion, []byte(`{not json`))
	apiStatus(t, rr, http.StatusBadRequest)

	// candidate that fails to provision (unknown app at the root):
	// 400, and distinct from the 412 a stale token would produce
	rr = mutateViaHandler(t, h, http.MethodPost, "/", goodVersion,
		[]byte(`{"admin": {"disabled": true}, "apps": {"bogus_app": {}}}`))
	apiStatus(t, rr, http.StatusBadRequest)

	// the old token is still valid and the config is unchanged
	readVersion, body := readFullConfigViaHandler(t, h)
	if readVersion != goodVersion {
		t.Fatalf("failed candidate must keep the old token; got %q, want %q", readVersion, goodVersion)
	}
	apps, _ := body["apps"].(map[string]any)
	if _, ok := apps["bogus_app"]; ok {
		t.Fatalf("failed candidate leaked a partial subtree: %#v", body)
	}
	foo, _ := apps["foo"].(map[string]any)
	if foo["intField"].(float64) != 1 {
		t.Fatalf("failed candidate altered the existing config: %#v", body)
	}

	// the old token still authorizes a successful change
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", goodVersion, []byte(`6`))
	apiStatus(t, rr, http.StatusOK)
}

// TestConfigVersionInputErrorsPreserveState covers the edge cases called
// out in the requirements: nonexistent paths, type mismatches, and
// deleting the root configuration.
func TestConfigVersionInputErrorsPreserveState(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	version, _ := readFullConfigViaHandler(t, h)
	rr := mutateViaHandler(t, h, http.MethodPost, "/", version, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)
	version = rr.Header().Get(ConfigVersionHeader)

	// PATCH a nonexistent key: 404
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/nope", version, []byte(`1`))
	apiStatus(t, rr, http.StatusNotFound)

	// traverse through a scalar as if it were an object: 400 type mismatch
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField/child", version, []byte(`1`))
	apiStatus(t, rr, http.StatusBadRequest)

	// traversal through a missing intermediate object: 400
	rr = mutateViaHandler(t, h, http.MethodPatch, "/apps/missing/child", version, []byte(`1`))
	apiStatus(t, rr, http.StatusBadRequest)

	// deleting the whole config: 400 input error
	rr = mutateViaHandler(t, h, http.MethodDelete, "/", version, nil)
	apiStatus(t, rr, http.StatusBadRequest)

	// nothing changed: token and config stay the same
	readVersion, body := readFullConfigViaHandler(t, h)
	if readVersion != version {
		t.Fatalf("input errors must not advance the token; got %q, want %q", readVersion, version)
	}
	apps, _ := body["apps"].(map[string]any)
	foo, _ := apps["foo"].(map[string]any)
	if foo["intField"].(float64) != 1 {
		t.Fatalf("input errors altered the config: %#v", body)
	}
}

// TestConfigVersionIdenticalContentsGetDifferentTokens verifies the
// token is not derivable from the config body: two consecutive versions
// with byte-identical effective contents (forced reload) must carry
// different tokens.
func TestConfigVersionIdenticalContentsGetDifferentTokens(t *testing.T) {
	resetConfigStateForVersionTests(t)

	v0 := currentConfigVersion()
	cfg := validVersionTestConfig(1)

	v1, err := changeConfig(http.MethodPost, "/"+rawConfigKey, cfg, "", v0, true)
	if err != nil {
		t.Fatalf("first replace: %v", err)
	}
	v2, err := changeConfig(http.MethodPost, "/"+rawConfigKey, cfg, "", v1, true)
	if err != nil {
		t.Fatalf("second identical replace: %v", err)
	}
	if v0 == v1 || v1 == v2 {
		t.Fatalf("identical contents must still produce distinct tokens: %q %q %q", v0, v1, v2)
	}

	// contents are in fact identical
	var a, b map[string]any
	if err := json.Unmarshal(cfg, &a); err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	if err := readConfig("/"+rawConfigKey, buf); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &b); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) {
		t.Fatalf("expected identical contents, got %v vs %v", a, b)
	}
}

// TestConfigVersionConcurrentSubmissions verifies that under concurrent
// submissions with the same starting token, exactly one wins (the order
// being whichever acquires and completes the version check first), all
// losers observe 412, and an older token can never become valid again.
func TestConfigVersionConcurrentSubmissions(t *testing.T) {
	resetConfigStateForVersionTests(t)

	v0 := currentConfigVersion()
	// seed with a value that no concurrent request will submit, so
	// every competing request is a real change (not an idempotent
	// no-op) and exactly one can win
	if _, err := changeConfig(http.MethodPost, "/"+rawConfigKey, validVersionTestConfig(-1), "", v0, false); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	vStart := currentConfigVersion()

	const n = 25
	var wg sync.WaitGroup
	statuses := make([]int, n)
	versions := make([]string, n)

	h := configHandlerForVersionTests(t)
	for i := 0; i < n; i++ {
		i := i
		wg.Go(func() {
			body := []byte(fmt.Sprintf(`%d`, i+1))
			rr := mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", vStart, body)
			statuses[i] = rr.Code
			versions[i] = rr.Header().Get(ConfigVersionHeader)
		})
	}
	wg.Wait()

	winners, losers := 0, 0
	winnerVersion := ""
	for i, code := range statuses {
		switch code {
		case http.StatusOK:
			winners++
			winnerVersion = versions[i]
		case http.StatusPreconditionFailed:
			losers++
		default:
			t.Fatalf("request %d got unexpected status %d", i, code)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one concurrent submission to win, got %d winners and %d losers", winners, losers)
	}
	if losers != n-1 {
		t.Fatalf("expected %d losers, got %d", n-1, losers)
	}

	// the winner's token is now current; the start token is permanently stale
	current := currentConfigVersion()
	if current != winnerVersion {
		t.Fatalf("current token %q does not match winning token %q", current, winnerVersion)
	}
	if vStart == current {
		t.Fatal("a successful concurrent commit must advance the token")
	}

	// the old token cannot become valid again by retrying
	rr := mutateViaHandler(t, h, http.MethodPatch, "/apps/foo/intField", vStart, []byte(`123`))
	apiStatus(t, rr, http.StatusPreconditionFailed)
}

// TestConfigVersionSuccessfulReadObservesNewConfigAndToken is an
// end-to-end read-after-write check across both a full replace and a
// subtree mutation.
func TestConfigVersionSuccessfulReadObservesNewConfigAndToken(t *testing.T) {
	resetConfigStateForVersionTests(t)
	h := configHandlerForVersionTests(t)

	v0, _ := readFullConfigViaHandler(t, h)

	rr := mutateViaHandler(t, h, http.MethodPost, "/", v0, validVersionTestConfig(1))
	apiStatus(t, rr, http.StatusOK)
	v1, body := readFullConfigViaHandler(t, h)
	if v1 == v0 {
		t.Fatal("read after full replace should observe the new token")
	}
	apps, _ := body["apps"].(map[string]any)
	foo, _ := apps["foo"].(map[string]any)
	if foo["intField"].(float64) != 1 {
		t.Fatalf("read after full replace should observe the new config, got %#v", body)
	}

	rr = mutateViaHandler(t, h, http.MethodPost, "/apps/foo/strField", v1, []byte(`"xyz"`))
	apiStatus(t, rr, http.StatusOK)
	v2, body := readFullConfigViaHandler(t, h)
	if v2 == v1 {
		t.Fatal("read after subtree mutation should observe the new token")
	}
	apps, _ = body["apps"].(map[string]any)
	foo, _ = apps["foo"].(map[string]any)
	if foo["strField"] != "xyz" {
		t.Fatalf("read after subtree mutation should observe the new value, got %#v", body)
	}
}
