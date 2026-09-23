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

package intercept

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(Intercept{})
	httpcaddyfile.RegisterHandlerDirective("intercept", parseCaddyfile)
}

// Intercept is a middleware that intercepts then replaces or modifies the original response.
// It can, for instance, be used to implement X-Sendfile/X-Accel-Redirect-like features
// when using modules like FrankenPHP or Caddy Snake.
//
// EXPERIMENTAL: Subject to change or removal.
type Intercept struct {
	// List of handlers and their associated matchers to evaluate
	// after successful response generation.
	// The first handler that matches the original response will
	// be invoked. The original response body will not be
	// written to the client;
	// it is up to the handler to finish handling the response.
	//
	// Three new placeholders are available in this handler chain:
	// - `{http.intercept.status_code}` The status code from the response
	// - `{http.intercept.header.*}` The headers from the response
	HandleResponse []caddyhttp.ResponseHandler `json:"handle_response,omitempty"`

	// Holds the named response matchers from the Caddyfile while adapting
	responseMatchers map[string]caddyhttp.ResponseMatcher

	// Holds the handle_response Caddyfile tokens while adapting
	handleResponseSegments []*caddyfile.Dispenser

	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
//
// EXPERIMENTAL: Subject to change or removal.
func (Intercept) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.intercept",
		New: func() caddy.Module { return new(Intercept) },
	}
}

// Provision ensures that i is set up properly before use.
//
// EXPERIMENTAL: Subject to change or removal.
func (irh *Intercept) Provision(ctx caddy.Context) error {
	// set up any response routes
	for i, rh := range irh.HandleResponse {
		err := rh.Provision(ctx)
		if err != nil {
			return fmt.Errorf("provisioning response handler %d: %w", i, err)
		}
	}

	irh.logger = ctx.Logger()

	return nil
}

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// EXPERIMENTAL: Subject to change or removal.
type interceptedResponseHandler struct {
	caddyhttp.ResponseRecorder
	handler      caddyhttp.ResponseHandler
	handlerIndex int
}

// EXPERIMENTAL: Subject to change or removal.
func (irh interceptedResponseHandler) Unwrap() http.ResponseWriter {
	return irh.ResponseRecorder
}

// EXPERIMENTAL: Subject to change or removal.
func (ir Intercept) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	rec := interceptedResponseHandler{}
	rec.ResponseRecorder = caddyhttp.NewResponseRecorder(w, buf, func(status int, header http.Header) bool {
		// see if any response handler is configured for this original response
		for i, rh := range ir.HandleResponse {
			if rh.Match != nil && !rh.Match.Match(status, header) {
				continue
			}
			rec.handler = rh
			rec.handlerIndex = i

			// a matching response handler buffers the final response so it
			// can be replaced or modified before it reaches the client;
			// informational responses (including the 101 WebSocket upgrade)
			// are always passed through untouched
			return status < 100 || status > 199
		}

		return false
	})

	if err := next.ServeHTTP(rec, r); err != nil {
		return err
	}
	// the recorder only buffers when a response handler matched the final
	// response; unmatched, informational (1xx), and hijacked/streamed
	// responses (e.g. WebSocket upgrades) are passed through unchanged
	if !rec.Buffered() {
		return nil
	}

	// set up the replacer so that parts of the original response can be
	// used for routing decisions
	for field, value := range rec.Header() {
		repl.Set("http.intercept.header."+field, strings.Join(value, ","))
	}
	repl.Set("http.intercept.status_code", rec.Status())

	if c := ir.logger.Check(zapcore.DebugLevel, "handling response"); c != nil {
		c.Write(zap.Int("handler", rec.handlerIndex))
	}

	// if configured to only change the status code, validate it now that
	// intercept placeholders are available; substitute the final status
	// code and replay the original headers and body unchanged
	if rec.handler.Routes == nil {
		finalStatus := rec.Status()
		if statusCodeStr := rec.handler.StatusCode.String(); statusCodeStr != "" {
			sc, ok := parseReplacementStatus(repl, statusCodeStr)
			if ok {
				finalStatus = sc
			} else {
				// invalid status code: keep the original headers and body,
				// respond with an internal server error
				ir.logger.Error("invalid replacement status code",
					zap.String("status_code", statusCodeStr),
					zap.Int("original_status_code", rec.Status()))
				finalStatus = http.StatusInternalServerError
			}
		}
		return writeInterceptedResponse(w, finalStatus, buf.Bytes())
	}

	// response recorder doesn't create a new copy of the original headers, they're
	// present in the original response writer and shared with the new recorder.
	// buffer the replacement response too so we can reconcile its status, headers,
	// and body with the parts of the original response that are preserved
	newBuf := bufPool.Get().(*bytes.Buffer)
	newBuf.Reset()
	defer bufPool.Put(newBuf)

	recorder := caddyhttp.NewResponseRecorder(w, newBuf, func(status int, _ http.Header) bool {
		// informational responses are not final and are passed through
		return status < 100 || status > 199
	})
	if err := rec.handler.Routes.Compile(emptyHandler).ServeHTTP(recorder, r); err != nil {
		return err
	}
	// the replacement routes streamed their response directly (an
	// informational status such as a 101 WebSocket upgrade, or a hijacked
	// connection); it is already on the wire, so do not write again
	if !recorder.Buffered() {
		return nil
	}

	finalHeader := w.Header()

	// a final (non-informational) status code written by the replacement
	// routes means they take over the response; otherwise the original
	// status code and body are preserved and only header changes apply
	// (the header-only and no-action cases)
	if newStatus := recorder.Status(); newStatus >= 200 && newStatus <= 599 {
		// the framing headers describe the original body; the replacement
		// body is fully buffered and known-length, so drop the original
		// Transfer-Encoding (it must not be duplicated nor trigger chunking)
		// and recompute Content-Length from the new body
		finalHeader.Del("Transfer-Encoding")

		if bodyAllowedForStatus(newStatus) {
			newBody := newBuf.Bytes()
			// always send an explicit Content-Length, including for a
			// deliberately empty replacement body such as `respond 200`;
			// for HEAD requests the body is suppressed by the underlying
			// ResponseWriter, but the Content-Length header is kept
			finalHeader.Set("Content-Length", strconv.Itoa(len(newBody)))
			w.WriteHeader(newStatus)
			_, err := w.Write(newBody)
			return err
		}

		// 204 and 304 responses never carry a body
		finalHeader.Del("Content-Length")
		w.WriteHeader(newStatus)
		return nil
	}

	// no replacement response was written: keep the original status,
	// headers (plus any the routes added), and body exactly as buffered
	origStatus := rec.Status()
	if origStatus < 200 || origStatus > 599 {
		origStatus = http.StatusOK
	}
	if !bodyAllowedForStatus(origStatus) {
		finalHeader.Del("Content-Length")
		w.WriteHeader(origStatus)
		return nil
	}
	origBody := buf.Bytes()
	// the original body is fully buffered, so unless it declared its own
	// Content-Length or used a Transfer-Encoding, send its exact length
	// explicitly instead of letting an early WriteHeader force chunking
	if finalHeader.Get("Content-Length") == "" && finalHeader.Get("Transfer-Encoding") == "" {
		finalHeader.Set("Content-Length", strconv.Itoa(len(origBody)))
	}
	w.WriteHeader(origStatus)
	if len(origBody) == 0 {
		return nil
	}
	_, err := w.Write(origBody)
	return err
}

