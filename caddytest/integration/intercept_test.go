package integration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestIntercept(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			respond /intercept "I'm a teapot" 408
			header /intercept To-Intercept ok
			respond /no-intercept "I'm not a teapot"

			intercept {
				@teapot status 408
				handle_response @teapot {
					header /intercept intercepted {resp.header.To-Intercept}
					respond /intercept "I'm a combined coffee/tea pot that is temporarily out of coffee" 503
				}
			}
		}
		`, "caddyfile")

	r, _ := tester.AssertGetResponse("http://localhost:9080/intercept", 503, "I'm a combined coffee/tea pot that is temporarily out of coffee")
	if r.Header.Get("intercepted") != "ok" {
		t.Fatalf(`header "intercepted" value is not "ok": %s`, r.Header.Get("intercepted"))
	}

	tester.AssertGetResponse("http://localhost:9080/no-intercept", 200, "I'm not a teapot")
}

// TestInterceptHandleResponse covers the response replacement semantics of
// handle_response blocks against an original `respond "old" 500`:
//   - an explicit empty replacement body must produce the new status with an
//     empty body (Content-Length: 0) plus any headers set by the route
//   - a non-empty replacement body must replace both status and body, with
//     Content-Length recomputed and the original headers preserved
//   - a header-only route must keep the original status and body while
//     layering the new header on top
//   - a route that does nothing must replay the original response verbatim
//   - stale original Content-Length and Transfer-Encoding headers must not
//     truncate, duplicate or otherwise misframe the replacement body
func TestInterceptHandleResponse(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			respond /empty "old" 500
			header /empty X-Case empty
			respond /body "old" 500
			header /body X-Case body
			respond /hdronly "old" 500
			header /hdronly X-Case hdronly
			respond /noaction "old" 500
			header /noaction X-Case noaction
			respond /cl "this-is-a-very-long-old-body" 500
			header /cl X-Case cl
			header /cl Content-Length 28
			respond /clshort "old" 500
			header /clshort X-Case clshort
			header /clshort Content-Length 3
			respond /te "old" 500
			header /te X-Case te
			header /te Transfer-Encoding chunked

			intercept {
				@empty {
					status 500
					header X-Case empty
				}
				handle_response @empty {
					header X-Replaced yes
					respond 200
				}

				@body {
					status 500
					header X-Case body
				}
				handle_response @body {
					respond "new" 202
				}

				@hdronly {
					status 500
					header X-Case hdronly
				}
				handle_response @hdronly {
					header X-Replaced yes
				}

				@noaction {
					status 500
					header X-Case noaction
				}
				handle_response @noaction {
				}

				@cl {
					status 500
					header X-Case cl
				}
				handle_response @cl {
					respond "new" 202
				}

				@clshort {
					status 500
					header X-Case clshort
				}
				handle_response @clshort {
					respond "a-much-longer-new-body" 202
				}

				@te {
					status 500
					header X-Case te
				}
				handle_response @te {
					respond "new" 202
				}
			}
		}
		`, "caddyfile")

	// explicit empty body: new status, the added header and an empty body
	r, body := tester.AssertGetResponse("http://localhost:9080/empty", 200, "")
	if r.Header.Get("X-Replaced") != "yes" {
		t.Errorf("empty replacement: expected X-Replaced header %q, got %q", "yes", r.Header.Get("X-Replaced"))
	}
	if r.Header.Get("X-Case") != "empty" {
		t.Errorf("empty replacement: expected original X-Case header to be preserved, got %q", r.Header.Get("X-Case"))
	}
	if r.ContentLength != 0 {
		t.Errorf("empty replacement: expected Content-Length 0, got %d", r.ContentLength)
	}
	if len(r.TransferEncoding) != 0 {
		t.Errorf("empty replacement: expected no Transfer-Encoding, got %v", r.TransferEncoding)
	}
	if body != "" {
		t.Errorf("empty replacement: expected empty body, got %q", body)
	}

	// non-empty replacement: new status and body, recomputed Content-Length
	r, body = tester.AssertGetResponse("http://localhost:9080/body", 202, "new")
	if body != "new" {
		t.Errorf("replacement body: expected %q, got %q", "new", body)
	}
	if r.ContentLength != int64(len("new")) {
		t.Errorf("replacement body: expected Content-Length %d, got %d", len("new"), r.ContentLength)
	}
	if r.Header.Get("X-Case") != "body" {
		t.Errorf("replacement body: expected original X-Case header to be preserved, got %q", r.Header.Get("X-Case"))
	}
	if len(r.TransferEncoding) != 0 {
		t.Errorf("replacement body: expected no Transfer-Encoding, got %v", r.TransferEncoding)
	}

	// header-only route: original status and body survive, header is added
	r, body = tester.AssertGetResponse("http://localhost:9080/hdronly", 500, "old")
	if body != "old" {
		t.Errorf("header-only: expected original body %q, got %q", "old", body)
	}
	if r.Header.Get("X-Replaced") != "yes" {
		t.Errorf("header-only: expected X-Replaced header %q, got %q", "yes", r.Header.Get("X-Replaced"))
	}
	if r.Header.Get("X-Case") != "hdronly" {
		t.Errorf("header-only: expected original X-Case header to be preserved, got %q", r.Header.Get("X-Case"))
	}
	if r.ContentLength != int64(len("old")) {
		t.Errorf("header-only: expected Content-Length %d, got %d", len("old"), r.ContentLength)
	}

	// no action at all: original response replayed verbatim
	r, body = tester.AssertGetResponse("http://localhost:9080/noaction", 500, "old")
	if body != "old" {
		t.Errorf("no-action: expected original body %q, got %q", "old", body)
	}
	if r.Header.Get("X-Replaced") != "" {
		t.Errorf("no-action: did not expect X-Replaced header, got %q", r.Header.Get("X-Replaced"))
	}
	if r.Header.Get("X-Case") != "noaction" {
		t.Errorf("no-action: expected original X-Case header to be preserved, got %q", r.Header.Get("X-Case"))
	}

	// stale, larger Content-Length must not truncate or stall the shorter
	// replacement body; it must be recomputed to the new body length
	r, body = tester.AssertGetResponse("http://localhost:9080/cl", 202, "new")
	if body != "new" {
		t.Errorf("content-length: expected replacement body %q, got %q", "new", body)
	}
	if r.ContentLength != int64(len("new")) {
		t.Errorf("content-length: expected recomputed Content-Length %d, got %d", len("new"), r.ContentLength)
	}
	if len(r.TransferEncoding) != 0 {
		t.Errorf("content-length: expected no Transfer-Encoding, got %v", r.TransferEncoding)
	}

	// a stale, shorter Content-Length must not truncate the longer
	// replacement body either
	r, body = tester.AssertGetResponse("http://localhost:9080/clshort", 202, "a-much-longer-new-body")
	if body != "a-much-longer-new-body" {
		t.Errorf("content-length: expected full replacement body %q, got %q", "a-much-longer-new-body", body)
	}
	if r.ContentLength != int64(len("a-much-longer-new-body")) {
		t.Errorf("content-length: expected recomputed Content-Length %d, got %d", len("a-much-longer-new-body"), r.ContentLength)
	}

	// original Transfer-Encoding: chunked must not leak into or duplicate on
	// the content-length-framed replacement
	r, body = tester.AssertGetResponse("http://localhost:9080/te", 202, "new")
	if body != "new" {
		t.Errorf("transfer-encoding: expected replacement body %q, got %q", "new", body)
	}
	if r.ContentLength != int64(len("new")) {
		t.Errorf("transfer-encoding: expected Content-Length %d, got %d", len("new"), r.ContentLength)
	}
	if len(r.TransferEncoding) != 0 {
		t.Errorf("transfer-encoding: expected no Transfer-Encoding on the replacement, got %v", r.TransferEncoding)
	}
}

