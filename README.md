# respcache

[![test](https://github.com/Thoracius/go-respcache/actions/workflows/test.yml/badge.svg)](https://github.com/Thoracius/go-respcache/actions/workflows/test.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/Thoracius/go-respcache.svg)](https://pkg.go.dev/github.com/Thoracius/go-respcache)

Cache and compression are integral parts of the HTTP request/response
cycle but are typically treated as an afterthought in web app development.
Respcache bridges that gap, providing an approach integrating client- and
server-side caching and compression. 

An elegant Go HTTP response layer that decides cache policy in one place:
ETag, Last-Modified, Cache-Control/max-age, gzip, conditional 304 responses,
and an optional server-side cache of the finished response.

Zero dependencies, standard library only.

## How it works

This is a port of a PHP class I wrote years ago, unhappy with how most
bloated frameworks failed to offer an elegant solution. The idea is simple:
a handler writes its body into a `Response` instead of straight to the wire.
Along the way it can call `LastModified()` as many times as it likes (once
per component that makes up the page; the latest timestamp wins) and set a
cache duration. Only when the handler returns, with the whole body known,
does the package write headers, exactly once.

Because the body is known, the ETag is exact, and a matching `If-None-Match`
or `If-Modified-Since` turns into a 304 without sending any bytes. With a
server cache configured, the finished response (already gzipped) is kept in
memory, and the next matching request is answered before the handler runs at
all: a 304 on a cached hit costs a map lookup and a string compare.

Most Go routers leave all of this to you, and getting 304s right by hand means
computing an ETag from a body you have not finished writing yet. `respcache`
gives you:

- a strong ETag from the real body,
- `If-None-Match` / `If-Modified-Since` handled with a bodiless 304,
- gzip once, with the compressed copy remembered,
- `Cache-Control`, `Expires`, `Last-Modified`, `Content-Length` and `Vary`
  set consistently and written exactly once,
- an optional LRU server cache keyed by path, and by chosen headers or
  cookies when a page varies per user.

## Usage

```go
package main

import (
	"net/http"
	"time"

	"github.com/Thoracius/go-respcache"
)

func main() {
	cache := respcache.NewMemoryCache()

	article := respcache.HandleFunc(func(w *respcache.Response, r *http.Request) {
		post := loadPost(r.PathValue("slug"))
		menu := loadMenu()

		w.ContentType("html").
			LastModified(post.Updated). // called per component;
			LastModified(menu.Updated). // the latest timestamp wins
			Cache(10 * time.Minute)     // browser max-age AND server cache

		renderTemplate(w, post, menu) // w is an http.ResponseWriter
	}, respcache.WithCache(cache))

	mux := http.NewServeMux()
	mux.Handle("GET /post/{slug}", article)
	http.ListenAndServe(":8080", mux)
}
```

Plain `http.Handler`s work too; reach the wrapper with `respcache.From(w)`:

```go
h := respcache.Handle(existingHandler, respcache.WithCache(cache))

// inside existingHandler:
if resp := respcache.From(w); resp != nil {
	resp.BrowserCache(time.Hour)
}
```

### Policy methods (all chainable)

| Method | Effect |
| --- | --- |
| `LastModified(t)` | Records `t`; the latest of all calls is sent as `Last-Modified` and used for `If-Modified-Since`. |
| `Cache(d)` | `Cache-Control: public, max-age=d` **and** keep in server cache for `d`. |
| `CacheUntil(t)` | Same, with an absolute time. |
| `BrowserCache(d)` | Only the `max-age`. |
| `ServerCache(d)` | Only the in-memory copy. |
| `NoStore()` | `Cache-Control: no-store …`, never cached server-side. ETag/304 still work. |
| `ContentType("html")` | Short names `html json xml txt css js xhtml` are expanded; `"a/b"` is used as-is. |
| `Status(code)` | Same as `WriteHeader`. Non-200 responses are passed through untouched. |
| `Vary("Accept-Language")` | Adds names to the `Vary` header (the handler options below do this for you). |

If nothing is set, the response is sent with `Cache-Control: no-cache` (keep a
copy, but always revalidate), which still gets the 304 benefit.

### Options

```go
respcache.HandleFunc(f,
	respcache.WithCache(cache),                       // enable server cache
	respcache.WithKeyFunc(func(r *http.Request) string { // default: path + "?" + query
		return r.Host + r.URL.Path
	}),
	respcache.WithVaryHeaders("Accept-Language"),          // key on + Vary by these headers
	respcache.WithVaryCookies("session"),                  // key on these cookies, Vary: Cookie
	respcache.WithResponseOptions(
		respcache.WithMinGzipSize(2048),               // default 1024
		respcache.WithGzipLevel(gzip.BestSpeed),       // default gzip.DefaultCompression
	),
)
```

`Handler.Invalidate(h.Key(req))` drops one entry; `MemoryCache.Flush()` drops all.
Implement the three-method `Cache` interface to back it with something else.

### Bounding the cache

`MemoryCache` is an LRU. Unbounded by default; set either or both:

```go
cache := respcache.NewMemoryCache()
cache.MaxEntries = 1000
cache.MaxBytes = 64 << 20 // raw + gzipped bytes; an entry larger than this is never stored
```

## Behaviour notes

- Only `200` responses are processed. Anything else is written through as the
  handler produced it (no ETag, no gzip, no caching).
- Only `GET` and `HEAD` are served from / stored in the server cache, and
  conditional headers are only honoured on those methods.
- `If-None-Match` takes precedence over `If-Modified-Since` (RFC 9110 §13.1.3).
  Weak comparison; `*` and comma lists are handled.
- `Last-Modified` is truncated to whole seconds before comparison, so a
  client echoing the header back gets a 304.
- `Content-Type` falls back to whatever the handler set on the header map,
  then to `http.DetectContentType`.
- ETag is `"<len>-<crc32c>"` in base-36. CRC32-C is hardware accelerated
  and, combined with the length, more than enough to distinguish revisions of
  a page. It is not a content-addressing hash; don't use it as one.
- The cache key does **not** include `Accept-Encoding`; the entry stores both
  raw and gzipped bodies and picks per request. `Vary: Accept-Encoding` is
  added when a gzipped variant exists.
- By default the server cache ignores `Cookie`, `Authorization`, etc. If a
  page varies by user, either call `NoStore()` / `BrowserCache()` instead of
  `Cache()`, or use `WithVaryCookies` / `WithVaryHeaders` so each variant is
  cached separately and the `Vary` header tells downstream caches the same.
- If the request context is cancelled by the time the handler returns (client
  went away), the response is still written but not stored, since a handler
  that bailed early may have produced a partial body.
- If the handler panics, nothing is written and nothing is cached.
- Streaming responses (SSE, large downloads) are not a fit: the body is
  buffered in full. Don't wrap those handlers.
- The server cache is in-process memory. That is the right default for a
  single Go server, but it starts cold on restart and is not shared between
  instances. For a large site or several instances, put a caching reverse
  proxy or CDN in front (nginx, Varnish, Caddy, Cloudflare): the headers this
  package sets are designed to make that work, and `Cache` is an interface if
  you want a Redis- or disk-backed store.

## Test

```
go test ./...
```
