package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
)

type DB struct {
	Path string // kv.Path

	// === internals ===

	kv     KV
	tables map[string]*TableDef // cached table schemas
}

// table schema
type TableDef struct {
	// === user-defined ===

	Name  string
	Types []uint32 // column types
	Cols  []string // column names
	PKeys int      // the first 'PKeys' columns are primary key (Cols[:PKeys])

	// === auto-assigned ===

	Prefix uint32 // key prefix for different tables (share a single B+tree)
}

// data types
const (
	TYPE_BYTES = 1 // arbitrary-length string
	TYPE_INT64 = 2 // 64-bit signed integer
)

// table cell
type Value struct {
	Type uint32
	I64  int64
	Str  []byte
}

// table row
type Record struct {
	Cols []string
	Vals []Value
}

func (rec *Record) AddStr(col string, val []byte) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_BYTES, Str: val})
	return rec
}

func (rec *Record) AddInt64(col string, val int64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_INT64, I64: val})
	return rec
}

func (rec *Record) Get(col string) *Value {
	for i, c := range rec.Cols {
		if c == col {
			return &rec.Vals[i]
		}
	}
	return nil
}

// rearrange record to match defined column order
func reorderRecord(tdef *TableDef, rec Record) ([]Value, error) {
	assert(len(rec.Cols) == len(rec.Vals))
	out := make([]Value, len(tdef.Cols))
	for i, c := range tdef.Cols {
		v := rec.Get(c)
		if v == nil {
			continue // leave uninitialized
		}
		if v.Type != tdef.Types[i] {
			return nil, fmt.Errorf("bad column type: %s", c)
		}
		out[i] = *v
	}
	return out, nil
}

// ensure no missing/redundant columns
// ('vals' has been rearranged to match defined column order)
func valuesComplete(tdef *TableDef, vals []Value, n int) error {
	for i, v := range vals {
		if i < n && v.Type == 0 {
			return fmt.Errorf("missing column: %s", tdef.Cols[i])
		} else if i >= n && v.Type != 0 {
			return fmt.Errorf("extra column: %s", tdef.Cols[i])
		}
	}
	return nil
}

// rearrange record to match defined column order;
// check for missing/redundant columns;
//
// - n == tdef.PKeys: record is exactly a primary key
// - n == len(tdef.Cols): record contains all columns
func checkRecord(tdef *TableDef, rec Record, n int) ([]Value, error) {
	vals, err := reorderRecord(tdef, rec)
	if err != nil {
		return nil, err
	}

	err = valuesComplete(tdef, vals, n)
	if err != nil {
		return nil, err
	}
	return vals, nil
}

func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	// table prefix
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], prefix)
	out = append(out, buf[:]...)

	// order-preserving encoded key
	out = encodeValues(out, vals)
	return out

}

// order-preserving encoding
//
// TODO: more detailed explanation
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		switch v.Type {
		case TYPE_INT64:
			var buf [8]byte
			u := uint64(v.I64) + (1 << 63)        // flip the sign bit
			binary.BigEndian.PutUint64(buf[:], u) // big endian
			out = append(out, buf[:]...)
		case TYPE_BYTES:
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0) // null-terminated
		default:
			panic("unreachable")
		}
	}
	return out
}

// escape the null byte (0); use 0x01 as escaping byte
//
// TODO: more detailed explanation
func escapeString(in []byte) []byte {
	toEscape := bytes.Count(in, []byte{0}) + bytes.Count(in, []byte{1})
	if toEscape == 0 {
		return in
	}

	out := make([]byte, len(in)+toEscape)
	pos := 0
	for _, ch := range in {
		if ch <= 1 {
			// 00 -> 01 01
			// 01 -> 01 02
			out[pos] = 0x01
			out[pos+1] = ch + 1
			pos += 2
		} else {
			out[pos] = ch
			pos += 1
		}
	}
	return out
}

func unescapeString(in []byte) []byte {
	if bytes.Count(in, []byte{1}) == 0 {
		return in
	}

	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == 0x01 {
			// 01 01 -> 00
			// 01 02 -> 01
			i++
			assert(in[i] == 1 || in[i] == 2)
			out = append(out, in[i]-1)
		} else {
			out = append(out, in[i])
		}
	}
	return out
}

func decodeValues(in []byte, out []Value) {
	for i := range out {
		switch out[i].Type {
		case TYPE_INT64:
			u := binary.BigEndian.Uint64(in[:8])
			out[i].I64 = int64(u - (1 << 63))
			in = in[8:]
		case TYPE_BYTES:
			idx := bytes.IndexByte(in, 0)
			assert(idx >= 0)
			out[i].Str = unescapeString(in[:idx])
			in = in[idx+1:]
		default:
			panic("unreachable")
		}
	}
	assert(len(in) == 0)
}

// internal table: metadata
// - auto-incrementing counter for generating table prefixes.
// - ...
var TDEF_META = &TableDef{
	Prefix: 1,
	Name:   "@meta",
	Types:  []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:   []string{"key", "val"},
	PKeys:  1,
}

// internal table: table schemas
var TDEF_TABLE = &TableDef{
	Prefix: 2,
	Name:   "@table",
	Types:  []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:   []string{"name", "def"},
	PKeys:  1,
}

var INTERNAL_TABLES map[string]*TableDef = map[string]*TableDef{
	TDEF_META.Name:  TDEF_META,
	TDEF_TABLE.Name: TDEF_TABLE,
}