// TestInterceptHandleResponseHEAD ensures a replacement response to a HEAD
// request has an empty body while keeping the replacement status and the
// Content-Length that describes the body that would have been sent.
func TestInterceptHandleResponseHEAD(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			respond /body "old" 500

			intercept {
				@err status 500
				handle_response @err {
					respond "new" 202
				}
			}
		}
		`, "caddyfile")

	req, err := http.NewRequest(http.MethodHead, "http://localhost:9080/body", nil)
	if err != nil {
		t.Fatalf("unable to create request %s", err)
	}
	r := tester.AssertResponseCode(req, 202)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("unable to read response body: %s", err)
	}
	if len(body) != 0 {
		t.Errorf("HEAD: expected empty body, got %q", string(body))
	}
	if r.ContentLength != int64(len("new")) {
		t.Errorf("HEAD: expected Content-Length %d, got %d", len("new"), r.ContentLength)
	}
}

// TestInterceptReplaceStatus covers the status-only replacement semantics:
// the expanded code must be a final status code (200-599); 1xx codes,
// out-of-range values, non-numeric values and failed placeholder expansions
// leave the original headers and body untouched and yield a 500.
func TestInterceptReplaceStatus(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			respond /valid "old" 500
			header /valid X-Case valid
			respond /onexx "old" 500
			header /onexx X-Case onexx
			respond /low "old" 500
			header /low X-Case low
			respond /high "old" 500
			header /high X-Case high
			respond /nan "old" 500
			header /nan X-Case nan
			respond /ph "old" 500
			header /ph X-Case ph

			intercept {
				@valid header X-Case valid
				replace_status @valid 200

				@onexx header X-Case onexx
				replace_status @onexx 150

				@low header X-Case low
				replace_status @low 99

				@high header X-Case high
				replace_status @high 600

				@nan header X-Case nan
				replace_status @nan abc

				@ph header X-Case ph
				replace_status @ph {http.request.uri.query.code}
			}
		}
		`, "caddyfile")

	// valid code: only the status changes, body and headers are replayed
	r, body := tester.AssertGetResponse("http://localhost:9080/valid", 200, "old")
	if r.Header.Get("X-Case") != "valid" {
		t.Errorf("valid: expected original header preserved, got %q", r.Header.Get("X-Case"))
	}

	// invalid codes: original status code becomes 500, original headers and
	// body are left untouched
	for _, uri := range []string{
		"http://localhost:9080/onexx", // 1xx is not a final status code
		"http://localhost:9080/low",   // below the valid range
		"http://localhost:9080/high",  // above the valid range
		"http://localhost:9080/nan",   // non-numeric
	} {
		r, body = tester.AssertGetResponse(uri, 500, "old")
		if body != "old" {
			t.Errorf("%s: expected original body %q, got %q", uri, "old", body)
		}
		if r.Header.Get("X-Case") == "" {
			t.Errorf("%s: expected original X-Case header to be preserved", uri)
		}
	}

	// placeholder that expands to a valid code is used
	r, body = tester.AssertGetResponse("http://localhost:9080/ph?code=202", 202, "old")
	if body != "old" {
		t.Errorf("placeholder valid: expected original body %q, got %q", "old", body)
	}
	if r.Header.Get("X-Case") != "ph" {
		t.Errorf("placeholder valid: expected original header preserved, got %q", r.Header.Get("X-Case"))
	}

	// placeholder expansions to 1xx, non-numeric, or an empty value (missing
	// query parameter) all yield 500 with the original response untouched
	for _, uri := range []string{
		"http://localhost:9080/ph?code=150",
		"http://localhost:9080/ph?code=abc",
		"http://localhost:9080/ph",
	} {
		r, body = tester.AssertGetResponse(uri, 500, "old")
		if body != "old" {
			t.Errorf("%s: expected original body %q, got %q", uri, "old", body)
		}
		if r.Header.Get("X-Case") != "ph" {
			t.Errorf("%s: expected original X-Case header to be preserved, got %q", uri, r.Header.Get("X-Case"))
		}
	}
}

