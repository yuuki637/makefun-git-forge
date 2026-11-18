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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// HTTP server bootstrap + request dispatch (ported from app.ts), plus the
// standalone main(): listen port from FORGE_PORT, one `PORT <n>` line on
// stdout once listening.

const maxBodyBytes = 96 * 1024 * 1024

type app struct {
	store         *Store
	expectedToken string
	counter       int64
	mu            sync.RWMutex
}

func newApp() *app {
	return &app{store: newStore(), expectedToken: os.Getenv("MAKEFUN_SHOP_TOKEN")}
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

func (a *app) handle(r *http.Request) Reply {
	n := atomic.AddInt64(&a.counter, 1)
	requestID := "req-" + strconv.FormatInt(n, 36)

	var rawBody []byte
	var err error
	if r.ContentLength != 0 {
		rawBody, err = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	}
	if err != nil || len(rawBody) > maxBodyBytes {
		return Reply{
			Status:  500,
			Headers: map[string]string{"x-github-request-id": requestID},
			JSON:    map[string]interface{}{"message": "Internal error"},
			HasJSON: true,
		}
	}

	escaped := r.URL.EscapedPath()
	var segments []string
	for _, s := range strings.Split(escaped, "/") {
		if s != "" {
			segments = append(segments, decodeSegment(s))
		}
	}
	path := "/" + strings.Join(segments, "/")
	isGitPath := len(segments) >= 2 && strings.HasSuffix(segments[1], ".git")

	host := r.Host
	if host == "" {
		host = "127.0.0.1"
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

	authed := a.expectedToken == "" || r.Header.Get("Authorization") == "Bearer "+a.expectedToken

	ctx := &Ctx{
		Store:     a.store,
		Method:    strings.ToUpper(r.Method),
		Path:      path,
		Segments:  segments,
		Query:     r.URL.Query(),
		Headers:   r.Header,
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
	if reply.Headers == nil {
		reply.Headers = map[string]string{}
	}
	reply.Headers["x-github-request-id"] = requestID
	return reply
}

func (a *app) serveHTTP(w http.ResponseWriter, r *http.Request) {
	reply := a.handle(r)

	headers := map[string]string{}
	for k, v := range reply.Headers {
		headers[k] = v
	}
	var body []byte
	if reply.HasJSON {
		if _, ok := headers["content-type"]; !ok {
			headers["content-type"] = "application/json; charset=utf-8"
		}
		body = marshalJSON(reply.JSON)
	} else if reply.HasBody {
		body = reply.Body
		if _, ok := headers["content-type"]; !ok && len(body) > 0 {
			headers["content-type"] = "application/json; charset=utf-8"
		}
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	if reply.Status != 204 {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(reply.Status)
	if reply.Status != 204 && len(body) > 0 {
		w.Write(body)
	}
}

func main() {
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
	server := &http.Server{Handler: http.HandlerFunc(a.serveHTTP)}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "serve failed:", err)
		os.Exit(1)
	}
}
