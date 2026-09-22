package respcache_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Thoracius/go-respcache"
)

// A page is built from two parts with different modification times. The
// Response accumulates them, and the latest one becomes Last-Modified.
func ExampleHandleFunc() {
	postUpdated := time.Date(2017, 3, 24, 15, 1, 0, 0, time.UTC)
	menuUpdated := time.Date(2017, 1, 10, 9, 0, 0, 0, time.UTC)

	cache := respcache.NewMemoryCache()
	cache.MaxBytes = 32 << 20

	calls := 0
	page := respcache.HandleFunc(func(w *respcache.Response, r *http.Request) {
		calls++
		w.ContentType("html").
			LastModified(menuUpdated).
			LastModified(postUpdated).
			Cache(10 * time.Minute)
		io.WriteString(w, "<h1>Hello</h1>"+strings.Repeat("<p>body</p>", 200))
	}, respcache.WithCache(cache))

	// First request: handler runs, full body sent.
	rec := httptest.NewRecorder()
	page.ServeHTTP(rec, httptest.NewRequest("GET", "/hello", nil))
	fmt.Println(rec.Code, rec.Header().Get("Last-Modified"))
	fmt.Println(rec.Header().Get("Cache-Control"))
	etag := rec.Header().Get("ETag")

	// Second request with the ETag: served from the cache as a 304, and
	// the handler is not called.
	req := httptest.NewRequest("GET", "/hello", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	page.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Body.Len(), "bytes")

	// Third request accepting gzip: still from cache, compressed copy sent.
	req = httptest.NewRequest("GET", "/hello", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	page.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Header().Get("Content-Encoding"))

	fmt.Println("handler ran", calls, "time(s)")

	// Output:
	// 200 Fri, 24 Mar 2017 15:01:00 GMT
	// public, max-age=600
	// 304 0 bytes
	// 200 gzip
	// handler ran 1 time(s)
}

// Plain http.Handlers can be wrapped too; use From to reach the Response.
func ExampleFrom() {
	h := respcache.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resp := respcache.From(w); resp != nil {
			resp.BrowserCache(time.Hour)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api", nil))
	fmt.Println(rec.Header().Get("Cache-Control"))
	fmt.Println(rec.Header().Get("Content-Type"))
	fmt.Println(rec.Body.String())

	// Output:
	// public, max-age=3600
	// application/json
	// {"ok":true}
}
