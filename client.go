package assistantserver

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client configures and signs calls to the assistant-server. DebugLog (nil by
// default) receives transport failures; enabling it mirrors the PHP
// "assistant-server.log" debug switch.
type Client struct {
	cfg  ClientConfig
	http *http.Client
}

// ClientConfig is the pure-struct client configuration.
type ClientConfig struct {
	// Address is the assistant-server base, no path, no trailing slash.
	Address string
	AppID   string
	Secret  string
	// Timeout defaults to 20s when HTTP is nil.
	Timeout time.Duration
	// HTTP overrides the transport (tests; insecure TLS builds).
	HTTP *http.Client
	// DebugLog option: receives transport-failure/debug lines; nil = silent.
	DebugLog func(format string, args ...any)
}

// NewClient wires the configuration into a usable client. A nil HTTP means a
// stdlib transport with the configured timeout.
func NewClient(cfg ClientConfig) *Client {
	c := &Client{cfg: cfg}
	if cfg.HTTP != nil {
		c.http = cfg.HTTP
	} else {
		c.http = &http.Client{Timeout: cfg.Timeout}
	}
	return c
}

// Request is the generic signed entry point for any endpoint: POST/PUT/PATCH
// treat params as the JSON body, every other method folds params into the
// query string (which participates in the signature).
func (c *Client) Request(method, path string, params map[string]any) *Result {
	method = strings.ToUpper(method)
	if missing := c.missingConfig(); missing != "" {
		return &Result{Code: "CONFIG_ERROR", Message: "ClientConfig." + missing + " is not configured"}
	}

	var body []byte
	var contentType string
	query := ""
	switch method {
	case "POST", "PUT", "PATCH":
		encoded, err := marshalBody(params)
		if err != nil {
			return &Result{Code: "VALIDATION_ERROR", Message: "params are not JSON-encodable: " + err.Error()}
		}
		body = encoded
		contentType = "application/json"
	default:
		vals := url.Values{}
		for key, value := range params {
			vals.Set(key, fmt.Sprintf("%v", value))
		}
		query = vals.Encode() // sorted; the signature signs exactly what we send
	}
	return c.send(method, path, query, body, contentType)
}

// RequestMultipart posts a multipart/form-data call whose raw wire bytes are
// prebuilt, so X-Content-SHA256 signs the exact bytes on the wire.
func (c *Client) RequestMultipart(method, path, fileField, filename string, fileContent []byte, extraForm map[string]string) *Result {
	if missing := c.missingConfig(); missing != "" {
		return &Result{Code: "CONFIG_ERROR", Message: "ClientConfig." + missing + " is not configured"}
	}

	boundary := randomHex(16) // PHP parity: bin2hex(random_bytes(16))
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.SetBoundary(boundary)
	for key, value := range extraForm {
		_ = w.WriteField(key, value)
	}
	fileWriter, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		return &Result{Code: "VALIDATION_ERROR", Message: "multipart setup failed: " + err.Error()}
	}
	if _, err = fileWriter.Write(fileContent); err != nil {
		return &Result{Code: "INTERNAL_ERROR", Message: err.Error()}
	}
	if err = w.Close(); err != nil {
		return &Result{Code: "INTERNAL_ERROR", Message: err.Error()}
	}
	return c.send(strings.ToUpper(method), path, "", buf.Bytes(),
		"multipart/form-data; boundary="+boundary)
}

// Sync posts a single audit event ("single sync").
func (c *Client) Sync(event map[string]any) *Result {
	return c.Request("POST", "/api/v1/audit/logs", event)
}

// SyncBatch posts the {"events":[...]} envelope (1..500 events per server
// contract; duplicate event_ids rejected server-side).
func (c *Client) SyncBatch(events []map[string]any) *Result {
	return c.Request("POST", "/api/v1/audit/logs/batch", map[string]any{"events": events})
}

// GETs one event by id; filters ride the signed query string.
func (c *Client) Get(id string, filters map[string]any) *Result {
	return c.Request("GET", "/api/v1/audit/logs/"+url.PathEscape(id), filters)
}

// GETs events matching filters ("query/retrieval").
func (c *Client) Query(filters map[string]any) *Result {
	return c.Request("GET", "/api/v1/audit/logs", filters)
}

// marshalBody JSON-encodes with HTML escaping off and UTF-8 kept raw, byte
// matching PHP json_encode(..., JSON_UNESCAPED_UNICODE) (contains minus the
// trailing newline the stdlib encoder appends).
func marshalBody(params map[string]any) ([]byte, error) {
	if params == nil {
		params = map[string]any{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(params); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// missingConfig returns the first blank secret/config name, "" when ready.
func (c *Client) missingConfig() string {
	switch {
	case c.cfg.Address == "":
		return "Address"
	case c.cfg.AppID == "":
		return "AppID"
	case c.cfg.Secret == "":
		return "Secret"
	}
	return ""
}

// send performs one signed round trip. HTTP 4xx/5xx does NOT surface as a Go
// error — the response body decides Result semantics like PHP.
func (c *Client) send(method, path, query string, body []byte, contentType string) *Result {
	nonce := randomHex(16) // 32 hex chars, inside [A-Za-z0-9_-]{8,64}
	headers := SignHeaders(c.cfg.AppID, c.cfg.Secret, method, path, string(body), query,
		fmt.Sprintf("%d", time.Now().Unix()), nonce)

	target := c.cfg.Address + path
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return c.internalError(method, path, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.internalError(method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.internalError(method, path, err)
	}
	return parseResult(resp.StatusCode, raw)
}

func (c *Client) internalError(method, path string, err error) *Result {
	if c.cfg.DebugLog != nil {
		c.cfg.DebugLog("assistant-server request failed (%s %s): %v", method, path, err)
	}
	return &Result{Code: "INTERNAL_ERROR", Message: err.Error()}
}

// randomHex returns 2n lowercase hex chars from crypto/rand.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
