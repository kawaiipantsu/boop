package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// staticFiles holds the WebUI assets compiled into the binary (§4.3).
//
// The `all:` prefix is deliberate: a bundler emits dot-prefixed files such as
// `.vite/manifest.json`, and the default embed rules would silently skip them.
//
// `static/dist` is produced by `make web-build` and is absent from a clean
// checkout, so the pattern names the parent directory instead. `placeholder.html`
// is committed purely so this pattern always matches at least one file — an
// embed pattern that matches nothing is a compile error, and the server must
// build whether or not anyone has run npm.
//
//go:embed all:static
var staticFiles embed.FS

// bundleRoot is the directory the frontend build writes into.
const bundleRoot = "static/dist"

// bundleFS returns the built frontend assets and reports whether a usable
// bundle was embedded. "Usable" means an index.html exists: an empty dist
// directory left behind by a failed build must degrade to the placeholder
// rather than serving 404s that look like a broken server.
func bundleFS() (fs.FS, bool) {
	sub, err := fs.Sub(staticFiles, bundleRoot)
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

// newStaticHandler serves the embedded WebUI.
//
// Static assets are served without authentication even when a token is
// required. They are a public JavaScript bundle with no user data in them, and
// a browser cannot attach an Authorization header to the initial document
// load, so gating them would make token auth unusable from a browser without
// buying anything: every /api route, including the WebSocket, still requires
// the token, and the shell renders nothing until one is supplied.
//
// With a bundle present it behaves as a single-page-application host: known
// files are served directly and every other path falls back to index.html so
// client-side routes survive a reload. Without one it serves a page that
// explains how to build the frontend, because a blank 404 at the root of a
// server the user just started is an unhelpful way to report a missing asset.
func newStaticHandler(basePath string) http.Handler {
	assets, ok := bundleFS()
	if !ok {
		return placeholderHandler()
	}
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "only GET and HEAD are supported for static assets")
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			serveIndex(w, r, assets, basePath)
			return
		}
		if _, err := fs.Stat(assets, name); err != nil {
			serveIndex(w, r, assets, basePath)
			return
		}
		// Bundlers emit content-hashed filenames under assets/, so those are
		// safe to cache forever. Everything else is revalidated, since a stale
		// index.html pinned in a browser cache outlives several rebuilds.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex writes the SPA entry document with revalidation forced and injects <base href="..."> if base is set.
func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS, basePath string) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "the embedded WebUI bundle is unreadable")
		return
	}
	if basePath != "" {
		if !strings.HasSuffix(basePath, "/") {
			basePath += "/"
		}
		baseTag := fmt.Sprintf("<head><base href=\"%s\">", basePath)
		data = []byte(strings.Replace(string(data), "<head>", baseTag, 1))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	writeBody(w, r, data)
}

// placeholderHandler serves the "no bundle built" page at the root and a JSON
// 404 elsewhere, so a frontend fetching a missing asset gets a machine-readable
// answer rather than a wall of HTML.
func placeholderHandler() http.Handler {
	page, err := staticFiles.ReadFile("static/placeholder.html")
	if err != nil {
		// Unreachable while placeholder.html is committed; the fallback keeps
		// the server useful rather than panicking if it is ever removed.
		page = []byte("<!doctype html><title>Boop</title><p>WebUI bundle not built. Run <code>make web-build</code>.")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, codeNotFound, "no WebUI bundle is embedded in this binary; run `make web-build`")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		writeBody(w, r, page)
	})
}

// writeBody writes a body, honouring HEAD.
func writeBody(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
