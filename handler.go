package respcache

import (
	"net/http"
	"strings"
)

// HandlerFunc is a handler that receives the wrapped *Response directly.
type HandlerFunc func(w *Response, r *http.Request)

// Handler runs an http.Handler through the full pipeline: server-cache
// lookup, buffered Response, then Finish.
type Handler struct {
	next        http.Handler
	cache       Cache
	key         func(*http.Request) string
	varyHeaders []string
	varyCookies []string
	opts        []Option
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithCache enables the server-side cache. Responses whose ServerCache
// duration is positive are stored and served on later matching GET/HEAD
// requests without invoking the handler.
func WithCache(c Cache) HandlerOption { return func(h *Handler) { h.cache = c } }

// WithKeyFunc replaces the base cache key function. The default is
// DefaultKey. Vary headers and cookies (see WithVaryHeaders and
// WithVaryCookies) are appended to whatever this returns.
func WithKeyFunc(f func(*http.Request) string) HandlerOption {
	return func(h *Handler) { h.key = f }
}

// WithVaryHeaders makes the server cache key include the value of each
// named request header, and adds them to the response's Vary header so
// browsers and proxies key the same way. Use for pages that differ by
// Accept-Language, a feature-flag header, and so on.
func WithVaryHeaders(names ...string) HandlerOption {
	return func(h *Handler) {
		for _, n := range names {
			h.varyHeaders = append(h.varyHeaders, http.CanonicalHeaderKey(n))
		}
	}
}

// WithVaryCookies makes the server cache key include the value of each
// named cookie (absent cookies key as empty), and adds "Cookie" to the
// response's Vary header. Use for pages that differ per session or user
// but are still worth caching per user. Unnamed cookies are ignored, so a
// stray analytics cookie doesn't fragment the cache.
func WithVaryCookies(names ...string) HandlerOption {
	return func(h *Handler) { h.varyCookies = append(h.varyCookies, names...) }
}

// WithResponseOptions passes options to every Response the handler creates.
func WithResponseOptions(opts ...Option) HandlerOption {
	return func(h *Handler) { h.opts = append(h.opts, opts...) }
}

// Handle wraps next. The handler receives a *Response as its
// http.ResponseWriter; use From to get at it.
func Handle(next http.Handler, opts ...HandlerOption) *Handler {
	h := &Handler{next: next, key: DefaultKey}
	for _, o := range opts {
		o(h)
	}
	return h
}

// HandleFunc wraps a HandlerFunc.
func HandleFunc(f HandlerFunc, opts ...HandlerOption) *Handler {
	return Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f(w.(*Response), r)
	}), opts...)
}

// DefaultKey keys the cache on path and query string.
func DefaultKey(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + r.URL.RawQuery
}

// Key returns the full server-cache key for r: the base key plus any vary
// headers and cookies. Use it with Invalidate.
func (h *Handler) Key(r *http.Request) string {
	if len(h.varyHeaders) == 0 && len(h.varyCookies) == 0 {
		return h.key(r)
	}
	var b strings.Builder
	b.WriteString(h.key(r))
	for _, name := range h.varyHeaders {
		b.WriteString("\x00h:")
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(r.Header.Get(name))
	}
	for _, name := range h.varyCookies {
		b.WriteString("\x00c:")
		b.WriteString(name)
		b.WriteByte('=')
		if c, err := r.Cookie(name); err == nil {
			b.WriteString(c.Value)
		}
	}
	return b.String()
}

// vary returns the Vary header values this handler adds to responses.
func (h *Handler) vary() []string {
	v := append([]string(nil), h.varyHeaders...)
	if len(h.varyCookies) > 0 {
		v = append(v, "Cookie")
	}
	return v
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cacheable := h.cache != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead)
	var key string
	if cacheable {
		key = h.Key(r)
		if e, ok := h.cache.Get(key); ok {
			e.WriteTo(w, r)
			return
		}
	}

	resp := New(w, r, h.opts...)
	resp.Vary(h.vary()...)
	h.next.ServeHTTP(resp, r)
	// If the handler panics we never reach Finish: nothing is cached and
	// the underlying writer is untouched for the server's recovery.
	e := resp.Finish()
	// A handler that noticed the client going away may have bailed out
	// with a partial body; never let that become the cached copy.
	if e != nil && cacheable && r.Context().Err() == nil {
		h.cache.Set(key, e)
	}
}

// Invalidate removes key from the handler's cache, if it has one. Build the
// key with Key, or with DefaultKey when no vary options are in use.
func (h *Handler) Invalidate(key string) {
	if h.cache != nil {
		h.cache.Delete(key)
	}
}
