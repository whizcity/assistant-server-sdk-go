package assistantserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, body string, status int) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := newCapturedRequest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestRequestPostsJSONBodyAndSignatureCoversItExactly(t *testing.T) {
	srv, cap := newTestServer(t, `{"accepted":1}`, 200)
	c := NewClient(ClientConfig{Address: srv.URL, AppID: testApp, Secret: postSecret})

	r := c.Request("POST", "/api/v1/audit/logs", map[string]any{"service": "demo", "note": "中文"})

	if !r.Success() {
		t.Fatalf("expected success, got %+v", r)
	}
	if cap.method != "POST" || cap.path != "/api/v1/audit/logs" || cap.query != "" {
		t.Errorf("wrong wire request: %s %s ?%s", cap.method, cap.path, cap.query)
	}
	if cap.contentType != "application/json" {
		t.Errorf("wrong content type: %q", cap.contentType)
	}
	if !strings.Contains(cap.body, "中文") {
		t.Errorf("body must keep unicode unescaped: %q", cap.body)
	}
	// The signed thing must be the sent thing: recompute from OBSERVED canonical parts.
	sig := HMACSHA256Hex(postSecret, cap.canonical(testApp))
	if sig != cap.header(HeaderSignature) {
		t.Errorf("signature mismatch:\n got  %s\n want %s", cap.header(HeaderSignature), sig)
	}
	if cap.header(HeaderAppID) != testApp {
		t.Errorf("app id header wrong: %q", cap.header(HeaderAppID))
	}
}

func TestRequestGetFoldsParamsIntoSignedQuery(t *testing.T) {
	srv, cap := newTestServer(t, `{"items":[]}`, 200)
	c := NewClient(ClientConfig{Address: srv.URL, AppID: testApp, Secret: testSecret})

	r := c.Request("GET", "/api/v1/audit/logs", map[string]any{"action": "update", "resource": "orders", "size": 20})

	if !r.Success() {
		t.Fatalf("expected success, got %+v", r)
	}
	want := "action=update&resource=orders&size=20"
	if cap.query != want {
		t.Errorf("query mismatch:\n got  %s\n want %s", cap.query, want)
	}
	if cap.body != "" {
		t.Errorf("GET body must be empty, got %q", cap.body)
	}
	if sig := HMACSHA256Hex(testSecret, cap.canonical(testApp)); sig != cap.header(HeaderSignature) {
		t.Errorf("signature must cover the query")
	}
}

func TestSyncBatchSplitsAt500AndRejectsEmpty(t *testing.T) {
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		sizes = append(sizes, len(body.Events))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":1,"failed":0}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{Address: srv.URL, AppID: testApp, Secret: postSecret})
	events := make([]map[string]any, 501)
	for i := range events {
		events[i] = map[string]any{"event_id": NewULID()}
	}

	if r := c.SyncBatch(events); !r.Success() {
		t.Fatalf("batch must succeed, got %+v", r)
	}
	if len(sizes) != 2 || sizes[0] != 500 || sizes[1] != 1 {
		t.Errorf("want batches [500 1], got %v", sizes)
	}
	if r := c.SyncBatch(nil); r.Success() || r.Code != "VALIDATION_ERROR" {
		t.Errorf("empty batch must be rejected locally, got %+v", r)
	}
}

func TestMissingConfigYieldsConfigErrorWithoutSending(t *testing.T) {
	c := NewClient(ClientConfig{Address: "", AppID: "", Secret: ""})
	r := c.Request("POST", "/api/v1/audit/logs", map[string]any{"service": "demo"})
	if r.Success() || r.Code != "CONFIG_ERROR" {
		t.Errorf("expected CONFIG_ERROR, got %+v", r)
	}
	if !strings.Contains(r.Message, "Address") {
		t.Errorf("message should name the missing keys, got %q", r.Message)
	}
}

func TestTransportFailureIsReportedAsInternalError(t *testing.T) {
	srv, _ := newTestServer(t, `{}`, 200)
	dead := srv.URL
	srv.Close()
	c := NewClient(ClientConfig{Address: dead, AppID: testApp, Secret: postSecret})
	r := c.Request("POST", "/api/v1/audit/logs", map[string]any{"service": "demo"})
	if r.Success() || r.Code != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %+v", r)
	}
	if r.Message == "" {
		t.Errorf("message should carry transport error text")
	}
}

func TestSugarSyncBatchSendsEnvelopeAndGetSubstitutesId(t *testing.T) {
	srv, cap := newTestServer(t, `{"accepted":2,"failed":0}`, 202)
	c := NewClient(ClientConfig{Address: srv.URL, AppID: testApp, Secret: postSecret})

	r := c.SyncBatch([]map[string]any{{
		"event_id": NewULID(), "service": "demo", "action": "update", "resource": "orders", "resource_id": "5",
	}})
	if !r.Success() {
		t.Fatalf("batch must succeed, got %+v", r)
	}
	if cap.path != "/api/v1/audit/logs/batch" {
		t.Errorf("wrong batch path: %s", cap.path)
	}
	if !strings.Contains(cap.body, `"events":[{`) {
		t.Errorf("body must use the events envelope, got %q", cap.body)
	}

	r2 := c.Get("5", map[string]any{})
	if !r2.Success() {
		t.Fatalf("get must succeed")
	}
	if cap.path != "/api/v1/audit/logs/5" || cap.verb != "GET" {
		t.Errorf("wrong get request: %s %s", cap.verb, cap.path)
	}
}
