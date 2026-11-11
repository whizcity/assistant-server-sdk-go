// Package auditgorm auto-captures gorm v2 writes into assistant-server audit
// events — the Go counterpart of the PHP SDK's Auditable trait. Register once
// per *gorm.DB: instance creates, instance updates, and mass Update/Delete
// chains that carry conditions all get captured.
//
// Bypasses that still need manual treatment (same as the PHP SDK): raw Exec
// SQL and Create calls whose destination is a map.
//
// Before-images are read through the current transaction's ConnPool. Events
// are POSTed asynchronously (goroutine) after the SQL succeeds, so a gorm call
// never waits for HTTP; failures stay silent unless the client has DebugLog.
package auditgorm

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	as "github.com/whizcity/assistant-server-sdk-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// sensitive mirrors the PHP config's never-log list.
var sensitive = map[string]struct{}{
	"password": {}, "password_confirmation": {}, "remember_token": {},
	"api_token": {}, "secret": {}, "id_card": {},
}

const (
	updateSnapshotKey = "as:audit_update_snapshot"
	deleteSnapshotKey = "as:audit_delete_snapshot"
)

type snapshot struct {
	profile    as.EventProfile
	table      string
	primaryKey string
	instanceID string
	before     map[string]any
	rows       map[string]map[string]any
}

// Register wires the write callbacks into gorm v2.
//   - profile is invoked per captured operation with the statement context.
//     When it returns an EventProfile with an empty OperatorID the operation is
//     silently skipped (no admin context = no audit row, PHP parity).
//   - include is an optional table filter; nil allows every table.
func Register(db *gorm.DB, client *as.Client, profile func(context.Context) as.EventProfile, include func(table string) bool) error {
	if db == nil {
		return errors.New("auditgorm: nil db")
	}
	guard := func(tx *gorm.DB) (as.EventProfile, string, bool) {
		table := statementTable(tx.Statement)
		if table == "" || (include != nil && !include(table)) || profile == nil {
			return as.EventProfile{}, table, false
		}
		ctx := tx.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		p := profile(ctx)
		if p.OperatorID == "" {
			return as.EventProfile{}, table, false
		}
		return p, table, true
	}

	db.Callback().Create().After("gorm:create").Register("as:audit_create", func(tx *gorm.DB) {
		if tx.Error != nil || tx.RowsAffected == 0 {
			return
		}
		p, table, ok := guard(tx)
		if !ok {
			return
		}
		emitCreate(client, p, table, tx)
	})

	db.Callback().Update().Before("gorm:before_update").Register("as:audit_update_before", func(tx *gorm.DB) {
		p, table, ok := guard(tx)
		if !ok {
			return
		}
		captureUpdate(tx, p, table)
	})
	db.Callback().Update().After("gorm:update").Register("as:audit_update_after", func(tx *gorm.DB) {
		if tx.Error != nil || tx.RowsAffected == 0 {
			return
		}
		emitUpdate(client, tx)
	})

	db.Callback().Delete().Before("gorm:before_delete").Register("as:audit_delete_before", func(tx *gorm.DB) {
		p, table, ok := guard(tx)
		if !ok {
			return
		}
		captureDelete(tx, p, table)
	})
	db.Callback().Delete().After("gorm:delete").Register("as:audit_delete_after", func(tx *gorm.DB) {
		if tx.Error != nil || tx.RowsAffected == 0 {
			return
		}
		emitDelete(client, tx)
	})
	return nil
}

func emitCreate(client *as.Client, p as.EventProfile, table string, tx *gorm.DB) {
	stmt := tx.Statement
	rv := deref(stmt.ReflectValue)
	switch rv.Kind() {
	case reflect.Struct:
		after, id := valuesOf(stmt, rv, false, false)
		if len(after) == 0 {
			return
		}
		sendAsync(client, []map[string]any{as.NewEvent(as.ActionCreate, p.Service, table, id, nil, after, p)})
	case reflect.Slice, reflect.Array:
		if rv.Len() == 0 {
			return
		}
		rows := make([]map[string]any, 0, rv.Len())
		var id string
		for i := 0; i < rv.Len(); i++ {
			after, rowID := valuesOf(stmt, rv.Index(i), false, false)
			if len(after) == 0 {
				continue
			}
			rows = append(rows, after)
			if id == "" {
				id = rowID
			}
		}
		switch {
		case len(rows) == 1:
			sendAsync(client, []map[string]any{as.NewEvent(as.ActionCreate, p.Service, table, id, nil, rows[0], p)})
		case len(rows) > 1:
			sendAsync(client, as.MergeInsertEvents(p.Service, table, rows, p))
		}
	}
}

