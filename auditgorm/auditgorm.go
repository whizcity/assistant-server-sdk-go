// Package auditgorm auto-captures gorm (jinzhu/gorm v1) writes into
// assistant-server audit events — the Go counterpart of the PHP SDK's
// Auditable trait. Register once per *gorm.DB: instance creates, instance
// updates, and mass Update/Delete chains that carry conditions all get captured.
//
// Bypasses that still need manual treatment (same as the PHP SDK):
// db.Table(...) writes without a model and raw Exec SQL.
//
// Events are POSTed asynchronously (goroutine) so a gorm call never waits for
// HTTP inside its own transaction; failures stay silent unless the client has
// DebugLog set.
package auditgorm

import (
	"fmt"
	"strings"
	"reflect"
	"time"

	gorm "github.com/jinzhu/gorm"
	as "github.com/whizcity/assistant-server-sdk-go"
)

// sensitive mirrors the PHP config's never-log list.
var sensitive = map[string]struct{}{
	"password": {}, "password_confirmation": {}, "remember_token": {},
	"api_token": {}, "secret": {}, "id_card": {},
}

// Register wires the three write callbacks into the gorm scope.
//   - profile is invoked per captured operation (request-scoped operator
//     identity); when it returns an EventProfile with an empty OperatorID the
//     operation is silently skipped (no admin context = no audit row, PHP parity).
//   - include is an optional table filter; nil allows every table.
func Register(db *gorm.DB, client *as.Client, profile func() as.EventProfile, include func(table string) bool) error {
	guard := func(scope *gorm.Scope) (as.EventProfile, string, bool) {
		table := scope.TableName()
		if include != nil && !include(table) {
			return as.EventProfile{}, table, false
		}
		if profile == nil {
			return as.EventProfile{}, table, false
		}
		p := profile()
		if p.OperatorID == "" {
			return as.EventProfile{}, table, false
		}
		return p, table, true
	}

	db.Callback().Create().After("gorm:create").Register("as:audit_create", func(scope *gorm.Scope) {
		p, table, ok := guard(scope)
		if !ok {
			return
		}
		emitCreate(client, p, table, scope)
	})
	db.Callback().Update().Before("gorm:before_update").Register("as:audit_update", func(scope *gorm.Scope) {
		p, table, ok := guard(scope)
		if !ok {
			return
		}
		emitUpdate(client, p, table, scope)
	})
	db.Callback().Delete().Before("gorm:before_delete").Register("as:audit_delete", func(scope *gorm.Scope) {
		p, table, ok := guard(scope)
		if !ok {
			return
		}
		emitDelete(client, p, table, scope)
	})
	return nil
}

func emitCreate(client *as.Client, p as.EventProfile, table string, scope *gorm.Scope) {
	after := map[string]any{}
	var resourceID string
	for _, field := range scope.Fields() {
		if field.DBName == "" || field.IsBlank {
			continue
		}
		after[field.DBName] = normalizeValue(fieldValue(field))
		if field.IsPrimaryKey {
			resourceID = fmt.Sprint(fieldValue(field))
		}
	}
	sendAsync(client, []map[string]any{as.NewEvent("create", p.Service, table, resourceID, nil, after, p)})
}

func emitUpdate(client *as.Client, p as.EventProfile, table string, scope *gorm.Scope) {
	// set columns are filled by assignUpdatingAttributesCallback, which always
	// registered ahead of gorm:before_update.
	var setMap map[string]any
	attrsRaw, hasAttrs := scope.InstanceGet("gorm:update_attrs")
	if hasAttrs {
		setMap, _ = attrsRaw.(map[string]any)
	}

	condition, variables := snapshotCondition(scope)
	allRows := queryRows(scope, condition, variables)
	if len(allRows) == 0 {
		return
	}

	if !hasAttrs {
		// Struct save path: gorm v1 has no dirty set — after = the model's
		// current field values, before = the row as it sat in the database.
		if len(allRows) != 1 {
			return
		}
		var key string
		after := map[string]any{}
		for _, field := range scope.Fields() {
			if field.DBName == "" || field.IsBlank {
				continue
			}
			value := normalizeValue(fieldValue(field))
			if field.IsPrimaryKey {
				key = fmt.Sprint(fieldValue(field))
			}
			if field.DBName == "id" {
				continue
			}
			after[field.DBName] = value
		}
		before := auditPayload(deleteKey(normalizeRow(allRows[key]), "id"))
		if reflect.DeepEqual(before, after) {
			return
		}
		sendAsync(client, []map[string]any{as.NewEvent("update", p.Service, table, key, before, after, p)})
		return
	}
	if len(setMap) == 0 {
		return
	}

	type pair struct {
		key    string
		before map[string]any
	}
	var pairsL []pair
	for key, row := range allRows {
		if _, ok := row["id"]; !ok {
			continue
		}
		before := intersectColumns(row, setMap)
		afterRow := setAfter(setMap)
		if reflect.DeepEqual(before, afterRow) {
			continue
		}
		pairsL = append(pairsL, pair{key, before})
	}
	if len(pairsL) == 0 {
		return
	}
	if len(pairsL) == 1 {
		first := pairsL[0]
		sendAsync(client, []map[string]any{as.NewEvent("update", p.Service, table, first.key, first.before, setAfter(setMap), p)})
		return
	}
	images := map[string]as.UpdatePair{}
	for _, it := range pairsL {
		images[it.key] = as.UpdatePair{Before: it.before, After: setAfter(setMap)}
	}
	sendAsync(client, as.MergeUpdateEvents(p.Service, table, images, p))
}