// TestInterceptReplaceStatusNotMatched ensures responses that don't match
// any response matcher pass through unchanged.
func TestInterceptReplaceStatusNotMatched(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			respond /ok "all good" 200

			intercept {
				@err status 5xx
				replace_status @err 503
			}
		}
		`, "caddyfile")

	tester.AssertGetResponse("http://localhost:9080/ok", 200, "all good")
}

// rawInterceptRequest dials the test server and sends a minimal HTTP/1.1
// request, returning the full raw response for assertions on wire-level
// behavior (multiple status lines, framing headers, etc.).
func rawInterceptRequest(t *testing.T, request string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:9080", 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// the server closes the connection after the response (Connection: close)
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(raw)
}

// TestInterceptInformationalPassThrough ensures 1xx informational responses
// are forwarded to the client immediately even when the final response is
// intercepted.
func TestInterceptInformationalPassThrough(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			route /hint {
				header X-Hint yes
				respond "early" 103
				respond "final" 200
			}

			intercept {
				@ok status 2xx
				handle_response @ok {
					header X-Replaced yes
				}
			}
		}
		`, "caddyfile")

	raw := rawInterceptRequest(t, "GET /hint HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	if !strings.Contains(raw, "HTTP/1.1 103 Early Hints") {
		t.Errorf("expected 103 informational response to pass through, raw response:\n%s", raw)
	}
	if !strings.Contains(raw, "HTTP/1.1 200 OK") {
		t.Errorf("expected final 200 response, raw response:\n%s", raw)
	}
	if !strings.Contains(strings.ToLower(raw), "x-hint: yes") {
		t.Errorf("expected informational header X-Hint on the wire, raw response:\n%s", raw)
	}
	if !strings.Contains(strings.ToLower(raw), "x-replaced: yes") {
		t.Errorf("expected final response to carry the added X-Replaced header, raw response:\n%s", raw)
	}
	if !strings.HasSuffix(strings.TrimSpace(raw), "final") {
		t.Errorf("expected final response body %q, raw response:\n%s", "final", raw)
	}
}