// writeInterceptedResponse writes status and body to w, omitting the body
// for status codes that do not allow one. It does not otherwise modify the
// response headers, which are written through the shared header map.
func writeInterceptedResponse(w http.ResponseWriter, status int, body []byte) error {
	if !bodyAllowedForStatus(status) {
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		return nil
	}
	// the body is fully buffered; advertise its length explicitly when the
	// original response did not declare framing so it is not chunked
	if w.Header().Get("Content-Length") == "" && w.Header().Get("Transfer-Encoding") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// parseReplacementStatus expands statusCodeStr using repl and parses it as
// an HTTP status code. It returns false if placeholder expansion fails, the
// result is not numeric, or the code is outside the valid 200-599 range of
// final response status codes.
func parseReplacementStatus(repl *caddy.Replacer, statusCodeStr string) (int, bool) {
	expanded, err := repl.ReplaceOrErr(statusCodeStr, true, true)
	if err != nil {
		return 0, false
	}
	sc, err := strconv.Atoi(expanded)
	if err != nil {
		return 0, false
	}
	if sc < 200 || sc > 599 {
		return 0, false
	}
	return sc, true
}

// bodyAllowedForStatus reports whether a response with the given status
// code is permitted to carry a body. It mirrors the logic of the same name
// in the standard library's net/http package.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent:
		return false
	case status == http.StatusNotModified:
		return false
	}
	return true
}

