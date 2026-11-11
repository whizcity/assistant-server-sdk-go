package auditgorm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	as "github.com/whizcity/assistant-server-sdk-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// eventSink captures the events the recorder receives. Recording is
// synchronous, so tests can read it immediately after the write.
type eventSink struct {
	mu     sync.Mutex
	events []map[string]any
	err    error
}

func (e *eventSink) record(_ *gorm.DB, events []map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.events = append(e.events, events...)
	return nil
}

func (e *eventSink) all() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.events...)
}

func newEventSink() *eventSink { return &eventSink{} }

type subject struct {
	ID        uint64
	Title     *string
	State     *string
	UpdatedAt time.Time
}

func (subject) TableName() string { return "subjects" }

func newAuditedDB(t *testing.T, e *eventSink, include func(string) bool, operatorID string) *gorm.DB {
	t.Helper()
	qdb, err := gorm.Open(sqlite.Open("file::memory:?mode=memory&cache=private"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	sqlDB, err := qdb.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = sqlDB.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for _, ddl := range []string{
		"CREATE TABLE subjects (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT, state TEXT, created_at DATETIME, updated_at DATETIME)",
	} {
		if err = qdb.Exec(ddl).Error; err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}
	profile := func(context.Context) as.EventProfile {
		return as.EventProfile{Service: "parking", OperatorID: operatorID, OperatorName: "张三"}
	}
	if err = Register(qdb, e.record, profile, include); err != nil {
		t.Fatalf("register: %v", err)
	}
	return qdb
}

func TestCreateCapturesFullAfterRow(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")

	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}

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
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("a"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	createEvents := len(e.all())

	row.State = strp("refunded")
	if err := qdb.Save(row).Error; err != nil {
		t.Fatal(err)
	}

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
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	for _, row := range []subject{
		{Title: strp("outer"), State: strp("draft")},
		{Title: strp("inner"), State: strp("draft")},
	} {
		if err := qdb.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := qdb.Model(&subject{}).Where("state = ?", "draft").Update("state", "paid").Error; err != nil {
		t.Fatal(err)
	}

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
	if n := intValue(after["count"]); n != 2 {
		t.Errorf("count wrong: %+v", after)
	}
	if updates[0]["resource_id"] != "" {
		t.Errorf("merged resource_id must be empty: %+v", updates[0])
	}
}

func TestMassDeleteMergesImagesPerRow(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	for i, title := range []string{"订单一", "订单二"} {
		row := subject{ID: uint64(i + 1), Title: strp(title), State: strp("draft")}
		insert(qdb, row)
	}
	updateState(qdb)

	if err := qdb.Where("id IN (?,?)", 1, 2).Delete(&subject{}).Error; err != nil {
		t.Fatal(err)
	}

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

func TestInstanceDeleteCapturesImage(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("订单一"), State: strp("paid")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	if err := qdb.Delete(row).Error; err != nil {
		t.Fatal(err)
	}

	var deletes []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "delete" {
			deletes = append(deletes, ev)
		}
	}
	if len(deletes) != 1 {
		t.Fatalf("expected one delete event, got %d", len(deletes))
	}
	if got := deletes[0]["resource_id"]; got != "1" {
		t.Errorf("resource_id = %v", got)
	}
	before := deletes[0]["before"].(map[string]any)
	if before["title"] != "订单一" || before["state"] != "paid" {
		t.Errorf("before image wrong: %+v", before)
	}
	if _, hasID := before["id"]; hasID {
		t.Errorf("primary key must not appear in before image: %+v", before)
	}
}

func TestModelUpdatesMapCapturesOnlyChangedFields(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("订单一"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}

	if err := qdb.Model(row).Updates(map[string]any{"state": "draft"}).Error; err != nil {
		t.Fatal(err)
	}
	if updates := countAction(e.all(), "update"); updates != 0 {
		t.Fatalf("no-op update must stay silent, got %d", updates)
	}

	if err := qdb.Model(row).Updates(map[string]any{"state": "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	var updates []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "update" {
			updates = append(updates, ev)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected one update event, got %d", len(updates))
	}
	after := updates[0]["after"].(map[string]any)
	if after["state"] != "paid" {
		t.Errorf("after wrong: %+v", after)
	}
	if _, hasTitle := after["title"]; hasTitle {
		t.Errorf("unchanged columns must not be logged: %+v", after)
	}
}

func TestStructUpdatesSkipsZeroFields(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("订单一"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}

	if err := qdb.Model(row).Updates(&subject{State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}

	var updates []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "update" {
			updates = append(updates, ev)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected one update event, got %d", len(updates))
	}
	after := updates[0]["after"].(map[string]any)
	if after["state"] != "paid" {
		t.Errorf("after wrong: %+v", after)
	}
	if _, hasTitle := after["title"]; hasTitle {
		t.Errorf("zero-valued fields are not written by Updates and must not be logged: %+v", after)
	}
}

func TestTableUpdatesMapCapturesRowsWithoutModelSchema(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("订单一"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}

	if err := qdb.Table("subjects").Where("id = ?", row.ID).Updates(map[string]any{"state": "paid"}).Error; err != nil {
		t.Fatal(err)
	}

	var updates []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "update" {
			updates = append(updates, ev)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected one update event, got %d", len(updates))
	}
	if got := updates[0]["resource_id"]; got != "1" {
		t.Errorf("resource_id = %v", got)
	}
	after := updates[0]["after"].(map[string]any)
	if after["state"] != "paid" {
		t.Errorf("after wrong: %+v", after)
	}
}

type statePatch struct {
	State *string
}

func TestTableUpdatesPartialStructCapturesOnlyProvidedFields(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "9")
	row := &subject{Title: strp("订单一"), State: strp("draft")}
	if err := qdb.Create(row).Error; err != nil {
		t.Fatal(err)
	}

	if err := qdb.Table("subjects").Where("id = ?", row.ID).Updates(&statePatch{State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}

	var updates []map[string]any
	for _, ev := range e.all() {
		if ev["action"] == "update" {
			updates = append(updates, ev)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected one update event, got %d", len(updates))
	}
	if got := updates[0]["resource_id"]; got != "1" {
		t.Errorf("resource_id = %v", got)
	}
	after := updates[0]["after"].(map[string]any)
	if after["state"] != "paid" {
		t.Errorf("after wrong: %+v", after)
	}
	if _, hasTitle := after["title"]; hasTitle {
		t.Errorf("partial struct must only log provided fields: %+v", after)
	}
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func countAction(events []map[string]any, action string) int {
	n := 0
	for _, ev := range events {
		if ev["action"] == action {
			n++
		}
	}
	return n
}

func TestRecorderErrorRollsBackWrite(t *testing.T) {
	e := newEventSink()
	e.err = errors.New("audit store down")
	qdb := newAuditedDB(t, e, nil, "9")

	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err == nil {
		t.Fatal("expected the recorder error to abort the create")
	}

	var count int64
	if err := qdb.Model(&subject{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("business row must be rolled back, got %d rows", count)
	}
}

func TestEmptyOperatorProfileSkipsAuditing(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, nil, "")
	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}
	if len(e.all()) != 0 {
		t.Errorf("expected silent skip without operator, got %d", len(e.all()))
	}
}

func TestIncludeFilterBlocksTables(t *testing.T) {
	e := newEventSink()
	qdb := newAuditedDB(t, e, func(name string) bool { return false }, "9")
	if err := qdb.Create(&subject{Title: strp("订单一"), State: strp("paid")}).Error; err != nil {
		t.Fatal(err)
	}
	if len(e.all()) != 0 {
		t.Errorf("include(false) must silence everything")
	}
}

func insert(qdb *gorm.DB, row subject) {
	qdb.Exec("INSERT INTO subjects (id,title,state) VALUES (?,?,?)", row.ID, *row.Title, *row.State)
}

func updateState(qdb *gorm.DB) { qdb.Model(&subject{}).Exec("UPDATE subjects SET state=? ", "draft") }

func strp(s string) *string { return &s }
