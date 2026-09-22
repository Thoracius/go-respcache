package respcache

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var bigBody = strings.Repeat("<p>Hello, cached world.</p>\n", 200)

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return do(h, http.MethodGet, path, hdr)
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func page(mod time.Time, ttl time.Duration) HandlerFunc {
	return func(w *Response, r *http.Request) {
		w.ContentType("html").LastModified(mod).Cache(ttl)
		io.WriteString(w, bigBody)
	}
}

func TestBasicHeaders(t *testing.T) {
	mod := time.Date(2017, 3, 24, 15, 1, 0, 0, time.UTC)
	h := HandleFunc(page(mod, time.Hour))
	rec := get(h, "/", nil)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if got := rec.Header().Get("Last-Modified"); got != "Fri, 24 Mar 2017 15:01:00 GMT" {
		t.Errorf("Last-Modified %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag")
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzipped without Accept-Encoding")
	}
	if rec.Body.String() != bigBody {
		t.Error("body mismatch")
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(bigBody)) {
		t.Errorf("Content-Length %q", got)
	}
}

func TestGzip(t *testing.T) {
	h := HandleFunc(page(time.Time{}, 0))
	rec := get(h, "/", map[string]string{"Accept-Encoding": "gzip, deflate, br"})

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip")
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("missing Vary")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if string(raw) != bigBody {
		t.Error("gunzipped body mismatch")
	}
	if rec.Body.Len() >= len(bigBody) {
		t.Error("gzip did not shrink body")
	}
	// Last-Modified absent when never set.
	if rec.Header().Get("Last-Modified") != "" {
		t.Error("unexpected Last-Modified")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control %q", got)
	}
}

func TestGzipQZero(t *testing.T) {
	h := HandleFunc(page(time.Time{}, 0))
	rec := get(h, "/", map[string]string{"Accept-Encoding": "gzip;q=0, identity"})
	if rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip served despite q=0")
	}
}

func TestSmallBodyNotGzipped(t *testing.T) {
	h := HandleFunc(func(w *Response, r *http.Request) {
		io.WriteString(w, "tiny")
	})
	rec := get(h, "/", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" {
		t.Error("small body should not be gzipped")
	}
}

func TestIfNoneMatch(t *testing.T) {
	h := HandleFunc(page(time.Time{}, 0))
	first := get(h, "/", nil)
	etag := first.Header().Get("ETag")

	for _, inm := range []string{etag, "W/" + etag, `"nope", ` + etag, "*"} {
		rec := get(h, "/", map[string]string{"If-None-Match": inm})
		if rec.Code != 304 {
			t.Errorf("If-None-Match %q: got %d", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Error("304 with body")
		}
		if rec.Header().Get("Content-Length") != "" || rec.Header().Get("Content-Type") != "" {
			t.Error("304 carries entity headers")
		}
		if rec.Header().Get("ETag") != etag {
			t.Error("304 missing ETag")
		}
	}
	rec := get(h, "/", map[string]string{"If-None-Match": `"different"`})
	if rec.Code != 200 {
		t.Errorf("mismatched ETag: got %d", rec.Code)
	}
}

func TestIfModifiedSince(t *testing.T) {
	mod := time.Date(2017, 3, 24, 15, 1, 0, 0, time.UTC)
	h := HandleFunc(page(mod, 0))

	cases := map[string]int{
		mod.Format(http.TimeFormat):                   304, // equal -> not modified
		mod.Add(time.Hour).Format(http.TimeFormat):    304,
		mod.Add(-time.Second).Format(http.TimeFormat): 200,
		"garbage": 200,
	}
	for ims, want := range cases {
		rec := get(h, "/", map[string]string{"If-Modified-Since": ims})
		if rec.Code != want {
			t.Errorf("If-Modified-Since %q: got %d want %d", ims, rec.Code, want)
		}
	}

	// If-None-Match wins over If-Modified-Since.
	rec := get(h, "/", map[string]string{
		"If-Modified-Since": mod.Format(http.TimeFormat),
		"If-None-Match":     `"stale"`,
	})
	if rec.Code != 200 {
		t.Errorf("INM should override IMS, got %d", rec.Code)
	}
}

func TestConditionalIgnoredOnPost(t *testing.T) {
	h := HandleFunc(page(time.Time{}, 0))
	etag := get(h, "/", nil).Header().Get("ETag")
	rec := do(h, http.MethodPost, "/", map[string]string{"If-None-Match": etag})
	if rec.Code != 200 {
		t.Errorf("POST got %d", rec.Code)
	}
}

func TestLastModifiedAccumulates(t *testing.T) {
	a := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	b := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	h := HandleFunc(func(w *Response, r *http.Request) {
		w.LastModified(b).LastModified(a).LastModified(a.Add(-time.Hour))
		io.WriteString(w, "x")
	})
	rec := get(h, "/", nil)
	if got := rec.Header().Get("Last-Modified"); got != b.Format(http.TimeFormat) {
		t.Errorf("Last-Modified %q, want latest", got)
	}
}

func TestNoStore(t *testing.T) {
	h := HandleFunc(func(w *Response, r *http.Request) {
		w.Cache(time.Hour).NoStore()
		io.WriteString(w, "secret")
	})
	rec := get(h, "/", nil)
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control %q", got)
	}
	if rec.Header().Get("Expires") != "" {
		t.Error("Expires set on no-store")
	}
}