// this handler does nothing because everything we need is already buffered
var emptyHandler caddyhttp.Handler = caddyhttp.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) error {
	return nil
})

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens. Syntax:
//
//	intercept [<matcher>] {
//	    # intercept original responses
//	    @name {
//	        status <code...>
//	        header <field> [<value>]
//	    }
//	    replace_status [<matcher>] <status_code>
//	    handle_response [<matcher>] {
//	        <directives...>
//	    }
//	}
//
// The FinalizeUnmarshalCaddyfile method should be called after this
// to finalize parsing of "handle_response" blocks, if possible.
//
// EXPERIMENTAL: Subject to change or removal.
func (i *Intercept) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// collect the response matchers defined as subdirectives
	// prefixed with "@" for use with "handle_response" blocks
	i.responseMatchers = make(map[string]caddyhttp.ResponseMatcher)

	d.Next() // consume the directive name
	for d.NextBlock(0) {
		// if the subdirective has an "@" prefix then we
		// parse it as a response matcher for use with "handle_response"
		if strings.HasPrefix(d.Val(), matcherPrefix) {
			err := caddyhttp.ParseNamedResponseMatcher(d.NewFromNextSegment(), i.responseMatchers)
			if err != nil {
				return err
			}
			continue
		}

		switch d.Val() {
		case "handle_response":
			// delegate the parsing of handle_response to the caller,
			// since we need the httpcaddyfile.Helper to parse subroutes.
			// See h.FinalizeUnmarshalCaddyfile
			i.handleResponseSegments = append(i.handleResponseSegments, d.NewFromNextSegment())

		case "replace_status":
			args := d.RemainingArgs()
			if len(args) != 1 && len(args) != 2 {
				return d.Errf("must have one or two arguments: an optional response matcher, and a status code")
			}

			responseHandler := caddyhttp.ResponseHandler{}

			if len(args) == 2 {
				if !strings.HasPrefix(args[0], matcherPrefix) {
					return d.Errf("must use a named response matcher, starting with '@'")
				}
				foundMatcher, ok := i.responseMatchers[args[0]]
				if !ok {
					return d.Errf("no named response matcher defined with name '%s'", args[0][1:])
				}
				responseHandler.Match = &foundMatcher
				responseHandler.StatusCode = caddyhttp.WeakString(args[1])
			} else if len(args) == 1 {
				responseHandler.StatusCode = caddyhttp.WeakString(args[0])
			}

			// make sure there's no block, cause it doesn't make sense
			if nesting := d.Nesting(); d.NextBlock(nesting) {
				return d.Errf("cannot define routes for 'replace_status', use 'handle_response' instead.")
			}

			i.HandleResponse = append(
				i.HandleResponse,
				responseHandler,
			)

		default:
			return d.Errf("unrecognized subdirective %s", d.Val())
		}
	}

	return nil
}

// FinalizeUnmarshalCaddyfile finalizes the Caddyfile parsing which
// requires having an httpcaddyfile.Helper to function, to parse subroutes.
//
// EXPERIMENTAL: Subject to change or removal.
func (i *Intercept) FinalizeUnmarshalCaddyfile(helper httpcaddyfile.Helper) error {
	for _, d := range i.handleResponseSegments {
		// consume the "handle_response" token
		d.Next()
		args := d.RemainingArgs()

		// TODO: Remove this check at some point in the future
		if len(args) == 2 {
			return d.Errf("configuring 'handle_response' for status code replacement is no longer supported. Use 'replace_status' instead.")
		}

		if len(args) > 1 {
			return d.Errf("too many arguments for 'handle_response': %s", args)
		}

		var matcher *caddyhttp.ResponseMatcher
		if len(args) == 1 {
			// the first arg should always be a matcher.
			if !strings.HasPrefix(args[0], matcherPrefix) {
				return d.Errf("must use a named response matcher, starting with '@'")
			}

			foundMatcher, ok := i.responseMatchers[args[0]]
			if !ok {
				return d.Errf("no named response matcher defined with name '%s'", args[0][1:])
			}
			matcher = &foundMatcher
		}

		// parse the block as routes
		handler, err := httpcaddyfile.ParseSegmentAsSubroute(helper.WithDispenser(d.NewFromNextSegment()))
		if err != nil {
			return err
		}
		subroute, ok := handler.(*caddyhttp.Subroute)
		if !ok {
			return helper.Errf("segment was not parsed as a subroute")
		}
		i.HandleResponse = append(
			i.HandleResponse,
			caddyhttp.ResponseHandler{
				Match:  matcher,
				Routes: subroute.Routes,
			},
		)
	}

	// move the handle_response entries without a matcher to the end.
	// we can't use sort.SliceStable because it will reorder the rest of the
	// entries which may be undesirable because we don't have a good
	// heuristic to use for sorting.
	withoutMatchers := []caddyhttp.ResponseHandler{}
	withMatchers := []caddyhttp.ResponseHandler{}
	for _, hr := range i.HandleResponse {
		if hr.Match == nil {
			withoutMatchers = append(withoutMatchers, hr)
		} else {
			withMatchers = append(withMatchers, hr)
		}
	}
	i.HandleResponse = append(withMatchers, withoutMatchers...)

	// clean up the bits we only needed for adapting
	i.handleResponseSegments = nil
	i.responseMatchers = nil

	return nil
}

const matcherPrefix = "@"

func parseCaddyfile(helper httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var ir Intercept
	if err := ir.UnmarshalCaddyfile(helper.Dispenser); err != nil {
		return nil, err
	}

	if err := ir.FinalizeUnmarshalCaddyfile(helper); err != nil {
		return nil, err
	}

	return ir, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Intercept)(nil)
	_ caddyfile.Unmarshaler       = (*Intercept)(nil)
	_ caddyhttp.MiddlewareHandler = (*Intercept)(nil)
)
