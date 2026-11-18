package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"runtime/pprof"
)

// HTTP server bootstrap + request dispatch (ported from app.ts), plus the
// standalone main(): listen port from FORGE_PORT, one `PORT <n>` line on
// stdout once listening. Serving normally goes through the hand-rolled
// HTTP/1.1 server in rawhttp.go; FORGE_RAW_HTTP=0 falls back to net/http
// with identical dispatch.

const maxBodyBytes = 96 * 1024 * 1024

type app struct {
	store         *Store
	expectedToken string
	counter       int64
	mu            sync.RWMutex

	// gen increments after every write-mode dispatch; cached GET replies
	// are tagged with the gen they were rendered under and are only served
	// while gen is unchanged (any mutation anywhere invalidates them).
	gen     int64
	cacheMu sync.RWMutex
	cache   map[string]*cacheEntry
}

type cacheEntry struct {
	gen     int64
	status  int
	headers map[string]string
	body    []byte
}

func newApp() *app {
	return &app{
		store:         newStore(),
		expectedToken: os.Getenv("MAKEFUN_SHOP_TOKEN"),
		cache:         map[string]*cacheEntry{},
	}
}

func decodeSegment(segment string) string {
	if !strings.Contains(segment, "%") {
		return segment
	}
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}

// lockMode decides the store lock for a request, mirroring Node's
// single-threaded semantics: reads share, mutations are exclusive.
func lockMode(method string, segments []string, path string, isGitPath bool) string {
	if path == "/healthz" {
		return "none"
	}
	if isGitPath {
		// advertisement / ls-refs / fetch are reads; receive-pack mutates.
		if len(segments) >= 3 && segments[2] == "git-receive-pack" {
			return "write"
		}
		return "read"
	}
	if path == "/mcp" || path == "/api/graphql" {
		return "write" // both surfaces can mutate; GraphQL queries are cheap
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if method == "GET" {
			return "read"
		}
		return "write"
	}
	return "read"
}

// requestInfo is one parsed HTTP request, from either transport.
type requestInfo struct {
	Method      string
	EscapedPath string
	RawQuery    string
	Header      headerMap
	Body        []byte
	BodyTooBig  bool
	Host        string
}

// rendered is a fully materialized response: status, final headers (sans
// x-github-request-id, which is per-request), and body bytes.
type rendered struct {
	status  int
	headers map[string]string
	body    []byte
}

// renderReply materializes a handler Reply exactly as the original
// net/http path did (content-type defaults, JSON marshaling).
func renderReply(reply Reply) rendered {
	headers := map[string]string{}
	for k, v := range reply.Headers {
		headers[k] = v
	}
	var body []byte
	if reply.HasJSON {
		if _, ok := headers["content-type"]; !ok {
			headers["content-type"] = "application/json; charset=utf-8"
		}
		if raw, ok := reply.JSON.(json.RawMessage); ok {
			body = []byte(raw)
		} else {
			body = marshalJSON(reply.JSON)
		}
	} else if reply.HasBody {
		body = reply.Body
		if _, ok := headers["content-type"]; !ok && len(body) > 0 {
			headers["content-type"] = "application/json; charset=utf-8"
		}
	}
	return rendered{status: reply.Status, headers: headers, body: body}
}

func (a *app) cacheLookup(key string) *cacheEntry {
	a.cacheMu.RLock()
	e := a.cache[key]
	a.cacheMu.RUnlock()
	if e == nil || e.gen != atomic.LoadInt64(&a.gen) {
		return nil
	}
	return e
}

func (a *app) cacheStore(key string, e *cacheEntry) {
	a.cacheMu.Lock()
	if len(a.cache) > 16384 {
		a.cache = map[string]*cacheEntry{}
	}
	a.cache[key] = e
	a.cacheMu.Unlock()
}

