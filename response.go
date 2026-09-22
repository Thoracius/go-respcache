// Package respcache is a small HTTP response layer that decides cache policy
// in one place: ETag, Last-Modified, Cache-Control/max-age, gzip, conditional
// 304 responses, and an optional server-side cache of the finished response.
//
// The idea is lifted from the Flashdance PHP framework's Response class:
// a handler buffers its body into a Response, calls LastModified() as many
// times as it likes (the latest timestamp wins), sets a cache duration, and
// the package writes headers exactly once, at the end, when it knows the
// whole body. Because the body is known, the ETag is exact, and a matching
// If-None-Match or If-Modified-Since turns into a 304 without sending bytes.
//
// With a server Cache configured on the Handler, the finished response
// (already gzipped) is kept in memory, and the next matching request is
// answered before the handler runs at all.
package respcache

import (
	"bytes"
	"compress/gzip"
	"hash/crc32"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Response wraps an http.ResponseWriter, buffering everything the handler
// writes so that headers can be decided once the full body is known.
// It implements http.ResponseWriter, so it can be passed to any handler.
//
// Nothing is sent to the client until Finish is called (Handler does this
// for you).
type Response struct {
	w http.ResponseWriter
	r *http.Request

	body   bytes.Buffer
	status int

	lastModified time.Time
	browserTTL   time.Duration
	serverTTL    time.Duration
	noStore      bool
	contentType  string
	vary         []string
	minGzip      int
	gzipLevel    int

	finished bool
}

// Option configures a Response created by New or Handle.
type Option func(*Response)

// WithMinGzipSize sets the body size below which gzip is skipped. Tiny
// bodies usually grow when compressed. Default 1024 bytes.
func WithMinGzipSize(n int) Option { return func(r *Response) { r.minGzip = n } }

// WithGzipLevel sets the compress/gzip level. Default gzip.DefaultCompression.
func WithGzipLevel(level int) Option { return func(r *Response) { r.gzipLevel = level } }

// New wraps w for request r. Most callers will use Handle instead.
func New(w http.ResponseWriter, r *http.Request, opts ...Option) *Response {
	resp := &Response{
		w:         w,
		r:         r,
		status:    http.StatusOK,
		minGzip:   1024,
		gzipLevel: gzip.DefaultCompression,
	}
	for _, o := range opts {
		o(resp)
	}
	return resp
}

// From returns the *Response behind w, or nil if w is not one. Useful in
// plain http.Handler code running under Handle.
func From(w http.ResponseWriter) *Response {
	r, _ := w.(*Response)
	return r
}

// ---- http.ResponseWriter -------------------------------------------------

// Header returns the header map that will be sent when the response is
// finished. Cache-related headers set here are overwritten by the
// Response's own policy.
func (r *Response) Header() http.Header { return r.w.Header() }

// Write buffers b. It never fails.
func (r *Response) Write(b []byte) (int, error) { return r.body.Write(b) }

// WriteHeader records the status code. It does not send anything yet.
func (r *Response) WriteHeader(code int) { r.status = code }

// ---- policy setters (chainable) ------------------------------------------

// LastModified records a modification time. It may be called several times
// (once per component that makes up the page); the latest time wins.
func (r *Response) LastModified(t time.Time) *Response {
	if t.After(r.lastModified) {
		r.lastModified = t
	}
	return r
}

// Cache sets both the browser max-age and the server-side cache TTL to d.
func (r *Response) Cache(d time.Duration) *Response {
	r.browserTTL, r.serverTTL, r.noStore = d, d, false
	return r
}

// CacheUntil is Cache with an absolute expiry.
func (r *Response) CacheUntil(t time.Time) *Response {
	return r.Cache(time.Until(t))
}

// BrowserCache sets only the browser max-age.
func (r *Response) BrowserCache(d time.Duration) *Response {
	r.browserTTL, r.noStore = d, false
	return r
}

// ServerCache sets only the server-side cache TTL. It has no effect unless
// the Response was created by a Handler with a Cache configured.
func (r *Response) ServerCache(d time.Duration) *Response {
	r.serverTTL = d
	return r
}

// NoStore marks the response as uncacheable anywhere: no server cache, and
// Cache-Control: no-store to the browser. ETag/304 handling is still done.
func (r *Response) NoStore() *Response {
	r.noStore, r.browserTTL, r.serverTTL = true, 0, 0
	return r
}

// ContentType sets Content-Type. Short names html, json, xml, txt, js and
// css are expanded; anything containing "/" is used verbatim. A charset is
// added for text types if none is given.
func (r *Response) ContentType(ct string) *Response {
	r.contentType = expandContentType(ct)
	return r
}

// Vary adds request header names the response depends on. They are sent
// in the Vary header alongside Accept-Encoding (added automatically when
// a gzipped variant exists).
func (r *Response) Vary(headers ...string) *Response {
	for _, h := range headers {
		r.vary = append(r.vary, http.CanonicalHeaderKey(h))
	}
	return r
}

// Status sets the status code (same as WriteHeader, but chainable).
func (r *Response) Status(code int) *Response {
	r.status = code
	return r
}

// Body returns the buffered body so far.
func (r *Response) Body() []byte { return r.body.Bytes() }

// ---- finishing -------------------------------------------------------------

// Finish decides headers and writes the response to the underlying writer.
// It returns the built Entry (nil if the response is not server-cacheable)
// so a caller can store it. Calling Finish twice is a no-op.
func (r *Response) Finish() *Entry {
	if r.finished {
		return nil
	}
	r.finished = true

	// Only 200 responses get the full treatment. Anything else is passed
	// through as-is; the handler owns its headers in that case.
	if r.status != http.StatusOK {
		r.w.WriteHeader(r.status)
		if r.r.Method != http.MethodHead {
			r.w.Write(r.body.Bytes()) //nolint:errcheck
		}
		return nil
	}

	e := &Entry{
		Body:         r.body.Bytes(),
		ContentType:  r.contentType,
		LastModified: r.lastModified.Truncate(time.Second),
		BrowserTTL:   r.browserTTL,
		NoStore:      r.noStore,
		Vary:         r.vary,
	}
	if e.ContentType == "" {
		e.ContentType = r.w.Header().Get("Content-Type")
	}
	if e.ContentType == "" {
		e.ContentType = http.DetectContentType(e.Body)
	}
	e.ETag = etagFor(e.Body)
	if len(e.Body) >= r.minGzip {
		e.Gzipped = gzipBytes(e.Body, r.gzipLevel)
	}
	if r.serverTTL > 0 && !r.noStore {
		e.Expires = time.Now().Add(r.serverTTL)
	}

	e.WriteTo(r.w, r.r)

	if e.Expires.IsZero() {
		return nil
	}
	return e
}

// Entry is a finished response, ready to be sent to any request for the
// same resource.
type Entry struct {
	Body         []byte
	Gzipped      []byte // nil if compression was skipped or didn't help
	ContentType  string
	ETag         string
	LastModified time.Time     // zero if unknown
	BrowserTTL   time.Duration // max-age to send
	NoStore      bool
	Vary         []string  // extra Vary header values
	Expires      time.Time // when the server-side copy stops being valid; zero = don't store
}

// Expired reports whether the server-side copy should no longer be served.
func (e *Entry) Expired(now time.Time) bool {
	return e.Expires.IsZero() || !now.Before(e.Expires)
}

// WriteTo sends the entry to w for request r: a 304 if the request's
// validators match, otherwise the full body (gzipped when the client
// accepts it and a compressed copy exists). Headers are written once.
func (e *Entry) WriteTo(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("ETag", e.ETag)
	if !e.LastModified.IsZero() {
		h.Set("Last-Modified", e.LastModified.UTC().Format(http.TimeFormat))
	}
	switch {
	case e.NoStore:
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Del("Expires")
	case e.BrowserTTL > 0:
		secs := int64(e.BrowserTTL / time.Second)
		h.Set("Cache-Control", "public, max-age="+strconv.FormatInt(secs, 10))
		h.Set("Expires", time.Now().Add(e.BrowserTTL).UTC().Format(http.TimeFormat))
	default:
		// Not told anything: let the browser keep a copy but revalidate.
		h.Set("Cache-Control", "no-cache")
		h.Del("Expires")
	}
	for _, v := range e.Vary {
		h.Add("Vary", v)
	}
	if e.Gzipped != nil {
		h.Add("Vary", "Accept-Encoding")
	}

	if notModified(r, e.ETag, e.LastModified) {
		// A 304 must not carry headers that describe a body.
		h.Del("Content-Type")
		h.Del("Content-Length")
		h.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := e.Body
	if e.Gzipped != nil && acceptsGzip(r) {
		body = e.Gzipped
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", e.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body) //nolint:errcheck
	}
}

// ---- helpers ---------------------------------------------------------------

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// etagFor builds a strong ETag from the body. CRC32-C is hardware
// accelerated on modern CPUs; combining it with the length makes accidental
// collisions between two versions of the same page vanishingly unlikely for
// cache-validation purposes.
func etagFor(body []byte) string {
	sum := crc32.Checksum(body, castagnoli)
	return `"` + strconv.FormatInt(int64(len(body)), 36) + "-" + strconv.FormatUint(uint64(sum), 36) + `"`
}

func gzipBytes(b []byte, level int) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		zw = gzip.NewWriter(&buf)
	}
	zw.Write(b) //nolint:errcheck
	zw.Close()
	if buf.Len() >= len(b) {
		return nil // compression didn't help
	}
	return buf.Bytes()
}