// get table schema by name
func getTableDef(db *DB, name string) *TableDef {
	// check internal schemas cache
	if tdef, ok := INTERNAL_TABLES[name]; ok {
		return tdef
	}

	// check schemas cache
	if tdef := db.tables[name]; tdef != nil {
		return tdef
	}

	// query schemas table
	tdef := getTableDefDB(db, name)
	if tdef != nil { // cache if found
		db.tables[name] = tdef
	}

	return tdef

}

// get table schema from internal table '@table'
func getTableDefDB(db *DB, name string) *TableDef {
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := dbGet(db, TDEF_TABLE, rec)
	assert(err == nil)
	if !ok {
		return nil
	}

	tdef := &TableDef{}
	err = json.Unmarshal(rec.Get("def").Str, tdef)
	assert(err == nil)
	return tdef
}

// get a row by primary key
// ('rec' contains input primary key and also is output container)
func dbGet(db *DB, tdef *TableDef, rec *Record) (bool, error) {
	// reorder input columns according to schema
	values, err := checkRecord(tdef, *rec, tdef.PKeys)
	if err != nil {
		return false, err
	}

	// encode the primary key
	key := encodeKey(nil, tdef.Prefix, values[:tdef.PKeys])

	// query the KV store
	val, ok := db.kv.Get(key)
	if !ok {
		return false, nil
	}

	// decode value into columns
	for i := tdef.PKeys; i < len(tdef.Cols); i++ {
		values[i].Type = tdef.Types[i]
	}
	decodeValues(val, values[tdef.PKeys:])
	rec.Cols = tdef.Cols
	rec.Vals = values
	return true, nil
}

// get a row by primary key
func (db *DB) Get(table string, rec *Record) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbGet(db, tdef, rec)
}

const TABLE_PREFIX_MIN = 100

// create table
func (db *DB) TableNew(tdef *TableDef) error {
	// verify table schema
	if err := tableDefChecK(tdef); err != nil {
		return err
	}

	// check for existing table name
	tableRec := (&Record{}).AddStr("name", []byte(tdef.Name))
	ok, err := dbGet(db, TDEF_TABLE, tableRec)
	assert(err == nil)
	if ok {
		return fmt.Errorf("table exists: %s", tdef.Name)
	}

	// allocate prefix
	assert(tdef.Prefix == 0)
	metaRec := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err = dbGet(db, TDEF_META, metaRec)
	assert(err == nil)
	if ok {
		tdef.Prefix = binary.LittleEndian.Uint32(metaRec.Get("val").Str)
		assert(tdef.Prefix > TABLE_PREFIX_MIN)
	} else {
		tdef.Prefix = TABLE_PREFIX_MIN
		metaRec.AddStr("val", make([]byte, 4))
	}

	// set next prefix
	binary.LittleEndian.PutUint32(metaRec.Get("val").Str, tdef.Prefix+1)
	_, err = dbUpdate(db, TDEF_META, &DBUpdateReq{Record: *metaRec})
	if err != nil {
		return err
	}

	// store the schema
	jsonTDef, err := json.Marshal(tdef)
	assert(err == nil)
	tableRec.AddStr("def", jsonTDef)
	_, err = dbUpdate(db, TDEF_TABLE, &DBUpdateReq{Record: *tableRec})
	return err
}

// verify table schema
func tableDefChecK(tdef *TableDef) error {
	bad := tdef.Name == "" || len(tdef.Cols) == 0
	bad = bad || len(tdef.Cols) != len(tdef.Types)
	bad = bad || !(1 <= tdef.PKeys && tdef.PKeys <= len(tdef.Cols))
	if bad {
		return fmt.Errorf("bad table schema: %s", tdef.Name)
	}
	return nil
}

type DBUpdateReq struct {
	// === in ===

	Record Record
	Mode   int

	// === out ===
	Updated bool
	Added   bool
}

// insert/update a row
func dbUpdate(
	db *DB, tdef *TableDef, dbUpdateReq *DBUpdateReq,
) (bool, error) {
	// reorder record to match defined column order
	values, err := checkRecord(tdef, dbUpdateReq.Record, len(tdef.Cols))
	if err != nil {
		return false, nil
	}

	key := encodeKey(nil, tdef.Prefix, values[:tdef.PKeys])
	val := encodeValues(nil, values[tdef.PKeys:])
	req := UpdateReq{Key: key, Val: val, Mode: dbUpdateReq.Mode}
	if _, err = db.kv.Update(&req); err != nil {
		return false, err
	}

	dbUpdateReq.Added, dbUpdateReq.Updated = req.Added, req.Updated
	return req.Updated, nil
}

// insert/update a row
func (db *DB) Set(table string, dbUpdateReq *DBUpdateReq) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbUpdate(db, tdef, dbUpdateReq)
}

func (db *DB) Insert(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_INSERT_ONLY,
	})
}

func (db *DB) Update(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_UPDATE_ONLY,
	})
}

func (db *DB) Upsert(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_UPSERT,
	})
}

// delete a row by primary key
func dbDelete(db *DB, tdef *TableDef, rec Record) (bool, error) {
	// reorder record to match defined column order
	values, err := checkRecord(tdef, rec, tdef.PKeys)
	if err != nil {
		return false, nil
	}

	key := encodeKey(nil, tdef.Prefix, values[:tdef.PKeys])
	return db.kv.Del(key)
}

// delete a row by primary key
func (db *DB) Delete(table string, rec Record) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbDelete(db, tdef, rec)
}

func (db *DB) Open() error {
	db.kv.Path = db.Path
	db.tables = map[string]*TableDef{}
	return db.kv.Open()
}

func (db *DB) Close() {
	db.kv.Close()
}
