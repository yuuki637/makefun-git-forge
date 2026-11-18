package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Hand-rolled HTTP/1.1 server over net.Listener. net/http costs several
// allocations and a full header-map canonicalization per request; on the
// trial box (2 CPU shared with the bench client) the read/write workloads
// are per-request-overhead bound, so this server parses requests out of a
// pooled byte buffer with byte-slice routing, keeps connections alive, and
// writes each response with a single Write in the common case.
//
// Protocol coverage (everything the kit's clients and the real git client
// send): Content-Length and chunked request bodies, Expect: 100-continue,
// keep-alive for HTTP/1.1 (and HTTP/1.0 with Connection: keep-alive),
// absolute-form targets, HEAD (headers only, like net/http).

const maxHeadBytes = 64 * 1024

// Pooled read buffers are kept at len == capacity; connReader tracks the
// live window with its s/e offsets.
var readBufPool = sync.Pool{New: func() interface{} { return make([]byte, 32*1024) }}
var writeBufPool = sync.Pool{New: func() interface{} { return make([]byte, 0, 8192) }}

func serveRaw(l net.Listener, a *app) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			return err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go serveConn(conn, a)
	}
}

// connReader is a byte window over the connection: buf[s:e] is unconsumed.
type connReader struct {
	conn net.Conn
	buf  []byte
	s, e int
}

func (r *connReader) fill() error {
	if r.s > 0 {
		copy(r.buf, r.buf[r.s:r.e])
		r.e -= r.s
		r.s = 0
	}
	if r.e == len(r.buf) {
		grown := make([]byte, len(r.buf)*2)
		copy(grown, r.buf[:r.e])
		r.buf = grown
	}
	n, err := r.conn.Read(r.buf[r.e:len(r.buf)])
	if n > 0 {
		r.e += n
	}
	return err
}

func (r *connReader) window() []byte { return r.buf[r.s:r.e] }

// readHead returns the request head (request line + headers, without the
// terminating CRLFCRLF) as a slice of the window; the caller copies what it
// keeps before the next fill.
func (r *connReader) readHead() ([]byte, error) {
	for {
		if idx := bytes.Index(r.window(), []byte("\r\n\r\n")); idx >= 0 {
			head := r.window()[:idx]
			r.s += idx + 4
			return head, nil
		}
		if len(r.window()) > maxHeadBytes {
			return nil, io.ErrUnexpectedEOF
		}
		if err := r.fill(); err != nil {
			return nil, err
		}
	}
}

// readBody reads exactly n body bytes. Small bodies already in the window
// are returned as a window slice (valid until the next fill, which only
// happens after dispatch); larger ones are read into a fresh buffer.
func (r *connReader) readBody(n int) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	if len(r.window()) >= n {
		body := r.window()[:n]
		r.s += n
		return body, nil
	}
	body := make([]byte, n)
	copied := copy(body, r.window())
	r.s = r.e
	for copied < n {
		m, err := r.conn.Read(body[copied:])
		if m > 0 {
			copied += m
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// readChunkedBody decodes a chunked request body.
func (r *connReader) readChunkedBody(limit int) ([]byte, bool, error) {
	var out []byte
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, false, err
		}
		// Chunk extensions are separated by ';'.
		sizeText := string(line)
		if idx := strings.IndexByte(sizeText, ';'); idx >= 0 {
			sizeText = sizeText[:idx]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 {
			return nil, false, io.ErrUnexpectedEOF
		}
		if size == 0 {
			// Trailers: lines until an empty one.
			for {
				trailer, err := r.readLine()
				if err != nil {
					return nil, false, err
				}
				if len(trailer) == 0 {
					return out, false, nil
				}
			}
		}
		if int64(len(out))+size > int64(limit) {
			return out, true, nil
		}
		chunk, err := r.readBody(int(size))
		if err != nil {
			return nil, false, err
		}
		out = append(out, chunk...)
		// Trailing CRLF after each chunk.
		crlf, err := r.readBody(2)
		if err != nil {
			return nil, false, err
		}
		if crlf[0] != '\r' || crlf[1] != '\n' {
			return nil, false, io.ErrUnexpectedEOF
		}
	}
}

// readLine reads one CRLF-terminated line (returned without the CRLF).
func (r *connReader) readLine() ([]byte, error) {
	for {
		if idx := bytes.Index(r.window(), []byte("\r\n")); idx >= 0 {
			line := r.window()[:idx]
			r.s += idx + 2
			return line, nil
		}
		if len(r.window()) > maxHeadBytes {
			return nil, io.ErrUnexpectedEOF
		}
		if err := r.fill(); err != nil {
			return nil, err
		}
	}
}

type parsedHead struct {
	method      string
	escapedPath string
	rawQuery    string
	version11   bool
	header      headerMap
	host        string
	contentLen  int64 // -1 when absent
	chunked     bool
	teUnknown   bool
	expect100   bool
	connClose   bool
	connKeepAlv bool
}

// equalFoldBytes reports whether b equals the lowercase ASCII word,
// case-insensitively, without allocating.
func equalFoldBytes(b []byte, word string) bool {
	if len(b) != len(word) {
		return false
	}
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != word[i] {
			return false
		}
	}
	return true
}

