// Package webui serves the compiled Vue SPA from an embedded filesystem.
//
// The build output lives in web/dist and is copied to internal/webui/dist by the
// Dockerfile (and by `make web` locally). Only a .gitkeep is committed there, so
// this package must work with an empty or partially populated tree: a missing
// index.html is a normal state in a backend-only checkout and must produce a
// helpful page, never a panic at init time.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS is the compiled frontend. `all:` is required so that a build with only
// hidden files (e.g. .gitkeep) does not fail the embed directive.
//
//go:embed all:dist
var distFS embed.FS

// apiPrefixes are paths that must never receive the SPA fallback. They are
// listed explicitly rather than derived from a catch-all so that adding a new
// API prefix in Phase B is a deliberate act.
//
// `/api` is checked with a trailing slash semantics (see shouldFallback).
var apiPrefixes = []string{
	"/api",
	"/ws",
	"/metrics",
	"/ops",
	"/healthz",
	"/readyz",
}

// Options configures Handler.
type Options struct {
	// FS overrides the embedded assets; nil means the embedded dist directory.
	// Tests use this instead of writing files into the source tree.
	FS fs.FS
	// DevHint is shown on the placeholder page. It should say how to build the
	// frontend, e.g. "cd web && npm ci && npm run build".
	DevHint string
}

// Handler returns an http.Handler serving the SPA.
//
// Routing rules:
//
//   - an existing file is served as-is, with immutable caching for hashed asset
//     names under /assets/;
//   - index.html is never cached, because it references the hashed bundles;
//   - any other GET/HEAD path outside the API prefixes gets index.html so
//     client-side routes such as /r/K7M2QX survive a page reload;
//   - API prefixes and non-GET methods get 404 JSON, never HTML.
func Handler(opts Options) http.Handler {
	assets := opts.FS
	if assets == nil {
		sub, err := fs.Sub(distFS, "dist")
		if err != nil {
			// fs.Sub only fails for a malformed path, which is a programming
			// error; fall back to an empty FS so the binary still serves APIs.
			assets = fstestEmpty{}
		} else {
			assets = sub
		}
	}
	index, indexErr := fs.ReadFile(assets, "index.html")
	h := &handler{
		assets:  assets,
		index:   index,
		indexOK: indexErr == nil,
		devHint: opts.DevHint,
		fileSrv: http.FileServer(http.FS(assets)),
	}
	return h
}

type handler struct {
	assets  fs.FS
	index   []byte
	indexOK bool
	devHint string
	fileSrv http.Handler
}

// ServeHTTP implements http.Handler.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainJSON(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and HEAD are supported")
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	if shouldFallback(clean) {
		writePlainJSON(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}

	if name, ok := h.lookup(clean); ok {
		h.serveFile(w, r, name)
		return
	}

	// SPA fallback. A request for a real-looking asset (has an extension) that
	// does not exist is a missing build artifact, not a client-side route;
	// answering with index.html would make the browser parse HTML as JS.
	if path.Ext(clean) != "" {
		writePlainJSON(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	if !h.indexOK {
		h.servePlaceholder(w, r)
		return
	}
	h.serveIndex(w, r)
}

// lookup maps a URL path onto a file in the embedded FS, guarding against
// directory traversal (`path.Clean` plus rejecting ".." segments).
func (h *handler) lookup(clean string) (string, bool) {
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "..") {
		return "", false
	}
	st, err := fs.Stat(h.assets, name)
	if err != nil || st.IsDir() {
		// A directory request falls through to the SPA index; embedded builds
		// do not ship directory listings.
		return "", false
	}
	return name, true
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if name == "index.html" {
		// Serve the bytes directly instead of through http.FileServer, which
		// redirects /index.html to "/" — that redirect would turn a plain
		// request for the entry document into an extra round trip.
		h.serveIndex(w, r)
		return
	}
	cache := "no-cache"
	if strings.HasPrefix(name, "assets/") {
		// Vite emits content-hashed filenames, so a long immutable cache is safe
		// and is what makes repeat loads fast.
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cache)
	// Reuse the standard file server for range requests, content type sniffing
	// and If-Modified-Since handling.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + name
	h.fileSrv.ServeHTTP(w, r2)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(h.index)
}

// servePlaceholder is what a backend-only checkout sees at "/". It explains the
// situation instead of showing a blank 404, because "frontend not built yet" is
// an expected state during parallel development.
func (h *handler) servePlaceholder(w http.ResponseWriter, r *http.Request) {
	hint := h.devHint
	if hint == "" {
		hint = "cd web && npm ci && npm run build"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>同绘 CoDraw · 前端未构建</title>
<style>
 body{margin:0;min-height:100vh;display:grid;place-items:center;background:#F7F6F2;color:#16181D;
      font:16px/1.6 ui-sans-serif,"PingFang SC","Microsoft YaHei",system-ui,sans-serif}
 main{max-width:560px;padding:32px;background:#fff;border:1px solid #E4E2DA;border-radius:16px}
 h1{font-size:20px;margin:0 0 8px}
 p{color:#5B6070;margin:0 0 16px}
 code{display:block;background:#F7F6F2;border:1px solid #E4E2DA;border-radius:8px;padding:10px 12px;
      font:13px/1.5 ui-monospace,"SF Mono",monospace;overflow-x:auto}
 a{color:#3D5AFE}
</style></head><body><main>
<h1>前端产物未找到</h1>
<p>后端已经在运行，但 <code style="display:inline;padding:1px 4px">internal/webui/dist</code> 里没有 <code style="display:inline;padding:1px 4px">index.html</code>。</p>
<p>构建前端后刷新：</p>
<code>`+hint+`</code>
<p style="margin-top:16px">接口仍然可用：<a href="/healthz">/healthz</a> · <a href="/readyz">/readyz</a> · <a href="/metrics">/metrics</a> · <a href="/ops">/ops</a></p>
</main></body></html>`)
}

// shouldFallback reports whether path belongs to the API surface, in which case
// the SPA must not answer.
func shouldFallback(clean string) bool {
	for _, p := range apiPrefixes {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}

func writePlainJSON(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"`+msg+`"}}`)
}

// fstestEmpty is an fs.FS with no files, used only if fs.Sub fails.
type fstestEmpty struct{}

func (fstestEmpty) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