func captureUpdate(tx *gorm.DB, p as.EventProfile, table string) {
	stmt := tx.Statement
	snap := snapshot{profile: p, table: table, primaryKey: primaryKeyName(stmt.Schema)}
	if id, raw, ok := singlePK(stmt); ok {
		snap.instanceID = id
		snap.before = fetchRow(tx, table, snap.primaryKey, raw)
		if snap.before == nil {
			return
		}
	} else {
		snap.rows = fetchRows(tx, table, snap.primaryKey)
		if len(snap.rows) == 0 {
			return
		}
	}
	tx.InstanceSet(updateSnapshotKey, snap)
}

func captureDelete(tx *gorm.DB, p as.EventProfile, table string) {
	stmt := tx.Statement
	snap := snapshot{profile: p, table: table, primaryKey: primaryKeyName(stmt.Schema)}
	if id, raw, ok := singlePK(stmt); ok {
		snap.instanceID = id
		snap.before = fetchRow(tx, table, snap.primaryKey, raw)
		if snap.before == nil {
			return
		}
	} else {
		snap.rows = fetchRows(tx, table, snap.primaryKey)
		if len(snap.rows) == 0 {
			return
		}
	}
	tx.InstanceSet(deleteSnapshotKey, snap)
}

func emitUpdate(client *as.Client, tx *gorm.DB) {
	raw, ok := tx.InstanceGet(updateSnapshotKey)
	if !ok {
		return
	}
	snap, ok := raw.(snapshot)
	if !ok {
		return
	}
	after := afterValues(tx)
	if len(after) == 0 {
		return
	}

	if snap.instanceID != "" {
		changed := changedAfter(snap.before, after)
		if len(changed) == 0 || onlyAutoUpdate(tx.Statement, changed) {
			return
		}
		before := beforeFor(snap.before, changed)
		sendAsync(client, []map[string]any{as.NewEvent(as.ActionUpdate, snap.profile.Service, snap.table, snap.instanceID, before, changed, snap.profile)})
		return
	}

	pairs := map[string]as.UpdatePair{}
	for key, row := range snap.rows {
		changed := changedAfter(row, after)
		if len(changed) == 0 || onlyAutoUpdate(tx.Statement, changed) {
			continue
		}
		pairs[key] = as.UpdatePair{Before: beforeFor(row, changed), After: changed}
	}
	switch {
	case len(pairs) == 1:
		for key, pair := range pairs {
			sendAsync(client, []map[string]any{as.NewEvent(as.ActionUpdate, snap.profile.Service, snap.table, key, pair.Before, pair.After, snap.profile)})
		}
	case len(pairs) > 1:
		sendAsync(client, as.MergeUpdateEvents(snap.profile.Service, snap.table, pairs, snap.profile))
	}
}

func emitDelete(client *as.Client, tx *gorm.DB) {
	raw, ok := tx.InstanceGet(deleteSnapshotKey)
	if !ok {
		return
	}
	snap, ok := raw.(snapshot)
	if !ok {
		return
	}
	if snap.instanceID != "" {
		image := deleteKey(auditPayload(snap.before), snap.primaryKey)
		sendAsync(client, []map[string]any{as.NewEvent(as.ActionDelete, snap.profile.Service, snap.table, snap.instanceID, image, nil, snap.profile)})
		return
	}

	images := map[string]map[string]any{}
	for key, row := range snap.rows {
		images[key] = deleteKey(auditPayload(row), snap.primaryKey)
	}
	switch {
	case len(images) == 1:
		for key, image := range images {
			sendAsync(client, []map[string]any{as.NewEvent(as.ActionDelete, snap.profile.Service, snap.table, key, image, nil, snap.profile)})
		}
	case len(images) > 1:
		sendAsync(client, as.MergeDeleteEvents(snap.profile.Service, snap.table, images, snap.profile))
	}
}

// valuesOf converts a model row into a column map. includeZero mirrors gorm's
// Save semantics (all selected columns) versus Create/Updates (non-zero only).
func valuesOf(stmt *gorm.Statement, rv reflect.Value, includeZero, includePK bool) (map[string]any, string) {
	out := map[string]any{}
	if stmt.Schema == nil || !rv.IsValid() {
		return out, ""
	}
	var id string
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" {
			continue
		}
		value, zero := field.ValueOf(stmt.Context, rv)
		if field.PrimaryKey {
			id = fmt.Sprint(normalizeValue(value))
			if !includePK {
				continue
			}
		}
		if zero && !includeZero {
			continue
		}
		out[field.DBName] = normalizeValue(value)
	}
	return auditPayload(out), id
}