func containsFoldBytes(b []byte, word string) bool {
	if len(word) == 0 {
		return true
	}
	for i := 0; i+len(word) <= len(b); i++ {
		if equalFoldBytes(b[i:i+len(word)], word) {
			return true
		}
	}
	return false
}

func parseDecimalBytes(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

func trimValueBytes(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// parseHead parses the request head (a window slice; everything it keeps
// is copied into strings, so the slice is never retained). Only the two
// headers the forge actually reads (Authorization, Git-Protocol) are kept,
// plus the framing fields, all matched on raw bytes.
func parseHead(head []byte) (*parsedHead, bool) {
	p := &parsedHead{contentLen: -1}
	lines := bytes.Split(head, []byte("\r\n"))
	if len(lines) == 0 {
		return nil, false
	}
	requestLine := string(lines[0])
	parts := strings.SplitN(requestLine, " ", 3)
	if len(parts) != 3 {
		return nil, false
	}
	p.method = strings.ToUpper(parts[0])
	target := parts[1]
	p.version11 = parts[2] == "HTTP/1.1"
	if !p.version11 && parts[2] != "HTTP/1.0" {
		return nil, false
	}
	// Absolute-form targets (http://host/path?query): keep only path+query.
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			target = rest[idx:]
		} else {
			target = "/"
		}
	}
	if idx := strings.IndexByte(target, '?'); idx >= 0 {
		p.escapedPath = target[:idx]
		p.rawQuery = target[idx+1:]
	} else {
		p.escapedPath = target
	}
	if p.escapedPath == "" {
		p.escapedPath = "/"
	}
	p.header = headerMap{}
	for _, line := range lines[1:] {
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := line[:colon]
		value := trimValueBytes(line[colon+1:])
		switch {
		case equalFoldBytes(name, "host"):
			p.host = string(value)
		case equalFoldBytes(name, "content-length"):
			if n, ok := parseDecimalBytes(value); ok {
				p.contentLen = n
			}
		case equalFoldBytes(name, "transfer-encoding"):
			if containsFoldBytes(value, "chunked") {
				p.chunked = true
			} else {
				p.teUnknown = true
			}
		case equalFoldBytes(name, "expect"):
			if equalFoldBytes(value, "100-continue") {
				p.expect100 = true
			}
		case equalFoldBytes(name, "connection"):
			if containsFoldBytes(value, "close") {
				p.connClose = true
			}
			if containsFoldBytes(value, "keep-alive") {
				p.connKeepAlv = true
			}
		case equalFoldBytes(name, "authorization"):
			if _, exists := p.header["Authorization"]; !exists {
				p.header["Authorization"] = string(value)
			}
		case equalFoldBytes(name, "git-protocol"):
			if _, exists := p.header["Git-Protocol"]; !exists {
				p.header["Git-Protocol"] = string(value)
			}
		}
	}
	return p, true
}

