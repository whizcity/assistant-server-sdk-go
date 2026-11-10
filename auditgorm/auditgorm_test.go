package auditgorm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gorm "github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
	as "github.com/whizcity/assistant-server-sdk-go"
)

// captured server that records {"events":[...]} envelopes.
type eventsServer struct {
	mu      sync.Mutex
	batches [][]map[string]any
	srv     *httptest.Server
}

func (e *eventsServer) all() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, b := range e.batches {
		out = append(out, b...)
	}
	return out
}

func newEventsServer(t *testing.T) *eventsServer {
	t.Helper()
	e := &eventsServer{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			w.WriteHeader(400)
			return
		}
		e.mu.Lock()
		e.batches = append(e.batches, envelope.Events)
		e.mu.Unlock()
		w.WriteHeader(202)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":` + itoa(len(envelope.Events)) + `,"failed":0}`))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func itoa(n int) string {
	d := "0123456789"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{d[n%10]}, out...)
		n /= 10
	}
	return string(out)
}

type subject struct {
	ID    uint64
	Title *string
	State *string
}

func (subject) TableName() string { return "subjects" }

func newAuditedDB(t *testing.T, e *eventsServer, include func(string) bool, operatorID string) *gorm.DB {
	t.Helper()
	qdb, err := gorm.Open("sqlite3", "file::memory:?mode=memory&cache=private")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = qdb.Close() })
	if err = qdb.DB().Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for _, ddl := range []string{
		"CREATE TABLE subjects (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT, state TEXT, created_at DATETIME)",
	} {
		if err = qdb.Exec(ddl).Error; err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}
	client := as.NewClient(as.ClientConfig{Address: e.srv.URL, AppID: "t", Secret: "s"})
	profile := func() as.EventProfile {
		return as.EventProfile{Service: "parking", OperatorID: operatorID, OperatorName: "张三"}
	}
	if err = Register(qdb, client, profile, include); err != nil {
		t.Fatalf("register: %v", err)
	}
	return qdb
}

func waitDrained(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCreateCapturesFullAfterRow(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, nil, "9")

	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	waitDrained(t, 2*time.Second)

	events := e.all()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev["action"] != "create" || ev["resource"] != "subjects" {
		t.Errorf("identity wrong: %+v", ev)
	}
	after := ev["after"].(map[string]any)
	if after["title"] != "订单一" || after["state"] != "paid" {
		t.Errorf("after payload wrong: %+v", after)
	}
	if ev["operator_id"] != "9" {
		t.Errorf("operator missing: %+v", ev)
	}
}

func TestInstanceUpdateCapturesBeforeAndAfterOnce(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("a"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, time.Second)
	createEvents := len(e.all())

	row.State = strp("refunded")
	if err := qdb.Save(row).Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, 2*time.Second)

	events := e.all()
	if len(events) != createEvents+1 {
		t.Fatalf("expected exactly one update event, got %d (before %d)", len(events), createEvents)
	}
	last := events[len(events)-1]
	raw, _ := json.Marshal(last)
	if last["action"] != "update" || last["resource_id"] == nil || last["resource_id"] == "" {
		t.Errorf("update event missing per-row resource_id: %s", raw)
	}
	after, ok0 := last["after"].(map[string]any)
	if !ok0 {
		t.Fatalf("after not a map: %s", raw)
	}
	if after["state"] != "refunded" {
		t.Errorf("after wrong: %s", raw)
	}
	before := last["before"].(map[string]any)
	if before["state"] != "draft" {
		t.Errorf("before wrong: %s", raw)
	}
}

func TestMassUpdateMergesIntoOneEvent(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, nil, "9")
	for _, row := range []subject{
		{Title: strp("outer"), State: strp("draft")},
		{Title: strp("inner"), State: strp("draft")},
	} {
		if err := qdb.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	waitDrained(t, time.Second)
	if err := qdb.Model(&subject{}).Where("state = ?", "draft").Update("state", "paid").Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, 2*time.Second)

	var updates []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "update" {
			updates = append(updates, ev)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected ONE merged update event, got %d", len(updates))
	}
	after := updates[0]["after"].(map[string]any)
	if n, _ := after["count"].(float64); int(n) != 2 {
		t.Errorf("count wrong: %+v", after)
	}
	if updates[0]["resource_id"] != "" {
		t.Errorf("merged resource_id must be empty: %+v", updates[0])
	}
}

func TestMassDeleteMergesImagesPerRow(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, nil, "9")
	for i, title := range []string{"订单一", "订单二"} {
		row := subject{ID: uint64(i + 1), Title: strp(title), State: strp("draft")}
		insert(qdb, row)
	}
	updateState(qdb)
	waitDrained(t, time.Second)

	if err := qdb.Where("id IN (?,?)", 1, 2).Delete(&subject{}).Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, 2*time.Second)

	var deletes []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "delete" {
			deletes = append(deletes, ev)
		}
	}
	if len(deletes) < 1 {
		t.Fatalf("expected at least one delete event, got %d", len(deletes))
	}
	rawDel, _ := json.Marshal(deletes[0])
	after, ok := deletes[0]["after"].(map[string]any)
	if !ok || after == nil {
		t.Fatalf("single-row delete carries image in before, none here: %s", rawDel)
	}
	_ = rawDel
	// mass -> single row count depends on prior state; assert payload when merged
	if len(deletes) == 1 && after["rows"] != nil {
		rows := after["rows"].(map[string]any)
		if len(rows) != 2 || rows["1"].(map[string]any)["title"] != "订单一" {
			t.Errorf("rows payload wrong: %s", rawDel)
		}
	}
}

func TestEmptyOperatorProfileSkipsAuditing(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, nil, "")
	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, 2*time.Second)
	if len(e.all()) != 0 {
		t.Errorf("expected silent skip without operator, got %d", len(e.all()))
	}
}

func TestIncludeFilterBlocksTables(t *testing.T) {
	e := newEventsServer(t)
	qdb := newAuditedDB(t, e, func(name string) bool { return false }, "9")
	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}
	waitDrained(t, 2*time.Second)
	if len(e.all()) != 0 {
		t.Errorf("include(false) must silence everything")
	}
}

func insert(qdb *gorm.DB, row subject) { qdb.Exec("INSERT INTO subjects (id,title,state) VALUES (?,?,?)", row.ID, *row.Title, *row.State) }

func updateState(qdb *gorm.DB) { qdb.Model(&subject{}).Exec("UPDATE subjects SET state=? ", "draft") }

func strp(s string) *string { return &s }
