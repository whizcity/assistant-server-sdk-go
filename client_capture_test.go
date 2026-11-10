package assistantserver

import (
	"io"
	"net/http"
	"strings"
)

// capturedRequest keeps exactly what hit the wire so tests can recompute the
// signature from observed values only (“signed thing != sent thing” guard).
type capturedRequest struct {
	verb        string
	method      string
	path        string
	query       string
	body        string
	contentType string
	headers     map[string]string
}

func newCapturedRequest() *capturedRequest {
	return &capturedRequest{headers: map[string]string{}}
}

func (c *capturedRequest) record(r *http.Request) {
	c.verb = r.Method
	c.method = r.Method
	c.path = r.URL.RequestURI()
	if i := strings.IndexByte(c.path, '?'); i >= 0 {
		c.path, c.query = c.path[:i], c.path[i+1:]
	}
	raw, _ := io.ReadAll(r.Body)
	c.body = string(raw)
	c.contentType = r.Header.Get("Content-Type")
	for k, vs := range r.Header {
		c.headers[strings.ToLower(k)] = vs[0]
	}
}

func (c *capturedRequest) header(name string) string {
	return c.headers[strings.ToLower(name)]
}

// canonical recomputes the server-side canonical string from observed values.
func (c *capturedRequest) canonical(appID string) string {
	digest := SHA256Hex(c.body)
	return strings.Join([]string{
		strings.ToUpper(c.verb),
		c.path,
		c.query,
		c.header(HeaderTimestamp),
		c.header(HeaderNonce),
		digest,
	}, "\n")
}