// afterValues reads the values gorm is about to write. Map destinations are
// taken as-is; struct destinations follow ConvertToAssignments' field rules.
func afterValues(tx *gorm.DB) map[string]any {
	stmt := tx.Statement
	out := map[string]any{}
	if stmt.Schema == nil {
		if m, ok := stmt.Dest.(map[string]any); ok {
			for key, value := range m {
				out[key] = normalizeValue(value)
			}
		}
		return auditPayload(out)
	}

	sel, restricted := stmt.SelectAndOmitColumns(false, true)
	if m, ok := stmt.Dest.(map[string]any); ok {
		for key, value := range m {
			column := key
			if field := stmt.Schema.LookUpField(key); field != nil {
				column = field.DBName
			}
			allowed, found := sel[column]
			if found && !allowed {
				continue // explicitly omitted or not updatable
			}
			if restricted && !found {
				continue
			}
			out[column] = normalizeValue(value)
		}
		addAutoUpdate(tx, out)
		return auditPayload(out)
	}

	rv := deref(reflect.ValueOf(stmt.Dest))
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return auditPayload(out)
	}
	destSchema := stmt.Schema
	if rv.Type() != stmt.Schema.ModelType {
		parsed := &gorm.Statement{DB: stmt.DB}
		if err := parsed.Parse(stmt.Dest); err == nil && parsed.Schema != nil {
			destSchema = parsed.Schema
		}
	}
	for _, field := range stmt.Schema.FieldsByDBName {
		if field.PrimaryKey || field.AutoUpdateTime > 0 {
			continue
		}
		updating := destSchema.LookUpField(field.DBName)
		if updating == nil || !updating.Updatable {
			continue
		}
		selected, explicitly := sel[field.DBName]
		if restricted && (!explicitly || !selected) {
			continue
		}
		value, zero := updating.ValueOf(stmt.Context, rv)
		if zero && !(explicitly && selected) {
			continue
		}
		out[field.DBName] = normalizeValue(value)
	}
	addAutoUpdate(tx, out)
	return auditPayload(out)
}

// addAutoUpdate includes gorm-managed updated_at columns, which are filled in
// during the update callback rather than present in a map destination.
func addAutoUpdate(tx *gorm.DB, out map[string]any) {
	stmt := tx.Statement
	if stmt.SkipHooks || stmt.Schema == nil {
		return
	}
	rv := stmt.ReflectValue
	for rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		if rv.Len() == 0 {
			return
		}
		rv = rv.Index(0)
	}
	rv = deref(rv)
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return
	}
	for _, field := range stmt.Schema.FieldsByDBName {
		if field.AutoUpdateTime == 0 {
			continue
		}
		if _, exists := out[field.DBName]; exists {
			continue // an explicit map value wins over the auto timestamp
		}
		value, _ := field.ValueOf(stmt.Context, rv)
		out[field.DBName] = normalizeValue(value)
	}
}

func singlePK(stmt *gorm.Statement) (string, any, bool) {
	if stmt.Schema == nil || len(stmt.Schema.PrimaryFields) != 1 {
		return "", nil, false
	}
	rv := deref(stmt.ReflectValue)
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return "", nil, false
	}
	field := stmt.Schema.PrimaryFields[0]
	value, zero := field.ValueOf(stmt.Context, rv)
	if zero {
		return "", nil, false
	}
	return fmt.Sprint(normalizeValue(value)), value, true
}

func primaryKeyName(sch *schema.Schema) string {
	if sch != nil && len(sch.PrimaryFields) == 1 {
		return sch.PrimaryFields[0].DBName
	}
	return "id"
}

// fetchRow reads one instance row through the transaction's own connection.
func fetchRow(tx *gorm.DB, table, primaryKey string, key any) map[string]any {
	var row map[string]any
	if err := baseSnapshotQuery(tx, table).Where(clause.Eq{Column: primaryKey, Value: key}).Take(&row).Error; err != nil {
		return nil
	}
	if row == nil {
		return nil
	}
	return normalizeRow(row)
}

