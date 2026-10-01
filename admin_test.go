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
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var testCfg = []byte(`{
			"apps": {
				"http": {
					"servers": {
						"myserver": {
							"listen": ["tcp/localhost:8080-8084"],
							"read_timeout": "30s"
						},
						"yourserver": {
							"listen": ["127.0.0.1:5000"],
							"read_header_timeout": "15s"
						}
					}
				}
			}
		}
		`)

type testAdminPublicKey string

func (k testAdminPublicKey) Equal(x crypto.PublicKey) bool {
	other, ok := x.(testAdminPublicKey)
	return ok && k == other
}
func TestUnsyncedConfigAccess(t *testing.T) {
	// each test is performed in sequence, so
	// each change builds on the previous ones;
	// the config is not reset between tests
	for i, tc := range []struct {
		method    string
		path      string // rawConfigKey will be prepended
		payload   string
		expect    string // JSON representation of what the whole config is expected to be after the request
		shouldErr bool
	}{
		{
			method:  "POST",
			path:    "",
			payload: `{"foo": "bar", "list": ["a", "b", "c"]}`, // starting value
			expect:  `{"foo": "bar", "list": ["a", "b", "c"]}`,
		},
		{
			method:  "POST",
			path:    "/foo",
			payload: `"jet"`,
			expect:  `{"foo": "jet", "list": ["a", "b", "c"]}`,
		},
		{
			method:  "POST",
			path:    "/bar",
			payload: `{"aa": "bb", "qq": "zz"}`,
			expect:  `{"foo": "jet", "bar": {"aa": "bb", "qq": "zz"}, "list": ["a", "b", "c"]}`,
		},
		{
			method: "DELETE",
			path:   "/bar/qq",
			expect: `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c"]}`,
		},
		{
			method:    "DELETE",
			path:      "/bar/qq",
			expect:    `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c"]}`,
			shouldErr: true,
		},
		{
			method:  "POST",
			path:    "/list",
			payload: `"e"`,
			expect:  `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c", "e"]}`,
		},
		{
			method:  "PUT",
			path:    "/list/3",
			payload: `"d"`,
			expect:  `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c", "d", "e"]}`,
		},
		{
			method: "DELETE",
			path:   "/list/3",
			expect: `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c", "e"]}`,
		},
		{
			method:  "PATCH",
			path:    "/list/3",
			payload: `"d"`,
			expect:  `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c", "d"]}`,
		},
		{
			method:  "POST",
			path:    "/list/...",
			payload: `["e", "f", "g"]`,
			expect:  `{"foo": "jet", "bar": {"aa": "bb"}, "list": ["a", "b", "c", "d", "e", "f", "g"]}`,
		},
	} {
		err := unsyncedConfigAccess(tc.method, rawConfigKey+tc.path, []byte(tc.payload), nil)

		if tc.shouldErr && err == nil {
			t.Fatalf("Test %d: Expected error return value, but got: %v", i, err)
		}
		if !tc.shouldErr && err != nil {
			t.Fatalf("Test %d: Should not have had error return value, but got: %v", i, err)
		}

		// decode the expected config so we can do a convenient DeepEqual
		var expectedDecoded any
		err = json.Unmarshal([]byte(tc.expect), &expectedDecoded)
		if err != nil {
			t.Fatalf("Test %d: Unmarshaling expected config: %v", i, err)
		}

		// make sure the resulting config is as we expect it
		if !reflect.DeepEqual(rawCfg[rawConfigKey], expectedDecoded) {
			t.Fatalf("Test %d:\nExpected:\n\t%#v\nActual:\n\t%#v",
				i, expectedDecoded, rawCfg[rawConfigKey])
		}
	}
}

// TestLoadConcurrent exercises Load under concurrent conditions
// and is most useful under test with `-race` enabled.
func TestLoadConcurrent(t *testing.T) {
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Go(func() {
			_ = Load(testCfg, true)
		})
	}
	wg.Wait()
}

// fooModule is a minimal app used by config-version tests as a legal
// subtree to mutate; it accepts arbitrary JSON and has no runtime
// side effects.
type fooModule struct{}

func (fooModule) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "foo",
		New: func() Module { return new(fooModule) },
	}
}
func (fooModule) Start() error { return nil }
func (fooModule) Stop() error  { return nil }

// UnmarshalJSON accepts anything so tests can add arbitrary subtrees
// under apps.foo without failing strict config decoding.
func (*fooModule) UnmarshalJSON([]byte) error { return nil }

var fooModuleRegistered sync.Once