var interimContinue = []byte("HTTP/1.1 100 Continue\r\n\r\n")

func writeAll(conn net.Conn, data []byte) bool {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return false
		}
		data = data[n:]
	}
	return true
}

func serveConn(conn net.Conn, a *app) {
	defer conn.Close()
	buf := readBufPool.Get().([]byte)
	defer readBufPool.Put(buf)
	r := &connReader{conn: conn, buf: buf}

	for {
		head, err := r.readHead()
		if err != nil {
			return
		}
		// parseHead copies everything it keeps into strings, so the
		// window slice is safe to parse in place.
		p, ok := parseHead(head)
		if !ok {
			return
		}

		if p.expect100 {
			if !writeAll(conn, interimContinue) {
				return
			}
		}

		info := &requestInfo{
			Method:      p.method,
			EscapedPath: p.escapedPath,
			RawQuery:    p.rawQuery,
			Header:      p.header,
			Host:        p.host,
		}
		closeAfter := p.teUnknown
		switch {
		case p.chunked:
			body, tooBig, err := r.readChunkedBody(maxBodyBytes + 1)
			if err != nil {
				return
			}
			info.Body = body
			info.BodyTooBig = tooBig
			if tooBig {
				closeAfter = true
			}
		case p.contentLen > 0:
			if p.contentLen > maxBodyBytes+1 {
				// Mirror the net/http path: it read up to the limit and
				// answered 500. Consume the capped prefix, then close —
				// the rest of the body would desync the connection.
				remaining := int64(maxBodyBytes + 1)
				for remaining > 0 {
					chunk := int64(len(r.window()))
					if chunk == 0 {
						if err := r.fill(); err != nil {
							return
						}
						continue
					}
					if chunk > remaining {
						chunk = remaining
					}
					r.s += int(chunk)
					remaining -= chunk
				}
				info.BodyTooBig = true
				closeAfter = true
			} else {
				body, err := r.readBody(int(p.contentLen))
				if err != nil {
					return
				}
				info.Body = body
			}
		}

		out, requestID := a.dispatch(info)

		keepAlive := !closeAfter && !p.connClose && (p.version11 || p.connKeepAlv)

		wbuf := writeBufPool.Get().([]byte)[:0]
		wbuf = append(wbuf, "HTTP/1.1 "...)
		wbuf = strconv.AppendInt(wbuf, int64(out.status), 10)
		wbuf = append(wbuf, ' ')
		wbuf = append(wbuf, http.StatusText(out.status)...)
		wbuf = append(wbuf, "\r\n"...)
		for k, v := range out.headers {
			wbuf = append(wbuf, k...)
			wbuf = append(wbuf, ": "...)
			wbuf = append(wbuf, v...)
			wbuf = append(wbuf, "\r\n"...)
		}
		wbuf = append(wbuf, "x-github-request-id: "...)
		wbuf = append(wbuf, requestID...)
		wbuf = append(wbuf, "\r\n"...)
		if out.status != 204 {
			wbuf = append(wbuf, "content-length: "...)
			wbuf = strconv.AppendInt(wbuf, int64(len(out.body)), 10)
			wbuf = append(wbuf, "\r\n"...)
		}
		if !keepAlive {
			wbuf = append(wbuf, "connection: close\r\n"...)
		}
		wbuf = append(wbuf, "\r\n"...)
		sendBody := out.status != 204 && p.method != "HEAD" && len(out.body) > 0
		if sendBody && len(out.body) <= 65536 {
			wbuf = append(wbuf, out.body...)
			if !writeAll(conn, wbuf) {
				writeBufPool.Put(wbuf[:0])
				return
			}
		} else {
			if !writeAll(conn, wbuf) {
				writeBufPool.Put(wbuf[:0])
				return
			}
			if sendBody && !writeAll(conn, out.body) {
				writeBufPool.Put(wbuf[:0])
				return
			}
		}
		writeBufPool.Put(wbuf[:0])

		if !keepAlive {
			return
		}
	}
}