// fetchRows snapshots every row a mass Update/Delete will touch. shortcut:
// composite primary keys are keyed by their first column only; add tuple keys
// when a table actually needs them.
func fetchRows(tx *gorm.DB, table, primaryKey string) map[string]map[string]any {
	stmt := tx.Statement
	_, hasWhere := stmt.Clauses["WHERE"]
	ids, hasIDs := slicePKValues(stmt)
	if !hasWhere && !hasIDs {
		return nil // global write: let gorm's AllowGlobalUpdate guard decide
	}
	query := baseSnapshotQuery(tx, table)
	if hasIDs {
		query = query.Where(clause.IN{Column: primaryKey, Values: ids})
	}
	var rows []map[string]any
	if err := query.Find(&rows).Error; err != nil {
		return nil
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		row = normalizeRow(row)
		key := fmt.Sprint(row[primaryKey])
		if key == "" || key == "<nil>" {
			continue
		}
		out[key] = row
	}
	return out
}

// baseSnapshotQuery rebuilds a read-only query carrying the pending WHERE,
// ORDER and LIMIT clauses. The SELECT shares the parent's ConnPool, so it sees
// the transaction's uncommitted state without disturbing its Vars.
func baseSnapshotQuery(tx *gorm.DB, table string) *gorm.DB {
	stmt := tx.Statement
	query := tx.Session(&gorm.Session{NewDB: true}).Table(table)
	if stmt.Model != nil {
		query = query.Model(stmt.Model)
	}
	if stmt.Unscoped {
		query = query.Unscoped()
	}
	for _, name := range []string{"WHERE", "ORDER BY", "LIMIT"} {
		if c, ok := stmt.Clauses[name]; ok {
			query.Statement.Clauses[name] = c
		}
	}
	return query
}

func slicePKValues(stmt *gorm.Statement) ([]any, bool) {
	if stmt.Schema == nil || len(stmt.Schema.PrimaryFields) != 1 {
		return nil, false
	}
	rv := deref(stmt.ReflectValue)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	field := stmt.Schema.PrimaryFields[0]
	values := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		value, zero := field.ValueOf(stmt.Context, rv.Index(i))
		if zero {
			continue
		}
		values = append(values, value)
	}
	return values, len(values) > 0
}

func statementTable(stmt *gorm.Statement) string {
	if stmt.Table != "" {
		return stmt.Table
	}
	if stmt.Schema != nil {
		return stmt.Schema.Table
	}
	return ""
}

func deref(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func beforeFor(row, after map[string]any) map[string]any {
	out := map[string]any{}
	for column := range after {
		if value, ok := row[column]; ok {
			out[column] = normalizeValue(value)
		}
	}
	return auditPayload(out)
}

// changedAfter keeps only columns whose value differs from the before image.
func changedAfter(before, after map[string]any) map[string]any {
	out := map[string]any{}
	for column, value := range after {
		old, ok := before[column]
		if !ok || !reflect.DeepEqual(normalizeValue(old), normalizeValue(value)) {
			out[column] = value
		}
	}
	return auditPayload(out)
}

// onlyAutoUpdate ignores GORM's timestamp-only write (a no-op Updates still
// bumps updated_at and RowsAffected on MySQL).
func onlyAutoUpdate(stmt *gorm.Statement, changed map[string]any) bool {
	if len(changed) != 1 || stmt.Schema == nil {
		return false
	}
	for column := range changed {
		field := stmt.Schema.LookUpField(column)
		return field != nil && field.AutoUpdateTime > 0
	}
	return false
}

func deleteKey(m map[string]any, key string) map[string]any {
	delete(m, key)
	return m
}

func normalizeRow(row map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range row {
		out[key] = normalizeValue(value)
	}
	return out
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
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	switch v := rv.Interface().(type) {
	case string:
		return v
	case bool:
		// MySQL scans tinyint(1) as int64; normalize Go bools the same way so
		// before/after comparisons stay stable.
		if v {
			return int64(1)
		}
		return int64(0)
	case []byte:
		return string(v)
	case int:
		return int64(v)
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case uint:
		return int64(v)
	case uint8:
		return int64(v)
	case uint16:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v)
		}
		return v
	case float32:
		return float64(v)
	case float64:
		return v
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case driver.Valuer:
		if concrete, err := v.Value(); err == nil {
			return normalizeValue(concrete)
		}
		return fmt.Sprint(value)
	default:
		return fmt.Sprint(value)
	}
}

func sendAsync(client *as.Client, events []map[string]any) {
	if client == nil || len(events) == 0 {
		return
	}
	batch := make([]map[string]any, len(events))
	for i := range events {
		copied := map[string]any{}
		for key, value := range events[i] {
			copied[key] = value
		}
		batch[i] = copied
	}
	go func() { _ = client.SyncBatch(batch) }()
}