// TestInterceptWebSocketUpgrade ensures a 101 Switching Protocols response
// and the tunneled data that follows pass through intercept untouched.
func TestInterceptWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "plain")
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter is not a Hijacker")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Tunnel: yes\r\n\r\n")
		if err := bufrw.Flush(); err != nil {
			t.Errorf("flush upgrade: %v", err)
			return
		}
		_, _ = fmt.Fprintf(conn, "tunnel-ok\n")
	}))
	defer backend.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
			reverse_proxy %s

			intercept {
				@err status 5xx
				handle_response @err {
					respond "replaced" 200
				}
			}
		}
		`, strings.TrimPrefix(backend.URL, "http://")), "caddyfile")

	raw := rawInterceptRequest(t, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(strings.NewReader(raw))
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v; raw response:\n%s", err, raw)
	}
	if !strings.Contains(statusLine, "101 Switching Protocols") {
		t.Fatalf("expected 101 Switching Protocols, got %q; raw response:\n%s", strings.TrimSpace(statusLine), raw)
	}
	headers := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v; raw response:\n%s", err, raw)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			headers.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
	if headers.Get("X-Tunnel") != "yes" {
		t.Errorf("expected tunneled upgrade response to carry X-Tunnel header, raw response:\n%s", raw)
	}
	tunneled, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(tunneled) != "tunnel-ok" {
		t.Errorf("expected tunneled data %q, got %q (err: %v); raw response:\n%s", "tunnel-ok", strings.TrimSpace(tunneled), err, raw)
	}

	// non-upgrade requests are still handled normally
	tester.AssertGetResponse("http://localhost:9080/", 200, "plain")
}