// acceptsGzip parses Accept-Encoding, honouring q=0 to disable gzip.
func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc = strings.TrimSpace(enc)
		if i := strings.IndexByte(enc, ';'); i >= 0 {
			params := strings.ReplaceAll(enc[i+1:], " ", "")
			enc = strings.TrimSpace(enc[:i])
			if q, ok := strings.CutPrefix(params, "q="); ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					continue
				}
			}
		}
		if enc == "gzip" || enc == "*" {
			return true
		}
	}
	return false
}

// notModified implements RFC 9110 §13.1: If-None-Match takes precedence
// over If-Modified-Since. Comparison is weak (W/ prefixes ignored), which is
// what If-None-Match on GET/HEAD calls for.
func notModified(r *http.Request, etag string, lastMod time.Time) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, tag := range strings.Split(inm, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(etag, "W/") {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !lastMod.IsZero() {
		if t, err := http.ParseTime(ims); err == nil && !lastMod.After(t) {
			return true
		}
	}
	return false
}

func expandContentType(ct string) string {
	if strings.Contains(ct, "/") {
		if strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "charset") {
			ct += "; charset=utf-8"
		}
		return ct
	}
	switch strings.ToLower(ct) {
	case "html":
		return "text/html; charset=utf-8"
	case "txt", "text", "plain":
		return "text/plain; charset=utf-8"
	case "css":
		return "text/css; charset=utf-8"
	case "js", "javascript":
		return "text/javascript; charset=utf-8"
	case "json":
		return "application/json"
	case "xml":
		return "application/xml"
	case "xhtml":
		return "application/xhtml+xml"
	}
	return ct
}
