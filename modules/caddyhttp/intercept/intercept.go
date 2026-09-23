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
	"io"
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
	// be invoked. If it writes a new response (status and/or
	// body), that response replaces the original one; its framing
	// (Content-Length and Transfer-Encoding) is recomputed from the
	// new body. If it only modifies headers, the original status
	// code and body are preserved with the new headers layered on
	// top. If it does nothing, the original response is replayed
	// unchanged.
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

			// both "handle_response" routes and "replace_status" buffer
			// the original response so it can be replaced or replayed
			// with its original headers and body intact
			return true
		}

		return false
	})

	if err := next.ServeHTTP(rec, r); err != nil {
		return err
	}
	if !rec.Buffered() {
		return nil
	}
	// if the next handler hijacked the connection (e.g. a WebSocket
	// upgrade) without ever writing a status code, the connection now
	// belongs to it; don't try to write a replacement response
	if rec.Status() == 0 {
		return nil
	}
	// 1xx responses, including 101 Switching Protocols (e.g. WebSocket
	// upgrades), are written through immediately by the response recorder;
	// a final response never materialized, so there is nothing to replace.
	if rec.Status() >= 100 && rec.Status() <= 199 {
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

	// replace_status: substitute only the final status code; the original
	// headers and body are replayed untouched. The status code is expanded
	// after the intercept placeholders are registered, and must be a valid
	// final status code (200-599); anything else (1xx, out of range,
	// non-numeric, or a failed placeholder expansion) results in a 500 with
	// the original response unchanged.
	if statusCodeStr := rec.handler.StatusCode.String(); statusCodeStr != "" {
		sc, err := strconv.Atoi(repl.ReplaceAll(statusCodeStr, ""))
		if err != nil || sc < 200 || sc > 599 {
			sc = http.StatusInternalServerError
		}

		w.WriteHeader(sc)
		if bodyAllowedForStatus(sc) && buf.Len() > 0 {
			_, err := io.Copy(w, buf)
			return err
		}
		return nil
	}

	// a handle_response without routes (e.g. an empty block) produces no
	// replacement, so replay the original response verbatim
	if rec.handler.Routes == nil {
		w.WriteHeader(rec.Status())
		if bodyAllowedForStatus(rec.Status()) && buf.Len() > 0 {
			_, err := io.Copy(w, buf)
			return err
		}
		return nil
	}

	// handle_response: run the replacement routes against a buffering
	// recorder so that the response framing can be corrected before
	// anything reaches the client. The recorder shares the underlying
	// response writer's header map, which already contains the original
	// headers, so a header-only route simply layers on top of them.
	newBuf := bufPool.Get().(*bytes.Buffer)
	newBuf.Reset()
	defer bufPool.Put(newBuf)

	wroteFinalResponse := false
	recorder := caddyhttp.NewResponseRecorder(w, newBuf, func(status int, _ http.Header) bool {
		// 101 Switching Protocols, e.g. WebSocket upgrades, must pass
		// through immediately instead of being buffered.
		if status == http.StatusSwitchingProtocols {
			return false
		}
		// informational responses are written through immediately by the
		// recorder, so don't count them as the final response
		if status < 100 || status > 199 {
			wroteFinalResponse = true
		}
		return true
	})
	if err := rec.handler.Routes.Compile(emptyHandler).ServeHTTP(recorder, r); err != nil {
		return err
	}

	// a 101 upgrade was already written (and the connection may be
	// hijacked), so don't touch the response writer again
	if recorder.Status() == http.StatusSwitchingProtocols {
		return nil
	}

	// if the replacement routes didn't produce a final response (e.g. an
	// empty block or one that only sets headers or request vars), replay
	// the original status, headers and body verbatim; any header changes
	// the routes made are preserved in the shared header map
	if !wroteFinalResponse {
		w.WriteHeader(rec.Status())
		if bodyAllowedForStatus(rec.Status()) && buf.Len() > 0 {
			_, err := io.Copy(w, buf)
			return err
		}
		return nil
	}

	// a replacement response was produced. The original Content-Length and
	// Transfer-Encoding describe the old body (or may even be duplicated),
	// so drop them and recompute the framing from the new body to prevent
	// truncated, duplicated or otherwise misframed responses.
	header := w.Header()
	header.Del("Transfer-Encoding")
	header.Del("Content-Length")
	if newBuf.Len() > 0 {
		header.Set("Content-Length", strconv.Itoa(newBuf.Len()))
	}

	status := recorder.Status()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if bodyAllowedForStatus(status) && newBuf.Len() > 0 {
		_, err := w.Write(newBuf.Bytes())
		return err
	}
	return nil
}

// bodyAllowedForStatus reports whether a response with the given final
// status code is allowed to carry a body, mirroring the rules of the
// standard library (1xx, 204 and 304 responses never have one).
func bodyAllowedForStatus(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
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