func TestConfigVersionTokens(t *testing.T) {
	fooModuleRegistered.Do(func() { RegisterModule(fooModule{}) })

	// admin endpoint is disabled so commits do not bind a real listener;
	// successful changes still run the whole prepare/provision pipeline
	const baseConfig = `{"admin":{"disabled":true},"apps":{"foo":{}}}`
	const rootPath = "/" + rawConfigKey + "/"
	const fooPath = "/" + rawConfigKey + "/apps/foo"

	if err := Load([]byte(baseConfig), true); err != nil {
		t.Fatalf("loading base config: %s", err)
	}
	t.Cleanup(func() { _ = Stop() })

	readRoot := func(t *testing.T) (string, []byte) {
		t.Helper()
		var buf bytes.Buffer
		token, err := readConfig(rootPath, &buf)
		if err != nil {
			t.Fatalf("reading config: %s", err)
		}
		if token == "" {
			t.Fatal("expected non-empty version token on read")
		}
		return token, buf.Bytes()
	}

	expectPrecondition := func(t *testing.T, err error) {
		t.Helper()
		var apiErr APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected APIError, got %T: %v", err, err)
		}
		if apiErr.HTTPStatus != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 precondition failed, got %d: %v", apiErr.HTTPStatus, err)
		}
	}

	expectStatus := func(t *testing.T, want int, err error) {
		t.Helper()
		var apiErr APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected APIError with status %d, got %T: %v", want, err, err)
		}
		if apiErr.HTTPStatus != want {
			t.Fatalf("expected status %d, got %d: %v", want, apiErr.HTTPStatus, err)
		}
	}

	t1, body1 := readRoot(t)

	// the token must be opaque: it cannot be derivable from the body
	if bytes.Contains(body1, []byte(t1)) {
		t.Fatal("version token appears in the config body")
	}

	// a token that never existed is a failed precondition; the
	// check runs before path traversal, so the nonexistent path
	// here is irrelevant
	_, err := changeConfig(http.MethodPost, rootPath+"marker", []byte(`"a"`), "00000000-0000-0000-0000-000000000000", false)
	expectPrecondition(t, err)

	// successful subtree creation rotates the token ...
	t2, err := changeConfig(http.MethodPut, fooPath+"/marker", []byte(`{"nested":"v1"}`), t1, false)
	if err != nil {
		t.Fatalf("creating subtree: %v", err)
	}
	if t2 == "" || t2 == t1 {
		t.Fatalf("expected new token after successful change, got %q (old %q)", t2, t1)
	}

	// ... and a read observes the new content together with the new token
	gotToken, gotBody := readRoot(t)
	if gotToken != t2 {
		t.Fatalf("read token %q does not match committed token %q", gotToken, t2)
	}
	if !bytes.Contains(gotBody, []byte(`"nested":"v1"`)) {
		t.Fatalf("new content not visible after commit: %s", gotBody)
	}

	// after one successful whole-config replacement, another caller
	// still holding the old token cannot modify a subtree
	t3, err := changeConfig(http.MethodPost, rootPath, []byte(baseConfig), t2, true)
	if err != nil {
		t.Fatalf("replacing whole config: %v", err)
	}
	if t3 == t2 {
		t.Fatal("expected token to rotate on whole-config replacement")
	}
	_, err = changeConfig(http.MethodPatch, fooPath+"/marker/nested", []byte(`"stale"`), t2, false)
	expectPrecondition(t, err)

	// and vice versa: a subtree change rotates the token, after which
	// the previous token cannot drive a whole replacement
	t4, err := changeConfig(http.MethodPost, fooPath+"/marker2", []byte(`1`), t3, false)
	if err != nil {
		t.Fatalf("subtree change: %v", err)
	}
	_, err = changeConfig(http.MethodPost, rootPath, []byte(baseConfig), t3, false)
	expectPrecondition(t, err)

	// identical content committed as a new version (force reload) still
	// gets a fresh token: tokens are never derived from content
	t5, err := changeConfig(http.MethodPost, rootPath, []byte(baseConfig), t4, true)
	if err != nil {
		t.Fatalf("force reload of identical config: %v", err)
	}
	if t5 == t4 {
		t.Fatal("identical consecutive versions must not share a token")
	}
	_, bodyAfterSame := readRoot(t)
	if !bytes.Equal(bytes.TrimSpace(bodyAfterSame), []byte(baseConfig)) {
		t.Fatalf("expected identical config, got %s", bodyAfterSame)
	}

	// resubmitting an unchanged write against the current token is a
	// no-op: errSameConfig, the same token, and no second side effect
	t5m, err := changeConfig(http.MethodPost, fooPath+"/marker2", []byte(`1`), t5, false)
	if err != nil {
		t.Fatalf("creating value for idempotent retry: %v", err)
	}
	sameToken, err := changeConfig(http.MethodPost, fooPath+"/marker2", []byte(`1`), t5m, false)
	if !errors.Is(err, errSameConfig) {
		t.Fatalf("expected errSameConfig on retry, got %v", err)
	}
	if sameToken != t5m {
		t.Fatalf("retry must not rotate token: got %q, want %q", sameToken, t5m)
	}

	// malformed candidate JSON: input error, state and token untouched
	beforeTok, beforeBody := readRoot(t)
	if beforeTok != t5m {
		t.Fatalf("setup: expected token %q, got %q", t5m, beforeTok)
	}
	_, err = changeConfig(http.MethodPost, fooPath+"/broken", []byte(`{not valid json`), t5m, false)
	expectStatus(t, http.StatusBadRequest, err)
	afterTok, afterBody := readRoot(t)
	if afterTok != beforeTok || !bytes.Equal(afterBody, beforeBody) {
		t.Fatal("malformed JSON changed the configuration")
	}

	// candidate that fails runtime preparation (unknown app module):
	// the running config, raw state and token must all survive
	_, err = changeConfig(http.MethodPost, rootPath,
		[]byte(`{"admin":{"disabled":true},"apps":{"definitely_not_a_module_xyz":{}}}`), t5m, false)
	if err == nil {
		t.Fatal("expected error loading config with unknown app module")
	}
	afterTok, afterBody = readRoot(t)
	if afterTok != beforeTok {
		t.Fatalf("token changed after failed reload: got %q, want %q", afterTok, beforeTok)
	}
	if !bytes.Equal(afterBody, beforeBody) {
		t.Fatalf("config changed after failed reload:\n%s\nwant\n%s", afterBody, beforeBody)
	}

	// the still-current token remains usable after those failures
	t6, err := changeConfig(http.MethodPost, fooPath+"/recovered", []byte(`true`), t5m, false)
	if err != nil {
		t.Fatalf("change after failed candidates should succeed with same token: %v", err)
	}

	// boundary: deleting the root configuration is an input error
	_, err = changeConfig(http.MethodDelete, rootPath, nil, t6, false)
	expectStatus(t, http.StatusBadRequest, err)
	_, err = changeConfig(http.MethodDelete, "/"+rawConfigKey, nil, t6, false)
	expectStatus(t, http.StatusBadRequest, err)

	// boundary: PATCHing through a scalar parent is a bad request
	_, err = changeConfig(http.MethodPatch, fooPath+"/recovered/does/not/exist", []byte(`1`), t6, false)
	expectStatus(t, http.StatusBadRequest, err)
	t7, err := changeConfig(http.MethodPut, fooPath+"/typed", []byte(`{"a":1}`), t6, false)
	if err != nil {
		t.Fatalf("creating typed subtree: %v", err)
	}
	// PUT to an existing key is a conflict, PATCH a missing map key is
	// a not-found, and traversing through an existing scalar is a bad
	// request -- all distinguishable input errors, all non-mutating
	_, err = changeConfig(http.MethodPut, fooPath+"/typed", []byte(`{"a":2}`), t7, false)
	expectStatus(t, http.StatusConflict, err)
	_, err = changeConfig(http.MethodPatch, fooPath+"/typed/missing", []byte(`1`), t7, false)
	expectStatus(t, http.StatusNotFound, err)
	_, err = changeConfig(http.MethodPut, fooPath+"/typed/a/deeper", []byte(`1`), t7, false)
	expectStatus(t, http.StatusBadRequest, err)
	t8, err := changeConfig(http.MethodPost, fooPath+"/nums", []byte(`[1,2,3]`), t7, false)
	if err != nil {
		t.Fatalf("creating array: %v", err)
	}
	_, err = changeConfig(http.MethodDelete, fooPath+"/nums/9", nil, t8, false)
	expectStatus(t, http.StatusBadRequest, err)

	// none of the failed boundary operations moved the state
	if tok, body := readRoot(t); tok != t8 || !bytes.Contains(body, []byte(`"nums"`)) {
		t.Fatalf("state changed after failed boundary operations: token %q body %s", tok, body)
	}

	// concurrency: many callers submit with the same token; exactly one
	// wins and commits, every other submission fails the precondition,
	// and a final read shows one consistent config with one token
	const n = 50
	var winners, losers int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, err := changeConfig(http.MethodPost, rootPath, []byte(baseConfig), t8, true)
			switch {
			case err == nil:
				atomic.AddInt64(&winners, 1)
			case errors.Is(err, errConfigVersionMismatch):
				atomic.AddInt64(&losers, 1)
			default:
				t.Errorf("unexpected concurrent result: %v", err)
			}
		})
	}
	wg.Wait()
	if winners != 1 || losers != n-1 {
		t.Fatalf("expected exactly 1 winner and %d losers, got %d winners and %d losers", n-1, winners, losers)
	}
	finalTok, _ := readRoot(t)
	if finalTok == t8 {
		t.Fatal("winning commit did not rotate the token")
	}
	// the old token is definitively dead after the serialized race
	_, err = changeConfig(http.MethodPost, rootPath+"late", []byte(`1`), t8, false)
	expectPrecondition(t, err)
}