func TestNon200Passthrough(t *testing.T) {
	h := HandleFunc(func(w *Response, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.Cache(time.Hour)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "nope")
	})
	rec := get(h, "/", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != 404 || rec.Body.String() != "nope" {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "" {
		t.Error("cache headers on non-200")
	}
	if rec.Header().Get("X-Custom") != "yes" {
		t.Error("handler header lost")
	}
}

func TestHead(t *testing.T) {
	h := HandleFunc(page(time.Time{}, 0))
	rec := do(h, http.MethodHead, "/", nil)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD got %d body=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(len(bigBody)) {
		t.Error("HEAD Content-Length wrong")
	}
}

func TestServerCache(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.ContentType("html").Cache(time.Hour)
		io.WriteString(w, bigBody)
	}, WithCache(cache))

	r1 := get(h, "/page", nil)
	r2 := get(h, "/page", map[string]string{"Accept-Encoding": "gzip"})
	r3 := get(h, "/page", map[string]string{"If-None-Match": r1.Header().Get("ETag")})
	get(h, "/other", nil)

	if calls != 2 {
		t.Fatalf("handler called %d times, want 2 (one per distinct path)", calls)
	}
	if r1.Body.String() != bigBody {
		t.Error("first response body")
	}
	if r2.Header().Get("Content-Encoding") != "gzip" {
		t.Error("cached hit should serve gzip when accepted")
	}
	zr, _ := gzip.NewReader(r2.Body)
	raw, _ := io.ReadAll(zr)
	if !bytes.Equal(raw, []byte(bigBody)) {
		t.Error("cached gzip body mismatch")
	}
	if r3.Code != 304 {
		t.Errorf("cached hit with matching ETag got %d", r3.Code)
	}
	if cache.Len() != 2 {
		t.Errorf("cache has %d entries", cache.Len())
	}

	h.Invalidate("/page")
	get(h, "/page", nil)
	if calls != 3 {
		t.Errorf("after invalidate, handler called %d times, want 3", calls)
	}
}

func TestServerCacheSkipsUncacheable(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.BrowserCache(time.Hour) // browser only, no server TTL
		io.WriteString(w, "x")
	}, WithCache(cache))
	get(h, "/", nil)
	get(h, "/", nil)
	if calls != 2 || cache.Len() != 0 {
		t.Errorf("calls=%d len=%d", calls, cache.Len())
	}

	// POST never cached even with Cache().
	calls = 0
	h2 := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Cache(time.Hour)
		io.WriteString(w, "x")
	}, WithCache(cache))
	do(h2, http.MethodPost, "/p", nil)
	do(h2, http.MethodPost, "/p", nil)
	if calls != 2 || cache.Len() != 0 {
		t.Errorf("POST cached: calls=%d len=%d", calls, cache.Len())
	}
}

func TestServerCacheExpiry(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Cache(20 * time.Millisecond)
		io.WriteString(w, "x")
	}, WithCache(cache))
	get(h, "/", nil)
	get(h, "/", nil)
	time.Sleep(30 * time.Millisecond)
	get(h, "/", nil)
	if calls != 2 {
		t.Errorf("calls=%d, want 2", calls)
	}
}

func TestPlainHandlerWithFrom(t *testing.T) {
	h := Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resp := From(w); resp != nil {
			resp.Cache(time.Minute)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	rec := get(h, "/", nil)
	if rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Error("From() did not reach the Response")
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Error("handler-set Content-Type not honoured")
	}
}

func TestETagStability(t *testing.T) {
	if etagFor([]byte("a")) == etagFor([]byte("b")) {
		t.Error("etag collision on trivial input")
	}
	if etagFor([]byte("abc")) != etagFor([]byte("abc")) {
		t.Error("etag not deterministic")
	}
	if e := etagFor(nil); !strings.HasPrefix(e, `"`) || !strings.HasSuffix(e, `"`) {
		t.Errorf("etag not quoted: %s", e)
	}
}

func TestFinishIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := New(rec, httptest.NewRequest("GET", "/", nil))
	io.WriteString(resp, "x")
	resp.Finish()
	resp.Finish()
	if rec.Body.String() != "x" {
		t.Error("double Finish wrote twice")
	}
}

