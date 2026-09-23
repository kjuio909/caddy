package integration

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// interceptTestConfig wraps a site config with the standard global options
// used by the intercept integration tests.
func interceptTestConfig(siteConfig string) string {
	return `{
			skip_install_trust
			admin localhost:2999
			http_port     9080
			https_port    9443
			grace_period  1ns
		}

		localhost:9080 {
` + siteConfig + `
		}
		`
}

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

// TestInterceptReplaceWithEmptyBody verifies that a replacement route which
// only adds a header and writes an empty body (`respond 200`) results in the
// new status, the added header, and an explicitly empty body.
func TestInterceptReplaceWithEmptyBody(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					header X-Replaced yes
					respond 200
				}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 200, "")
	if r.Header.Get("X-Replaced") != "yes" {
		t.Fatalf(`header "X-Replaced" value is not "yes": %q`, r.Header.Get("X-Replaced"))
	}
	if body != "" {
		t.Fatalf("expected empty body, got %q", body)
	}
	if cl := r.Header.Get("Content-Length"); cl != "0" {
		t.Fatalf("expected explicit Content-Length 0, got %q", cl)
	}
}

// TestInterceptReplaceWithNewBody verifies that a non-empty replacement body
// replaces the original body and that Content-Length is recomputed for it.
func TestInterceptReplaceWithNewBody(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					respond "new" 202
				}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 202, "new")
	if body != "new" {
		t.Fatalf("expected replacement body %q, got %q", "new", body)
	}
	if r.ContentLength != int64(len("new")) {
		t.Fatalf("expected recomputed Content-Length %d, got %d", len("new"), r.ContentLength)
	}
}

// TestInterceptHeaderOnly verifies that replacement routes which only mutate
// headers (and never write a response) preserve the original status and body
// while applying the header change.
func TestInterceptHeaderOnly(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					header X-Added yes
				}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 500, "old")
	if r.Header.Get("X-Added") != "yes" {
		t.Fatalf(`header "X-Added" value is not "yes": %q`, r.Header.Get("X-Added"))
	}
	if body != "old" {
		t.Fatalf("expected original body to be preserved, got %q", body)
	}
}

// TestInterceptNoAction verifies that matched replacement routes which do
// nothing leave the original response entirely untouched.
func TestInterceptNoAction(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					vars intercepted true
				}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 500, "old")
	if body != "old" {
		t.Fatalf("expected original body to be preserved, got %q", body)
	}
	if r.Header.Get("X-Intercepted") != "" {
		t.Fatalf("expected no replacement header to be added, got X-Intercepted=%q", r.Header.Get("X-Intercepted"))
	}
}

// TestInterceptReplaceStatusPlaceholder verifies that a replace_status value
// containing a placeholder is expanded after the intercept placeholders are
// populated, and a valid 200-599 result substitutes only the status code while
// the original body is kept.
func TestInterceptReplaceStatusPlaceholder(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			header X-Want-Status 202
			respond "old" 500

			intercept {
				@want {
					status 500
					header X-Want-Status *
				}
				replace_status @want {http.intercept.header.X-Want-Status}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 202, "old")
	if r.StatusCode != 202 {
		t.Fatalf("expected placeholder-expanded status 202, got %d", r.StatusCode)
	}
	if body != "old" {
		t.Fatalf("expected original body to be preserved, got %q", body)
	}
}

// TestInterceptReplaceStatusInvalid verifies that 1xx, out-of-range,
// non-numeric, and failed placeholder expansions leave the original headers
// and body untouched but produce a final 500 status.
func TestInterceptReplaceStatusInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{name: "informational_100", code: "100"},
		{name: "informational_199", code: "199"},
		{name: "too_low_99", code: "99"},
		{name: "too_high_600", code: "600"},
		{name: "non_numeric", code: "abc"},
		{name: "failed_placeholder", code: "{http.intercept.header.Does-Not-Exist}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tester := caddytest.NewTester(t)
			tester.InitServer(interceptTestConfig(`
				respond "old" 500

				intercept {
					@err status 500
					replace_status @err `+tc.code+`
				}
			`), "caddyfile")

			r, body := tester.AssertGetResponse("http://localhost:9080/", 500, "old")
			if body != "old" {
				t.Fatalf("expected original body to be preserved, got %q", body)
			}
			if r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("expected original headers to be preserved, got %v", r.Header)
			}
		})
	}
}

