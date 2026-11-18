package main

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type httpHeader = http.Header

// Shared request/response plumbing (ported from web.ts).

type Ctx struct {
	Store     *Store
	Method    string
	Path      string
	Segments  []string
	Query     url.Values
	Headers   httpHeader
	RawBody   []byte
	JSON      interface{}
	JSONError bool
	Host      string
	RequestID string
	Authed    bool
}

type Reply struct {
	Status  int
	Headers map[string]string
	Body    []byte
	HasBody bool
	JSON    interface{}
	HasJSON bool
}

func jsonReply(status int, value interface{}, headers map[string]string) Reply {
	if headers == nil {
		headers = map[string]string{}
	}
	return Reply{Status: status, Headers: headers, JSON: value, HasJSON: true}
}

func errorReply(status int, message string) Reply {
	return Reply{
		Status:  status,
		Headers: map[string]string{},
		JSON: map[string]interface{}{
			"message":           message,
			"documentation_url": "https://docs.github.com/rest",
		},
		HasJSON: true,
	}
}

func bodyReply(status int, contentType string, body []byte) Reply {
	return Reply{Status: status, Headers: map[string]string{"content-type": contentType}, Body: body, HasBody: true}
}

func (c *Ctx) header(name string) string {
	return c.Headers.Get(name)
}

// bodyObj reads the parsed JSON body as an object (empty when not an object).
func (c *Ctx) bodyObj() map[string]interface{} {
	if m, ok := c.JSON.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

// strField mirrors web.ts str(): the value when it is a string.
func strField(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func queryNum(raw string, present bool) float64 {
	if !present {
		return 0 // Number(null) === 0
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func (c *Ctx) pageParams() (perPage int, page int) {
	perRaw := queryNum(c.Query.Get("per_page"), hasQuery(c.Query, "per_page"))
	pageRaw := queryNum(c.Query.Get("page"), hasQuery(c.Query, "page"))
	perPage = 30
	if !math.IsNaN(perRaw) && !math.IsInf(perRaw, 0) && perRaw > 0 {
		perPage = int(math.Floor(perRaw))
		if perPage > 100 {
			perPage = 100
		}
	}
	page = 1
	if !math.IsNaN(pageRaw) && !math.IsInf(pageRaw, 0) && pageRaw > 0 {
		page = int(math.Floor(pageRaw))
	}
	return perPage, page
}

func hasQuery(q url.Values, key string) bool {
	_, ok := q[key]
	return ok
}

// paginate slices items for the page and builds a Link header value.
func paginate[T any](c *Ctx, basePath string, items []T) ([]T, string) {
	perPage, page := c.pageParams()
	start := (page - 1) * perPage
	var slice []T
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = []T{}
	}
	origin := "http://" + c.Host
	var parts []string
	hasNext := start+perPage < len(items)
	lastPage := int(math.Ceil(float64(len(items)) / float64(perPage)))
	if lastPage < 1 {
		lastPage = 1
	}
	if hasNext {
		parts = append(parts, "<"+origin+basePath+"?per_page="+strconv.Itoa(perPage)+"&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
		parts = append(parts, "<"+origin+basePath+"?per_page="+strconv.Itoa(perPage)+"&page="+strconv.Itoa(lastPage)+">; rel=\"last\"")
	}
	if page > 1 {
		parts = append(parts, "<"+origin+basePath+"?per_page="+strconv.Itoa(perPage)+"&page=1>; rel=\"first\"")
		parts = append(parts, "<"+origin+basePath+"?per_page="+strconv.Itoa(perPage)+"&page="+strconv.Itoa(page-1)+">; rel=\"prev\"")
	}
	return slice, strings.Join(parts, ", ")
}