func emitDelete(client *as.Client, p as.EventProfile, table string, scope *gorm.Scope) {
	condition, variables := snapshotCondition(scope)
	rows := queryRows(scope, condition, variables)
	if len(rows) == 0 {
		return
	}
	if len(rows) == 1 {
		for key, row := range rows {
			image := auditPayload(normalizeRow(row))
			image = deleteKey(image, "id")
			sendAsync(client, []map[string]any{as.NewEvent("delete", p.Service, table, key, image, nil, p)})
		}
		return
	}
	images := map[string]map[string]any{}
	for key, row := range rows {
		image := auditPayload(normalizeRow(row))
		image = deleteKey(image, "id")
		images[key] = image
	}
	sendAsync(client, as.MergeDeleteEvents(p.Service, table, images, p))
}

func setAfter(setMap map[string]any) map[string]any { return auditPayload(setMap) }

func deleteKey(m map[string]any, key string) map[string]any {
	delete(m, key)
	return m
}

// snapshotCondition rebuilds the pending WHERE clause + its bind values BEFORE
// gorm composes the UPDATE/DELETE SQL, so the audit SELECT never disturbs the
// parent chain's SQLVars ordering. shortcut: the clause still carries
// JOIN/ORDER/LIMIT pieces (CombinedConditionSql); extend when chains beyond a
// plain WHERE actually need auditing.
func snapshotCondition(scope *gorm.Scope) (condition string, variables []any) {
	// gorm marks bind vars as "$$$" pre-Raw; the wire dialect wants "?".
	condition = strings.ReplaceAll(scope.CombinedConditionSql(), "$$$", "?")
	variables = make([]any, len(scope.SQLVars))
	copy(variables, scope.SQLVars)
	scope.SQLVars = nil
	return condition, variables
}

func queryRows(scope *gorm.Scope, condition string, variables []any) map[string]map[string]any {
	query := "SELECT * FROM " + scope.QuotedTableName() + " " + condition
	statement, err := scope.SQLDB().Query(query, variables...)
	if err != nil {
		return nil
	}
	defer func() { _ = statement.Close() }()
	cols, err := statement.Columns()
	if err != nil {
		return nil
	}
	rows := map[string]map[string]any{}
	for statement.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = statement.Scan(pointers...); err != nil {
			return nil
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = normalizeValue(values[i])
		}
		rows[fmt.Sprint(row["id"])] = row
	}
	return rows
}

// normalizeRow makes scanned values audit-safe ([]byte → string etc.) for events.
func normalizeRow(row map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range row {
		out[key] = normalizeValue(value)
	}
	return out
}

// intersectColumns narrows the before image to the columns being written and
// strips the PK, mirroring the PHP builder's before-image shape.
func intersectColumns(row map[string]any, setMap map[string]any) map[string]any {
	out := map[string]any{}
	for column := range setMap {
		if column == "id" {
			continue
		}
		if value, ok := row[column]; ok {
			out[column] = normalizeValue(value)
		}
	}
	return auditPayload(out)
}

// auditPayload strips sensitive fields from a wire payload.
func auditPayload(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range data {
		if _, blocked := sensitive[key]; blocked {
			continue
		}
		out[key] = normalizeValue(value)
	}
	return out
}

func normalizeValue(value any) any {
	if value == nil {
		return nil
	}
	if rv := reflect.ValueOf(value); rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		value = rv.Elem().Interface()
	}
	switch v := value.(type) {
	case string, int, int64, float64, bool:
		return v
	case []byte:
		return string(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(value)
	}
}

// fieldValue reads a live reflection value from a gorm field.
func fieldValue(field *gorm.Field) any {
	if !field.Field.IsValid() || !field.Field.IsZero() == false && field.Field.Kind() == reflect.Invalid {
		// unreachable guard kept minimal
	}
	if !field.Field.IsValid() {
		return nil
	}
	return field.Field.Interface()
}

func sendAsync(client *as.Client, events []map[string]any) {
	if client == nil || len(events) == 0 {
		return
	}
	batch := make([]map[string]any, len(events))
	for i := range events {
		copy := map[string]any{}
		for key, value := range events[i] {
			copy[key] = value
		}
		batch[i] = copy
	}
	go func() { _ = client.SyncBatch(batch) }()
}