func BenchmarkLoad(b *testing.B) {
	for b.Loop() {
		Load(testCfg, true)
	}
}

// TestHandleConfigVersionHeader exercises the version token over the
// HTTP boundary of the /config/ endpoint: a read hands out the token,
// writes must echo it, and failures leave the token in place.
func TestHandleConfigVersionHeader(t *testing.T) {
	fooModuleRegistered.Do(func() { RegisterModule(fooModule{}) })

	const configPath = "/" + rawConfigKey + "/apps/foo"
	if err := Load([]byte(`{"admin":{"disabled":true},"apps":{"foo":{}}}`), true); err != nil {
		t.Fatalf("loading config: %s", err)
	}
	t.Cleanup(func() { _ = Stop() })

	// build a real admin mux so APIError responses are rendered with
	// their status codes, as the wrapper around handleConfig does
	addr, err := ParseNetworkAddress("localhost:2019")
	if err != nil {
		t.Fatalf("parsing admin address: %v", err)
	}
	adminHandler, err := (&AdminConfig{Listen: "localhost:2019"}).newAdminHandler(addr, false, Context{})
	if err != nil {
		t.Fatalf("creating admin handler: %v", err)
	}

	doRequest := func(t *testing.T, method, path, token string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		var r *http.Request
		if body != nil {
			r = httptest.NewRequest(method, "http://localhost:2019"+path, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, "http://localhost:2019"+path, nil)
		}
		if token != "" {
			r.Header.Set(ConfigVersionHeader, token)
		}
		rr := httptest.NewRecorder()
		adminHandler.ServeHTTP(rr, r)
		return rr
	}

	// a read includes the opaque token
	rr := doRequest(t, http.MethodGet, configPath, "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on read, got %d: %s", rr.Code, rr.Body.String())
	}
	v1 := rr.Header().Get(ConfigVersionHeader)
	if v1 == "" {
		t.Fatal("read response is missing the version token header")
	}

	// writes without a token fail the precondition before any content
	// is considered
	rr = doRequest(t, http.MethodPost, configPath+"/a", "", []byte(`1`))
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 without token, got %d", rr.Code)
	}

	// an unknown token fails too
	rr = doRequest(t, http.MethodPost, configPath+"/a", "not-a-real-token", []byte(`1`))
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 with unknown token, got %d", rr.Code)
	}

	// a precondition failure must not move the token
	rr = doRequest(t, http.MethodGet, configPath, "", nil)
	if got := rr.Header().Get(ConfigVersionHeader); got != v1 {
		t.Fatalf("token changed after failed writes: got %q, want %q", got, v1)
	}

	// a write with the current token succeeds and returns a new token
	rr = doRequest(t, http.MethodPost, configPath+"/a", v1, []byte(`1`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on valid write, got %d: %s", rr.Code, rr.Body.String())
	}
	v2 := rr.Header().Get(ConfigVersionHeader)
	if v2 == "" || v2 == v1 {
		t.Fatalf("expected rotated token on success, got %q (old %q)", v2, v1)
	}

	// the next read shows both the new content and the new token
	rr = doRequest(t, http.MethodGet, configPath, "", nil)
	if got := rr.Header().Get(ConfigVersionHeader); got != v2 {
		t.Fatalf("read token %q does not match write token %q", got, v2)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"a":1`)) {
		t.Fatalf("committed content not visible: %s", rr.Body.String())
	}

	// the old token can no longer drive a write
	rr = doRequest(t, http.MethodDelete, configPath+"/a", v1, nil)
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 with stale token, got %d", rr.Code)
	}

	// deleting the root config is an input error, not a precondition
	rr = doRequest(t, http.MethodDelete, "/"+rawConfigKey+"/", v2, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 deleting root config, got %d", rr.Code)
	}

	// malformed JSON is a content error (400), distinguishable from 412
	rr = doRequest(t, http.MethodPost, configPath+"/b", v2, []byte(`{bad`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on malformed JSON, got %d", rr.Code)
	}
	rr = doRequest(t, http.MethodGet, configPath, "", nil)
	if got := rr.Header().Get(ConfigVersionHeader); got != v2 {
		t.Fatalf("token changed after a 400 content error: got %q, want %q", got, v2)
	}
}

func TestAdminHandlerErrorHandling(t *testing.T) {
	initAdminMetrics()

	handler := adminHandler{
		mux: http.NewServeMux(),
	}

	handler.mux.Handle("/error", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := fmt.Errorf("test error")
		handler.handleError(w, r, err)
	}))

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Error("expected error response, got success")
	}

	var apiErr APIError
	if err := json.NewDecoder(rr.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if apiErr.Message != "test error" {
		t.Errorf("expected error message 'test error', got '%s'", apiErr.Message)
	}
}

func TestAdminHandlerServeHTTPRedactsSensitiveHeadersInLogs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)

	defaultLoggerMu.Lock()
	origLogger := defaultLogger.logger
	defaultLogger.logger = zap.New(core)
	defaultLoggerMu.Unlock()
	t.Cleanup(func() {
		defaultLoggerMu.Lock()
		defaultLogger.logger = origLogger
		defaultLoggerMu.Unlock()
	})

	handler := adminHandler{
		mux: http.NewServeMux(),
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Test", "ok")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if logs.Len() == 0 {
		t.Fatal("expected request log entry")
	}

	ctx := logs.All()[0].ContextMap()
	headers, ok := ctx["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected headers field in log context, got %T", ctx["headers"])
	}

	if got := headers["Authorization"]; !reflect.DeepEqual(got, []any{"REDACTED"}) {
		t.Fatalf("expected redacted Authorization header, got %#v", got)
	}
	if got := headers["Cookie"]; !reflect.DeepEqual(got, []any{"REDACTED"}) {
		t.Fatalf("expected redacted Cookie header, got %#v", got)
	}
	if got := headers["X-Test"]; !reflect.DeepEqual(got, []any{"ok"}) {
		t.Fatalf("expected X-Test header to remain visible, got %#v", got)
	}
}

func initAdminMetrics() {
	if adminMetrics.requestErrors != nil {
		prometheus.Unregister(adminMetrics.requestErrors)
	}
	if adminMetrics.requestCount != nil {
		prometheus.Unregister(adminMetrics.requestCount)
	}

	adminMetrics.requestErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "admin_http",
		Name:      "request_errors_total",
		Help:      "Number of errors that occurred handling admin endpoint requests",
	}, []string{"handler", "path", "method"})

	adminMetrics.requestCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "admin_http",
		Name:      "requests_total",
		Help:      "Count of requests to the admin endpoint",
	}, []string{"handler", "path", "code", "method"}) // Added code and method labels

	prometheus.MustRegister(adminMetrics.requestErrors)
	prometheus.MustRegister(adminMetrics.requestCount)
}

func TestAdminHandlerBuiltinRouteErrors(t *testing.T) {
	initAdminMetrics()

	cfg := &Config{
		Admin: &AdminConfig{
			Listen: "localhost:2019",
		},
	}

	// Build the admin handler directly (no listener active)
	addr, err := ParseNetworkAddress("localhost:2019")
	if err != nil {
		t.Fatalf("Failed to parse address: %v", err)
	}
	handler, err := cfg.Admin.newAdminHandler(addr, false, Context{})
	if err != nil {
		t.Fatalf("Failed to create admin handler: %v", err)
	}

	tests := []struct {
		name           string
		path           string
		method         string
		headers        map[string]string
		expectedStatus int
	}{
		{
			name:           "stop endpoint wrong method",
			path:           "/stop",
			method:         http.MethodGet,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:   "config endpoint wrong content-type",
			path:   "/config/",
			method: http.MethodPost,
			// a valid token passes the version precondition so the
			// request reaches content-type validation
			headers:        map[string]string{ConfigVersionHeader: rawCfgVersion},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "config ID missing ID",
			path:           "/id/",
			method:         http.MethodGet,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, fmt.Sprintf("http://localhost:2019%s", test.path), nil)
			for k, v := range test.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != test.expectedStatus {
				t.Errorf("expected status %d but got %d", test.expectedStatus, rr.Code)
			}

			metricValue := testGetMetricValue(map[string]string{
				"path":    test.path,
				"handler": "admin",
				"method":  test.method,
			})
			if metricValue != 1 {
				t.Errorf("expected error metric to be incremented once, got %v", metricValue)
			}
		})
	}
}

func testGetMetricValue(labels map[string]string) float64 {
	promLabels := prometheus.Labels{}
	maps.Copy(promLabels, labels)

	metric, err := adminMetrics.requestErrors.GetMetricWith(promLabels)
	if err != nil {
		return 0
	}

	pb := &dto.Metric{}
	metric.Write(pb)
	return pb.GetCounter().GetValue()
}

type mockRouter struct {
	routes []AdminRoute
}

func (m mockRouter) Routes() []AdminRoute {
	return m.routes
}

type mockModule struct {
	mockRouter
}

func (m *mockModule) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID: "admin.api.mock",
		New: func() Module {
			mm := &mockModule{
				mockRouter: mockRouter{
					routes: m.routes,
				},
			}
			return mm
		},
	}
}

func TestNewAdminHandlerRouterRegistration(t *testing.T) {
	originalModules := make(map[string]ModuleInfo)
	maps.Copy(originalModules, modules)
	defer func() {
		modules = originalModules
	}()

	mockRoute := AdminRoute{
		Pattern: "/mock",
		Handler: AdminHandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusOK)
			return nil
		}),
	}

	mock := &mockModule{
		mockRouter: mockRouter{
			routes: []AdminRoute{mockRoute},
		},
	}
	RegisterModule(mock)

	addr, err := ParseNetworkAddress("localhost:2019")
	if err != nil {
		t.Fatalf("Failed to parse address: %v", err)
	}

	admin := &AdminConfig{
		EnforceOrigin: false,
	}
	handler, err := admin.newAdminHandler(addr, false, Context{})
	if err != nil {
		t.Fatalf("Failed to create admin handler: %v", err)
	}

	req := httptest.NewRequest("GET", "/mock", nil)
	req.Host = "localhost:2019"
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status code %d but got %d", http.StatusOK, rr.Code)
		t.Logf("Response body: %s", rr.Body.String())
	}
}

type mockProvisionableRouter struct {
	mockRouter
	provisionErr error
	provisioned  bool
}

func (m *mockProvisionableRouter) Provision(Context) error {
	m.provisioned = true
	return m.provisionErr
}

type mockProvisionableModule struct {
	*mockProvisionableRouter
}

func (m *mockProvisionableModule) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID: "admin.api.mock_provision",
		New: func() Module {
			mm := &mockProvisionableModule{
				mockProvisionableRouter: &mockProvisionableRouter{
					mockRouter:   m.mockRouter,
					provisionErr: m.provisionErr,
				},
			}
			return mm
		},
	}
}

func TestAdminRouterProvisioning(t *testing.T) {
	tests := []struct {
		name         string
		provisionErr error
		wantErr      bool
	}{
		{
			name:         "successful provisioning",
			provisionErr: nil,
			wantErr:      false,
		},
		{
			name:         "provisioning error",
			provisionErr: fmt.Errorf("provision failed"),
			wantErr:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalModules := make(map[string]ModuleInfo)
			maps.Copy(originalModules, modules)
			defer func() {
				modules = originalModules
			}()

			mockRoute := AdminRoute{
				Pattern: "/mock",
				Handler: AdminHandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
					return nil
				}),
			}

			// Create provisionable module
			mock := &mockProvisionableModule{
				mockProvisionableRouter: &mockProvisionableRouter{
					mockRouter: mockRouter{
						routes: []AdminRoute{mockRoute},
					},
					provisionErr: test.provisionErr,
				},
			}
			RegisterModule(mock)

			admin := &AdminConfig{}
			addr, err := ParseNetworkAddress("localhost:2019")
			if err != nil {
				t.Fatalf("Failed to parse address: %v", err)
			}

			_, err = admin.newAdminHandler(addr, false, Context{})

			if test.wantErr {
				if err == nil {
					t.Error("Expected error but got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error but got: %v", err)
				}
			}
		})
	}
}

func TestAllowedOriginsUnixSocket(t *testing.T) {
	// see comment in allowedOrigins() as to why we do not fill out allowed origins for UDS
	tests := []struct {
		name          string
		addr          NetworkAddress
		origins       []string
		expectOrigins []string
	}{
		{
			name: "unix socket with default origins",
			addr: NetworkAddress{
				Network: "unix",
				Host:    "/tmp/caddy.sock",
			},
			origins:       nil, // default origins
			expectOrigins: []string{},
		},
		{
			name: "unix socket with custom origins",
			addr: NetworkAddress{
				Network: "unix",
				Host:    "/tmp/caddy.sock",
			},
			origins: []string{"example.com"},
			expectOrigins: []string{
				"example.com",
			},
		},
		{
			name: "tcp socket on localhost gets all loopback addresses",
			addr: NetworkAddress{
				Network:   "tcp",
				Host:      "localhost",
				StartPort: 2019,
				EndPort:   2019,
			},
			origins: nil,
			expectOrigins: []string{
				"localhost:2019",
				"[::1]:2019",
				"127.0.0.1:2019",
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			admin := AdminConfig{
				Origins: test.origins,
			}

			got := admin.allowedOrigins(test.addr)

			var gotOrigins []string
			for _, u := range got {
				gotOrigins = append(gotOrigins, u.Host)
			}

			if len(gotOrigins) != len(test.expectOrigins) {
				t.Errorf("%d: Expected %d origins but got %d", i, len(test.expectOrigins), len(gotOrigins))
				return
			}

			expectMap := make(map[string]struct{})
			for _, origin := range test.expectOrigins {
				expectMap[origin] = struct{}{}
			}

			gotMap := make(map[string]struct{})
			for _, origin := range gotOrigins {
				gotMap[origin] = struct{}{}
			}

			if !reflect.DeepEqual(expectMap, gotMap) {
				t.Errorf("%d: Origins mismatch.\nExpected: %v\nGot: %v", i, test.expectOrigins, gotOrigins)
			}
		})
	}
}

func TestRemoteAdminAccessControlPathSegmentMatching(t *testing.T) {
	const authorizedKey testAdminPublicKey = "authorized"
	peerCert := &x509.Certificate{PublicKey: authorizedKey}

	tests := []struct {
		name        string
		allowedPath string
		requestPath string
		wantErr     bool
	}{
		{
			name:        "exact path",
			allowedPath: "/pki/ca/prod",
			requestPath: "/pki/ca/prod",
			wantErr:     false,
		},
		{
			name:        "subpath",
			allowedPath: "/pki/ca/prod",
			requestPath: "/pki/ca/prod/certificates",
			wantErr:     false,
		},
		{
			name:        "trailing slash subpath",
			allowedPath: "/pki/ca/prod/",
			requestPath: "/pki/ca/prod/certificates",
			wantErr:     false,
		},
		{
			name:        "sibling with shared prefix",
			allowedPath: "/pki/ca/prod",
			requestPath: "/pki/ca/prod-backup",
			wantErr:     true,
		},
		{
			name:        "same segment plus digit",
			allowedPath: "/pki/ca/prod",
			requestPath: "/pki/ca/prod1",
			wantErr:     true,
		},
		{
			name:        "root path",
			allowedPath: "/",
			requestPath: "/pki/ca/prod",
			wantErr:     false,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := RemoteAdmin{
				AccessControl: []*AdminAccess{
					{
						Permissions: []AdminPermissions{
							{
								Methods: []string{http.MethodGet},
								Paths:   []string{test.allowedPath},
							},
						},
						publicKeys: []crypto.PublicKey{authorizedKey},
					},
				},
			}

			req := httptest.NewRequest(http.MethodGet, "https://localhost:2021"+test.requestPath, nil)
			req.TLS = &tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{peerCert}},
			}

			err := remote.enforceAccessControls(req)
			if test.wantErr {
				if err == nil {
					t.Errorf("test %d (%s): allowed path %q, request path %q: expected forbidden error, got nil", i, test.name, test.allowedPath, test.requestPath)
					return
				}
				var apiErr APIError
				if !errors.As(err, &apiErr) {
					t.Errorf("test %d (%s): allowed path %q, request path %q: expected APIError with HTTP status %d, got %T: %v", i, test.name, test.allowedPath, test.requestPath, http.StatusForbidden, err, err)
					return
				}
				if apiErr.HTTPStatus != http.StatusForbidden {
					t.Errorf("test %d (%s): allowed path %q, request path %q: expected HTTP status %d, got %d", i, test.name, test.allowedPath, test.requestPath, http.StatusForbidden, apiErr.HTTPStatus)
				}
				return
			}

			if err != nil {
				t.Errorf("test %d (%s): allowed path %q, request path %q: expected no error, got %v", i, test.name, test.allowedPath, test.requestPath, err)
			}
		})
	}
}

func TestReplaceRemoteAdminServer(t *testing.T) {
	const testCert = `MIIDCTCCAfGgAwIBAgIUXsqJ1mY8pKlHQtI3HJ23x2eZPqwwDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UEAwwJbG9jYWxob3N0MB4XDTIzMDEwMTAwMDAwMFoXDTI0MDEw
MTAwMDAwMFowFDESMBAGA1UEAwwJbG9jYWxob3N0MIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEA4O4S6BSoYcoxvRqI+h7yPOjF6KjntjzVVm9M+uHK4lzX
F1L3pSxJ2nDD4wZEV3FJ5yFOHVFqkG2vXG3BIczOlYG7UeNmKbQnKc5kZj3HGUrS
VGEktA4OJbeZhhWP15gcXN5eDM2eH3g9BFXVX6AURxLiUXzhNBUEZuj/OEyH9yEF
/qPCE+EjzVvWxvBXwgz/io4r4yok/Vq/bxJ6FlV6R7DX5oJSXyO0VEHZPi9DIyNU
kK3F/r4U1sWiJGWOs8i3YQWZ2ejh1C0aLFZpPcCGGgMNpoF31gyYP6ZuPDUyCXsE
g36UUw1JHNtIXYcLhnXuqj4A8TybTDpgXLqvwA9DBQIDAQABo1MwUTAdBgNVHQ4E
FgQUc13z30pFC63rr/HGKOE7E82vjXwwHwYDVR0jBBgwFoAUc13z30pFC63rr/HG
KOE7E82vjXwwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAHO3j
oeiUXXJ7xD4P8Wj5t9d+E8lE1Xv1Dk3Z+EdG5+dan+RcToE42JJp9zB7FIh5Qz8g
W77LAjqh5oyqz3A2VJcyVgfE3uJP1R1mJM7JfGHf84QH4TZF2Q1RZY4SZs0VQ6+q
5wSlIZ4NXDy4Q4XkIJBGS61wT8IzYFXYBpx4PCP1Qj0PIE4sevEGwjsBIgxK307o
BxF8AWe6N6e4YZmQLGjQ+SeH0iwZb6vpkHyAY8Kj2hvK+cq2P7vU3VGi0t3r1F8L
IvrXHCvO2BMNJ/1UK1M4YNX8LYJqQhg9hEsIROe1OE/m3VhxIYMJI+qZXk9yHfgJ
vq+SH04xKhtFudVBAQ==`

	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: false,
		},
		{
			name: "nil admin config",
			cfg: &Config{
				Admin: nil,
			},
			wantErr: false,
		},
		{
			name: "nil remote config",
			cfg: &Config{
				Admin: &AdminConfig{},
			},
			wantErr: false,
		},
		{
			name: "invalid listen address",
			cfg: &Config{
				Admin: &AdminConfig{
					Remote: &RemoteAdmin{
						Listen: "invalid:address",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "valid config",
			cfg: &Config{
				Admin: &AdminConfig{
					Identity: &IdentityConfig{},
					Remote: &RemoteAdmin{
						Listen: "localhost:2021",
						AccessControl: []*AdminAccess{
							{
								PublicKeys:  []string{testCert},
								Permissions: []AdminPermissions{{Methods: []string{"GET"}, Paths: []string{"/test"}}},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid certificate",
			cfg: &Config{
				Admin: &AdminConfig{
					Identity: &IdentityConfig{},
					Remote: &RemoteAdmin{
						Listen: "localhost:2021",
						AccessControl: []*AdminAccess{
							{
								PublicKeys:  []string{"invalid-cert-data"},
								Permissions: []AdminPermissions{{Methods: []string{"GET"}, Paths: []string{"/test"}}},
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := Context{
				Context: context.Background(),
				cfg:     test.cfg,
			}

			if test.cfg != nil {
				test.cfg.storage = &certmagic.FileStorage{Path: t.TempDir()}
			}

			if test.cfg != nil && test.cfg.Admin != nil && test.cfg.Admin.Identity != nil {
				identityCertCache = certmagic.NewCache(certmagic.CacheOptions{
					GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
						return &certmagic.Config{}, nil
					},
				})
			}

			err := replaceRemoteAdminServer(ctx, test.cfg)

			if test.wantErr {
				if err == nil {
					t.Error("Expected error but got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error but got: %v", err)
				}
			}

			// Clean up
			if remoteAdminServer != nil {
				_ = stopAdminServer(remoteAdminServer)
			}
		})
	}
}

type mockIssuer struct {
	configSet *certmagic.Config
}

func (m *mockIssuer) Issue(ctx context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	return &certmagic.IssuedCertificate{
		Certificate: []byte(csr.Raw),
	}, nil
}

func (m *mockIssuer) SetConfig(cfg *certmagic.Config) {
	m.configSet = cfg
}

func (m *mockIssuer) IssuerKey() string {
	return "mock"
}

type mockIssuerModule struct {
	*mockIssuer
}

func (m *mockIssuerModule) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID: "tls.issuance.acme",
		New: func() Module {
			return &mockIssuerModule{mockIssuer: new(mockIssuer)}
		},
	}
}

func TestManageIdentity(t *testing.T) {
	originalModules := make(map[string]ModuleInfo)
	maps.Copy(originalModules, modules)
	defer func() {
		modules = originalModules
	}()

	RegisterModule(&mockIssuerModule{})

	certPEM := []byte(`-----BEGIN CERTIFICATE-----
MIIDujCCAqKgAwIBAgIIE31FZVaPXTUwDQYJKoZIhvcNAQEFBQAwSTELMAkGA1UE
BhMCVVMxEzARBgNVBAoTCkdvb2dsZSBJbmMxJTAjBgNVBAMTHEdvb2dsZSBJbnRl
cm5ldCBBdXRob3JpdHkgRzIwHhcNMTQwMTI5MTMyNzQzWhcNMTQwNTI5MDAwMDAw
WjBpMQswCQYDVQQGEwJVUzETMBEGA1UECAwKQ2FsaWZvcm5pYTEWMBQGA1UEBwwN
TW91bnRhaW4gVmlldzETMBEGA1UECgwKR29vZ2xlIEluYzEYMBYGA1UEAwwPbWFp
bC5nb29nbGUuY29tMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE3lcub2pUwkjC
5GJQA2ZZfJJi6d1QHhEmkX9VxKYGp6gagZuRqJWy9TXP6++1ZzQQxqZLD0TkuxZ9
8i9Nz00000CCBjCCAQQwHQYDVR0lBBYwFAYIKwYBBQUHAwEGCCsGAQUFBwMCMGgG
CCsGAQUFBwEBBFwwWjArBggrBgEFBQcwAoYfaHR0cDovL3BraS5nb29nbGUuY29t
L0dJQUcyLmNydDArBggrBgEFBQcwAYYfaHR0cDovL2NsaWVudHMxLmdvb2dsZS5j
b20vb2NzcDAdBgNVHQ4EFgQUiJxtimAuTfwb+aUtBn5UYKreKvMwDAYDVR0TAQH/
BAIwADAfBgNVHSMEGDAWgBRK3QYWG7z2aLV29YG2u2IaulqBLzAXBgNVHREEEDAO
ggxtYWlsLmdvb2dsZTANBgkqhkiG9w0BAQUFAAOCAQEAMP6IWgNGZE8wP9TjFjSZ
3mmW3A1eIr0CuPwNZ2LJ5ZD1i70ojzcj4I9IdP5yPg9CAEV4hNASbM1LzfC7GmJE
tPzW5tRmpKVWZGRgTgZI8Hp/xZXMwLh9ZmXV4kESFAGj5G5FNvJyUV7R5Eh+7OZX
7G4jJ4ZGJh+5jzN9HdJJHQHGYNIYOzC7+HH9UMwCjX9vhQ4RjwFZJThS2Yb+y7pb
9yxTJZoXC6J0H5JpnZb7kZEJ+Xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
-----END CERTIFICATE-----`)

	keyPEM := []byte(`-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDRS0LmTwUT0iwP
...
-----END PRIVATE KEY-----`)

	tmpDir, err := os.MkdirTemp("", "TestManageIdentity-")
	if err != nil {
		t.Fatal(err)
	}
	testStorage := certmagic.FileStorage{Path: tmpDir}
	// Clean up the temp dir after the test finishes. Ensure any background
	// certificate maintenance is stopped first to avoid RemoveAll races.
	t.Cleanup(func() {
		if identityCertCache != nil {
			identityCertCache.Stop()
			identityCertCache = nil
		}
		// Give goroutines a moment to exit and release file handles.
		time.Sleep(50 * time.Millisecond)
		_ = os.RemoveAll(tmpDir)
	})

	err = testStorage.Store(context.Background(), "localhost/localhost.crt", certPEM)
	if err != nil {
		t.Fatal(err)
	}
	err = testStorage.Store(context.Background(), "localhost/localhost.key", keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		cfg        *Config
		wantErr    bool
		checkState func(*testing.T, *Config)
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "nil admin config",
			cfg: &Config{
				Admin: nil,
			},
		},
		{
			name: "nil identity config",
			cfg: &Config{
				Admin: &AdminConfig{},
			},
		},
		{
			name: "default issuer when none specified",
			cfg: &Config{
				Admin: &AdminConfig{
					Identity: &IdentityConfig{
						Identifiers: []string{"localhost"},
					},
				},
				storage: &testStorage,
			},
			checkState: func(t *testing.T, cfg *Config) {
				if len(cfg.Admin.Identity.issuers) == 0 {
					t.Error("Expected at least 1 issuer to be configured")
					return
				}
				if _, ok := cfg.Admin.Identity.issuers[0].(*mockIssuerModule); !ok {
					t.Error("Expected mock issuer to be configured")
				}
			},
		},
		{
			name: "custom issuer",
			cfg: &Config{
				Admin: &AdminConfig{
					Identity: &IdentityConfig{
						Identifiers: []string{"localhost"},
						IssuersRaw: []json.RawMessage{
							json.RawMessage(`{"module": "acme"}`),
						},
					},
				},
				storage: &testStorage,
			},
			checkState: func(t *testing.T, cfg *Config) {
				if len(cfg.Admin.Identity.issuers) != 1 {
					t.Fatalf("Expected 1 issuer, got %d", len(cfg.Admin.Identity.issuers))
				}
				mockIss, ok := cfg.Admin.Identity.issuers[0].(*mockIssuerModule)
				if !ok {
					t.Fatal("Expected mock issuer")
				}
				if mockIss.configSet == nil {
					t.Error("Issuer config was not set")
				}
			},
		},
		{
			name: "invalid issuer module",
			cfg: &Config{
				Admin: &AdminConfig{
					Identity: &IdentityConfig{
						Identifiers: []string{"localhost"},
						IssuersRaw: []json.RawMessage{
							json.RawMessage(`{"module": "doesnt_exist"}`),
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if identityCertCache != nil {
				// Reset the cert cache before each test
				identityCertCache.Stop()
				identityCertCache = nil
			}
			// Ensure any cache started by manageIdentity is stopped at the end
			defer func() {
				if identityCertCache != nil {
					identityCertCache.Stop()
					identityCertCache = nil
				}
			}()

			ctx := Context{
				Context:         context.Background(),
				cfg:             test.cfg,
				moduleInstances: make(map[string][]Module),
			}

			// If this test provided a FileStorage, set the package-level
			// testCertMagicStorageOverride so certmagicConfig will use it.
			if test.cfg != nil && test.cfg.storage != nil {
				testCertMagicStorageOverride = test.cfg.storage
				defer func() { testCertMagicStorageOverride = nil }()
			}

			err := manageIdentity(ctx, test.cfg)

			if test.wantErr {
				if err == nil {
					t.Error("Expected error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Expected no error but got: %v", err)
			}

			if test.checkState != nil {
				test.checkState(t, test.cfg)
			}
		})
	}
}

func TestUnsyncedConfigAccessCanonicalArrayIndices(t *testing.T) {
	rawCfg = map[string]any{
		rawConfigKey: map[string]any{
			"list": []any{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"},
		},
	}

	tests := []struct {
		name       string
		path       string
		wantOutput string
		wantErr    bool
	}{
		{name: "allow zero", path: "/" + rawConfigKey + "/list/0", wantOutput: "\"zero\"\n"},
		{name: "allow one", path: "/" + rawConfigKey + "/list/1", wantOutput: "\"one\"\n"},
		{name: "allow ten", path: "/" + rawConfigKey + "/list/10", wantOutput: "\"ten\"\n"},
		{name: "reject leading zero", path: "/" + rawConfigKey + "/list/01", wantErr: true},
		{name: "reject multiple leading zeros", path: "/" + rawConfigKey + "/list/002", wantErr: true},
		{name: "reject plus sign", path: "/" + rawConfigKey + "/list/+1", wantErr: true},
		{name: "reject negative zero", path: "/" + rawConfigKey + "/list/-0", wantErr: true},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotOutput bytes.Buffer
			err := unsyncedConfigAccess(http.MethodGet, tc.path, nil, &gotOutput)

			if tc.wantErr {
				if err == nil {
					t.Errorf("test %d (%s): input path %q: expected error, got nil with output %q", i, tc.name, tc.path, gotOutput.String())
				}
				return
			}

			if err != nil {
				t.Errorf("test %d (%s): input path %q: expected no error with output %q, got error %v with output %q", i, tc.name, tc.path, tc.wantOutput, err, gotOutput.String())
			}
			if gotOutput.String() != tc.wantOutput {
				t.Errorf("test %d (%s): input path %q: expected output %q, got %q", i, tc.name, tc.path, tc.wantOutput, gotOutput.String())
			}
		})
	}
}