// TestInterceptReplaceRecomputesContentLength verifies that a stale
// Content-Length from the original response does not truncate the shorter
// replacement body nor leak the wrong length.
func TestInterceptReplaceRecomputesContentLength(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			header Content-Length 100
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					respond "new" 202
				}
			}
		`), "caddyfile")

	r, body := tester.AssertGetResponse("http://localhost:9080/", 202, "new")
	if body != "new" {
		t.Fatalf("expected complete replacement body %q, got %q", "new", body)
	}
	if r.ContentLength != int64(len("new")) {
		t.Fatalf("expected recomputed Content-Length %d, got %d", len("new"), r.ContentLength)
	}
}

// TestInterceptReplaceDropsTransferEncoding verifies on the wire that an
// original Transfer-Encoding header is not duplicated (nor does it cause
// chunked framing) once the response is replaced with a known-length body.
func TestInterceptReplaceDropsTransferEncoding(t *testing.T) {
	caddytest.NewTester(t).InitServer(interceptTestConfig(`
			header Transfer-Encoding chunked
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					respond "new" 202
				}
			}
		`), "caddyfile")

	conn, err := net.Dial("tcp", "127.0.0.1:9080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	response := string(raw)

	head, resBody, ok := strings.Cut(response, "\r\n\r\n")
	if !ok {
		t.Fatalf("malformed response: %q", response)
	}
	if !strings.HasPrefix(head, "HTTP/1.1 202") {
		t.Fatalf("expected status 202, got %q", strings.SplitN(head, "\r\n", 1)[0])
	}
	if strings.Contains(strings.ToLower(head), "transfer-encoding") {
		t.Fatalf("expected Transfer-Encoding to be removed, got headers %q", head)
	}
	if !strings.Contains(head, "Content-Length: 3") {
		t.Fatalf("expected Content-Length: 3, got headers %q", head)
	}
	if resBody != "new" {
		t.Fatalf("expected unchunked body %q, got %q", "new", resBody)
	}
}

// TestInterceptReplaceHead verifies that HEAD requests receive no body even
// when the response is replaced, while the replacement Content-Length is still
// advertised.
func TestInterceptReplaceHead(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(interceptTestConfig(`
			respond "old" 500

			intercept {
				@err status 500
				handle_response @err {
					respond "new" 202
				}
			}
		`), "caddyfile")

	req, err := http.NewRequest("HEAD", "http://localhost:9080/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r := tester.AssertResponseCode(req, 202)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("expected empty body for HEAD, got %q", string(body))
	}
	if r.Header.Get("Content-Length") != "3" {
		t.Fatalf("expected Content-Length 3 for HEAD, got %q", r.Header.Get("Content-Length"))
	}
}

// TestInterceptInformationalPassthrough verifies that an informational 1xx
// response is passed straight through before the buffered final response, even
// when the final response is intercepted and its status is replaced.
func TestInterceptInformationalPassthrough(t *testing.T) {
	caddytest.NewTester(t).InitServer(interceptTestConfig(`
			handle /hints {
				respond 103
				respond "old" 500
			}

			intercept {
				@err status 500
				replace_status @err 202
			}
		`), "caddyfile")

	conn, err := net.Dial("tcp", "127.0.0.1:9080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if _, err := io.WriteString(conn, "GET /hints HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	response := string(raw)

	first := strings.Index(response, "HTTP/1.1 103")
	final := strings.Index(response, "HTTP/1.1 202")
	if first < 0 || final < 0 || first > final {
		t.Fatalf("expected 103 informational response before 202 final response, got %q", response)
	}
	if !strings.HasSuffix(response, "old") {
		t.Fatalf("expected original body to be preserved, got %q", response)
	}
}