func TestLRUMaxEntries(t *testing.T) {
	c := NewMemoryCache()
	c.MaxEntries = 2
	entry := func() *Entry { return &Entry{Body: []byte("x"), Expires: time.Now().Add(time.Hour)} }

	c.Set("a", entry())
	c.Set("b", entry())
	c.Get("a") // a is now most recently used
	c.Set("c", entry())

	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted (least recently used)")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should still be cached", k)
		}
	}
	if c.Len() != 2 {
		t.Errorf("len %d", c.Len())
	}
}

func TestLRUMaxBytes(t *testing.T) {
	c := NewMemoryCache()
	c.MaxBytes = 100
	mk := func(n int) *Entry {
		return &Entry{Body: make([]byte, n), Expires: time.Now().Add(time.Hour)}
	}

	c.Set("big", mk(200)) // larger than the whole cache: never stored
	if c.Len() != 0 {
		t.Fatal("oversized entry stored")
	}
	c.Set("a", mk(40))
	c.Set("b", mk(40))
	c.Set("c", mk(40)) // 120 > 100 -> evict a
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted")
	}
	if c.Bytes() != 80 || c.Len() != 2 {
		t.Errorf("bytes=%d len=%d", c.Bytes(), c.Len())
	}

	c.Set("b", mk(10)) // replacing must not double count
	if c.Bytes() != 50 {
		t.Errorf("after replace bytes=%d", c.Bytes())
	}
	c.Delete("b")
	if c.Bytes() != 40 || c.Len() != 1 {
		t.Errorf("after delete bytes=%d len=%d", c.Bytes(), c.Len())
	}
	c.Flush()
	if c.Bytes() != 0 || c.Len() != 0 {
		t.Error("flush did not reset")
	}
}

func TestVaryCookies(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Cache(time.Hour)
		u := "anon"
		if c, err := r.Cookie("session"); err == nil {
			u = c.Value
		}
		io.WriteString(w, "hello "+u)
	}, WithCache(cache), WithVaryCookies("session"))

	alice := map[string]string{"Cookie": "session=alice; tracker=zzz"}
	bob := map[string]string{"Cookie": "tracker=yyy; session=bob"}
	aliceAgain := map[string]string{"Cookie": "session=alice; tracker=123"}

	r1 := get(h, "/", alice)
	r2 := get(h, "/", bob)
	r3 := get(h, "/", aliceAgain) // same session, different tracker -> cache hit
	r4 := get(h, "/", nil)

	if r1.Body.String() != "hello alice" || r2.Body.String() != "hello bob" ||
		r3.Body.String() != "hello alice" || r4.Body.String() != "hello anon" {
		t.Errorf("bodies: %q %q %q %q", r1.Body, r2.Body, r3.Body, r4.Body)
	}
	if calls != 3 {
		t.Errorf("handler called %d times, want 3", calls)
	}
	if got := r1.Header().Values("Vary"); len(got) != 1 || got[0] != "Cookie" {
		t.Errorf("Vary %v", got)
	}
	// Cached hit must carry Vary too.
	if got := r3.Header().Values("Vary"); len(got) != 1 || got[0] != "Cookie" {
		t.Errorf("cached Vary %v", got)
	}
}

func TestVaryHeaders(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Cache(time.Hour)
		io.WriteString(w, strings.Repeat(r.Header.Get("Accept-Language"), 2000))
	}, WithCache(cache), WithVaryHeaders("accept-language"))

	get(h, "/", map[string]string{"Accept-Language": "en", "Accept-Encoding": "gzip"})
	get(h, "/", map[string]string{"Accept-Language": "de"})
	rec := get(h, "/", map[string]string{"Accept-Language": "en"})
	if calls != 2 {
		t.Errorf("handler called %d times, want 2", calls)
	}
	vary := rec.Header().Values("Vary")
	if len(vary) != 2 || vary[0] != "Accept-Language" || vary[1] != "Accept-Encoding" {
		t.Errorf("Vary %v", vary)
	}

	// Key() matches what ServeHTTP used, so Invalidate works.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Language", "en")
	h.Invalidate(h.Key(req))
	get(h, "/", map[string]string{"Accept-Language": "en"})
	if calls != 3 {
		t.Errorf("after Invalidate handler called %d times, want 3", calls)
	}
}

func TestCancelledRequestNotCached(t *testing.T) {
	var calls int32
	cache := NewMemoryCache()
	h := HandleFunc(func(w *Response, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Cache(time.Hour)
		io.WriteString(w, "partial")
	}, WithCache(cache))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if cache.Len() != 0 {
		t.Error("response for cancelled request was cached")
	}
	get(h, "/", nil)
	if calls != 2 {
		t.Errorf("calls=%d, want 2", calls)
	}
}