// dispatch routes one request and returns the rendered response plus the
// request id to stamp on it.
func (a *app) dispatch(info *requestInfo) (rendered, string) {
	n := atomic.AddInt64(&a.counter, 1)
	requestID := "req-" + strconv.FormatInt(n, 36)

	if info.BodyTooBig {
		return rendered{
			status:  500,
			headers: map[string]string{"content-type": "application/json; charset=utf-8"},
			body:    marshalJSON(map[string]interface{}{"message": "Internal error"}),
		}, requestID
	}
	rawBody := info.Body

	authed := a.expectedToken == "" || info.Header.Get("Authorization") == "Bearer "+a.expectedToken

	// Fast path: a cached GET /api/v3 response is a byte replay while no
	// write-mode request has completed since it was rendered. The key
	// space is the escaped path + raw query, self-consistent because
	// entries are stored under the same escaped key after full dispatch
	// verified the route — so the lookup can run before path decoding,
	// and a spelling that decodes differently simply never hits.
	preKey := ""
	if info.Method == "GET" && authed && strings.HasPrefix(info.EscapedPath, "/api/v3/") {
		preKey = info.EscapedPath + "\x00" + info.RawQuery
		if e := a.cacheLookup(preKey); e != nil {
			return rendered{status: e.status, headers: e.headers, body: e.body}, requestID
		}
	}

	var segments []string
	for _, s := range strings.Split(info.EscapedPath, "/") {
		if s != "" {
			segments = append(segments, decodeSegment(s))
		}
	}
	path := "/" + strings.Join(segments, "/")
	isGitPath := len(segments) >= 2 && strings.HasSuffix(segments[1], ".git")

	host := info.Host
	if host == "" {
		host = "127.0.0.1"
	}

	// Pre-rendered response cache: GETs on the REST surface are byte
	// replays while no write-mode request has completed since rendering.
	// Requests whose escaped spelling hid the /api/v3/ prefix (percent
	// escapes) fall back to the decoded-path key and its lookup here.
	cacheable := info.Method == "GET" && len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3"
	cacheKey := ""
	if cacheable && authed {
		if preKey != "" {
			cacheKey = preKey
		} else {
			cacheKey = path + "\x00" + info.RawQuery
			if e := a.cacheLookup(cacheKey); e != nil {
				return rendered{status: e.status, headers: e.headers, body: e.body}, requestID
			}
		}
	}

	var query url.Values
	if info.RawQuery != "" {
		query, _ = url.ParseQuery(info.RawQuery)
	}

	var jsonVal interface{}
	jsonError := false
	if len(rawBody) > 0 && !isGitPath {
		dec := json.NewDecoder(bytes.NewReader(rawBody))
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			jsonError = true
		} else {
			jsonVal = v
		}
	}

	ctx := &Ctx{
		Store:     a.store,
		Method:    info.Method,
		Path:      path,
		Segments:  segments,
		Query:     query,
		Headers:   info.Header,
		RawBody:   rawBody,
		JSON:      jsonVal,
		JSONError: jsonError,
		Host:      host,
		RequestID: requestID,
		Authed:    authed,
	}

	mode := lockMode(ctx.Method, segments, path, isGitPath)
	switch mode {
	case "write":
		a.mu.Lock()
		defer a.mu.Unlock()
	case "read":
		a.mu.RLock()
		defer a.mu.RUnlock()
	}

	needsAuth := path != "/healthz" && ((len(segments) > 0 && segments[0] == "api") || path == "/mcp" || isGitPath)

	var reply Reply
	switch {
	case path == "/healthz":
		reply = jsonReply(200, map[string]interface{}{"ok": true}, nil)
	case needsAuth && !authed:
		reply = errorReply(401, "Requires authentication")
	case len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3":
		reply = handleRest(ctx)
	case path == "/api/graphql":
		reply = handleGraphql(ctx)
	case path == "/mcp":
		reply = handleMcp(ctx)
	case isGitPath:
		reply = handleGit(ctx)
	default:
		reply = errorReply(404, "Not Found")
	}

	out := renderReply(reply)
	if mode == "write" {
		// The mutation (if any) is complete; invalidate cached reads.
		atomic.AddInt64(&a.gen, 1)
	}
	if cacheKey != "" && out.status == 200 && len(out.body) <= 1<<20 {
		a.cacheStore(cacheKey, &cacheEntry{
			gen:     atomic.LoadInt64(&a.gen),
			status:  out.status,
			headers: out.headers,
			body:    out.body,
		})
	}
	return out, requestID
}

// ------------------------------------------------------------ net/http path
func (a *app) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var rawBody []byte
	var readErr error
	if r.ContentLength != 0 {
		rawBody, readErr = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	}
	info := &requestInfo{
		Method:      strings.ToUpper(r.Method),
		EscapedPath: r.URL.EscapedPath(),
		RawQuery:    r.URL.RawQuery,
		Body:        rawBody,
		BodyTooBig:  readErr != nil || len(rawBody) > maxBodyBytes,
		Host:        r.Host,
	}
	hdr := headerMap{}
	for name, values := range r.Header {
		if len(values) > 0 {
			hdr[name] = values[0]
		}
	}
	info.Header = hdr

	out, requestID := a.dispatch(info)
	for k, v := range out.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("X-GitHub-Request-Id", requestID)
	if out.status != 204 {
		w.Header().Set("Content-Length", strconv.Itoa(len(out.body)))
	}
	w.WriteHeader(out.status)
	if out.status != 204 && len(out.body) > 0 {
		w.Write(out.body)
	}
}

func main() {
	// The trial box is small (2 CPU / 1 GB) and the workload is heavy on
	// short-lived request garbage: trade heap headroom for fewer GC cycles.
	debug.SetGCPercent(300)

	if profilePath := os.Getenv("FORGE_CPUPROFILE"); profilePath != "" {
		if f, err := os.Create(profilePath); err == nil {
			_ = pprof.StartCPUProfile(f)
			secs := 120
			if v := os.Getenv("FORGE_CPUPROFILE_SECS"); v != "" {
				if parsed, perr := strconv.Atoi(v); perr == nil && parsed > 0 {
					secs = parsed
				}
			}
			go func() {
				time.Sleep(time.Duration(secs) * time.Second)
				pprof.StopCPUProfile()
				_ = f.Close()
			}()
		}
	}

	port := os.Getenv("FORGE_PORT")
	if port == "" {
		port = "0"
	}
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen failed:", err)
		os.Exit(1)
	}
	addr := listener.Addr().(*net.TCPAddr)
	fmt.Printf("PORT %d\n", addr.Port)
	a := newApp()
	if os.Getenv("FORGE_RAW_HTTP") == "0" {
		server := &http.Server{Handler: http.HandlerFunc(a.serveHTTP)}
		if err := server.Serve(listener); err != nil {
			fmt.Fprintln(os.Stderr, "serve failed:", err)
			os.Exit(1)
		}
		return
	}
	if err := serveRaw(listener, a); err != nil {
		fmt.Fprintln(os.Stderr, "serve failed:", err)
		os.Exit(1)
	}
}
