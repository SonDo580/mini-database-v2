package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
)

type DB struct {
	Path string // kv.Path

	// === internals ===

	kv     KV
	tables map[string]*TableDef // cached table schemas
	mu     sync.Mutex           // for table schemas cache
}

// DB transaction
type DBTX struct {
	kv KVTX
	db *DB
}

// begin a transaction
func (db *DB) Begin(tx *DBTX) {
	tx.db = db
	db.kv.Begin(&tx.kv)
}

// end a transaction: commit updates, rollback on error
func (db *DB) Commit(tx *DBTX) error {
	return db.kv.Commit(&tx.kv)
}

// end a transaction: rollback
func (db *DB) Abort(tx *DBTX) {
	db.kv.Abort(&tx.kv)
}

// save state before executing statement
func (tx *DBTX) Save(saved *TXSaved) {
	tx.kv.Save(saved)
}

// revert any updates by the statement
func (tx *DBTX) Revert(saved *TXSaved) {
	tx.kv.Revert(saved)
}

// table schema
type TableDef struct {
	// === user-defined ===

	Name    string     // table name
	Types   []uint32   // column types
	Cols    []string   // column names
	Indexes [][]string // 1st index is primary key

	// === auto-assigned ===

	Prefixes []uint32 // key prefix for indexes
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

// string representation of Value
func (v *Value) Display() string {
	switch v.Type {
	case TYPE_BYTES:
		return string(v.Str)
	case TYPE_INT64:
		return strconv.FormatInt(v.I64, 10)
	default:
		panic("unreachable")
	}
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

// extract multiple column values
func getValues(tdef *TableDef, rec Record, cols []string) ([]Value, error) {
	vals := make([]Value, len(cols))
	for i, c := range cols {
		v := rec.Get(c)
		if v == nil {
			return nil, fmt.Errorf("missing column: %s", c)
		}
		if v.Type != tdef.Types[slices.Index(tdef.Cols, c)] {
			return nil, fmt.Errorf("bad column type: %s", c)
		}
		vals[i] = *v
	}
	return vals, nil
}

// return non-PK columns in defined order
func nonPrimaryKeyCols(tdef *TableDef) (out []string) {
	for _, c := range tdef.Cols {
		if slices.Index(tdef.Indexes[0], c) < 0 {
			out = append(out, c)
		}
	}
	return
}

/* ORDER-PRESERVING ENCODING:
- To support range queries, serialized keys must be compared
  with respect to their data types
  . 1 way is to replace bytes.Compare() with a callback that decodes
    and compares keys according to table schema. -> SLOW
  . another way is to choose a special serialization format so that
    the resulting bytes reflects the sort order.

=== Numbers:
- For unsigned:
  . Put the higher bits first -> Big-Endian
- For signed (2's-comp):
  . Map positive values to upper half of the unsigned range,
    negative values to lower half of the unsigned range
    -> flip the MSB

=== Strings:
- Key can consists of multiple columns.
  But simply concatenating creates ambiguity.
  Example: ("a", "bc") vs. ("ab", "c")
- There are 2 ways to encode strings with lengths
  . Prepend the length -> DESTROY sort order
  . Put a delimiter at the end (the NULL byte).
    Above example (encoded): "a\x00bc\x00", "ab\x00c\x00".
- With delimiter approach, the input cannot contain the delimiter
  -> escape the delimiter
  Use 0x01 as escaping byte, which must be escaped itself:
  . 00 -> 01 01; 01 -> 01 02
  . Note that the escape sequences still preserved sort order.

=== Missing columns as infinity:
- Example: query with index (a, b)
  . a > 1	<-> (a, b) > (1, +inf)
  . a <= 1	<-> (a, b) < (1, +inf)
  . a >= 1	<-> (a, b) > (1, -inf)
  . a < 1	<-> (a, b) < (1, -inf)
- Example: query with index (a, b, c)
  . a > 1	<-> (a, b, c) > (1, +inf, +inf)
  . a <= 1	<-> (a, b, c) < (1, +inf, +inf)
  . a >= 1	<-> (a, b, c) > (1, -inf, -inf)
  . a < 1	<-> (a, b, c) < (1, -inf, -inf)
  . a = 1 AND b > 2		<-> (a, b, c) > (1, 2, +inf)
  . a = 1 AND b <= 2	<-> (a, b, c) < (1, 2, +inf)
  . a = 1 AND b >= 2	<-> (a, b, c) > (1, 2, -inf)
  . a = 1 AND b < 2		<-> (a, b, c) < (1, 2, -inf)
  . a > 1 AND ..., a >= 1 AND ..., a < 1 AND ..., a <= 1 AND ...
    -> use the same bounds as the 1st 4 cases
- Choose "\xff" as +inf, "" as -inf
  . -inf case: ignore missing column since no columns are encoded as ""
  . +inf case: prepend a tag to each encoded column so they don't start with "\xff"
*/

// order-preserving encoding
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		out = append(out, byte(v.Type)) // ensure not start with 0xff

		switch v.Type {
		case TYPE_INT64:
			var buf [8]byte
			u := uint64(v.I64) ^ (1 << 63)        // flip the sign bit
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

// for primary key and secondary indexes
func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	// table prefix
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], prefix)
	out = append(out, buf[:]...)

	// order-preserving encoded key
	out = encodeValues(out, vals)
	return out
}

// query key can be a prefix of index key
func encodeKeyPartial(
	out []byte, prefix uint32, vals []Value, cmp int,
) []byte {
	out = encodeKey(out, prefix, vals)

	// encode missing columns as infinity
	if cmp == CMP_GT || cmp == CMP_LE {
		out = append(out, 0xff) // +inf
	} // else: -inf (empty string)

	return out
}

// escape the null byte (0); use 0x01 as escaping byte
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
		// skip prepended byte (value type)
		assert(out[i].Type == uint32(in[0]))
		in = in[1:]

		switch out[i].Type {
		case TYPE_INT64:
			u := binary.BigEndian.Uint64(in[:8])
			out[i].I64 = int64(u ^ (1 << 63)) // flip the MSB back
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

func decodeKey(in []byte, out []Value) {
	decodeValues(in[4:], out) // skip table prefix
}

// internal table: metadata
// - auto-incrementing counter for generating table prefixes.
// - ...
var TDEF_META = &TableDef{
	Name:     "@meta",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"key", "val"},
	Indexes:  [][]string{{"key"}},
	Prefixes: []uint32{1},
}

// internal table: table schemas
var TDEF_TABLE = &TableDef{
	Name:     "@table",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"name", "def"},
	Indexes:  [][]string{{"name"}},
	Prefixes: []uint32{2},
}

var INTERNAL_TABLES map[string]*TableDef = map[string]*TableDef{
	TDEF_META.Name:  TDEF_META,
	TDEF_TABLE.Name: TDEF_TABLE,
}

// get table schema by name
func getTableDef(tx *DBTX, name string) *TableDef {
	// check internal tables
	if tdef, ok := INTERNAL_TABLES[name]; ok {
		return tdef
	}

	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()

	// check schemas cache
	if tdef := tx.db.tables[name]; tdef != nil {
		return tdef
	}

	// query schemas table
	tdef := getTableDefDB(tx, name)
	if tdef != nil { // cache if found
		tx.db.tables[name] = tdef
	}

	return tdef

}

// get table schema from internal table '@table'
func getTableDefDB(tx *DBTX, name string) *TableDef {
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := dbGet(tx, TDEF_TABLE, rec)
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
func dbGet(tx *DBTX, tdef *TableDef, rec *Record) (bool, error) {
	// extract primary key
	values, err := getValues(tdef, *rec, tdef.Indexes[0])
	if err != nil {
		return false, err
	}

	keyRec := Record{Cols: tdef.Indexes[0], Vals: values}
	sc := Scanner{
		Cmp1: CMP_GE,
		Cmp2: CMP_LE,
		Key1: keyRec,
		Key2: keyRec,
	}
	if err := dbScan(tx, tdef, &sc); err != nil || !sc.Valid() {
		return false, err
	}
	sc.Deref(rec)
	return true, nil
}

// get a row by primary key
func (tx *DBTX) Get(table string, rec *Record) (bool, error) {
	tdef := getTableDef(tx, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbGet(tx, tdef, rec)
}

const TABLE_PREFIX_MIN = 100

// verify table schema;
// verify indexes & add PK to secondary indexes
func tableDefChecK(tdef *TableDef) error {
	// verify table schema
	bad := tdef.Name == "" || len(tdef.Cols) == 0 || len(tdef.Indexes) == 0
	bad = bad || len(tdef.Cols) != len(tdef.Types)
	if bad {
		return fmt.Errorf("bad table schema: %s", tdef.Name)
	}

	// verify indexes & add PK to secondary indexes
	for i, index := range tdef.Indexes {
		index, err := checkIndexCols(tdef, index)
		if err != nil {
			return err
		}
		tdef.Indexes[i] = index
	}

	return nil
}

/* Secondary index:
- Are extra KV pairs containing the PK in the B+tree.
  Each index has its own prefix.
- Doesn't have unique constraint -> can generate duplicate keys.
- Instead of modifying B+tree to support duplicates,
  add the PK (only columns not already in key) to the key
  to make it unique, and leave the value empty.
*/

// verify index & add PK to secondary index
func checkIndexCols(tdef *TableDef, index []string) ([]string, error) {
	if len(index) == 0 {
		return nil, fmt.Errorf("empty index")
	}

	seen := map[string]bool{}
	for _, c := range index {
		if slices.Index(tdef.Cols, c) < 0 {
			return nil, fmt.Errorf("unknown index column: %s", c)
		}
		if seen[c] {
			return nil, fmt.Errorf("duplicate column in index: %s", c)
		}
		seen[c] = true
	}

	// add PK to secondary index
	for _, c := range tdef.Indexes[0] {
		if !seen[c] {
			index = append(index, c)
		}
	}

	assert(len(index) <= len(tdef.Cols))
	return index, nil
}

// create table
func (tx *DBTX) TableNew(tdef *TableDef) error {
	// verify table schema & sanitize indexes
	if err := tableDefChecK(tdef); err != nil {
		return err
	}

	// check for existing table
	tableRec := (&Record{}).AddStr("name", []byte(tdef.Name))
	ok, err := dbGet(tx, TDEF_TABLE, tableRec)
	assert(err == nil)
	if ok {
		return fmt.Errorf("table exists: %s", tdef.Name)
	}

	// get current prefix counter
	var prefix uint32
	metaRec := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err = dbGet(tx, TDEF_META, metaRec)
	assert(err == nil)
	if ok {
		prefix = binary.LittleEndian.Uint32(metaRec.Get("val").Str)
		assert(prefix > TABLE_PREFIX_MIN)
	} else {
		prefix = uint32(TABLE_PREFIX_MIN)
		metaRec.AddStr("val", make([]byte, 4)) // prepare to save
	}

	// allocate prefixes
	assert(len(tdef.Prefixes) == 0)
	for i := range tdef.Indexes {
		tdef.Prefixes = append(tdef.Prefixes, prefix+uint32(i))
	}

	// set next prefix
	nextPrefix := prefix + uint32(len(tdef.Indexes))
	binary.LittleEndian.PutUint32(metaRec.Get("val").Str, nextPrefix)
	_, err = dbUpdate(tx, TDEF_META, &DBUpdateReq{Record: *metaRec})
	if err != nil {
		return err
	}

	// store the schema
	jsonTDef, err := json.Marshal(tdef)
	assert(err == nil)
	tableRec.AddStr("def", jsonTDef)
	_, err = dbUpdate(tx, TDEF_TABLE, &DBUpdateReq{Record: *tableRec})
	return err
}

type DBUpdateReq struct {
	// === in ===

	Record Record
	Mode   int

	// === out ===

	Updated bool // inserted/updated
	Added   bool // inserted
}

// insert/update a row
func dbUpdate(
	tx *DBTX, tdef *TableDef, dbReq *DBUpdateReq,
) (bool, error) {
	// reorder columns to start with the primary key
	cols := slices.Concat(tdef.Indexes[0], nonPrimaryKeyCols(tdef))
	values, err := getValues(tdef, dbReq.Record, cols)
	if err != nil {
		return false, err
	}

	// insert/update the row
	npk := len(tdef.Indexes[0]) // number of primary key columns
	key := encodeKey(nil, tdef.Prefixes[0], values[:npk])
	val := encodeValues(nil, values[npk:])
	req := UpdateReq{Key: key, Val: val, Mode: dbReq.Mode}
	if _, err = tx.kv.Update(&req); err != nil {
		return false, err
	}
	dbReq.Added, dbReq.Updated = req.Added, req.Updated

	// maintain secondary indexes
	if req.Updated && !req.Added {
		// construct old record
		decodeValues(req.Old, values[npk:])
		oldRec := Record{Cols: cols, Vals: values}
		// delete old index keys
		if err = indexOp(tx, tdef, INDEX_DEL, oldRec); err != nil {
			return false, err
		}
	}
	if req.Updated {
		// add new index keys
		if err = indexOp(tx, tdef, INDEX_ADD, dbReq.Record); err != nil {
			return false, err
		}
	}

	return req.Updated, nil
}

const (
	INDEX_ADD = 1
	INDEX_DEL = 2
)

// add or remove secondary index keys
func indexOp(tx *DBTX, tdef *TableDef, op int, rec Record) error {
	for i := 1; i < len(tdef.Indexes); i++ {
		// index key
		values, err := getValues(tdef, rec, tdef.Indexes[i])
		assert(err == nil) // 'rec' is full record
		key := encodeKey(nil, tdef.Prefixes[i], values)

		switch op {
		case INDEX_ADD:
			req := UpdateReq{Key: key, Val: nil}
			_, err = tx.kv.Update(&req)
			assert(err != nil || req.Added) // internal consistency
		case INDEX_DEL:
			deleted := false
			deleted, err = tx.kv.Del(&DeleteReq{Key: key})
			assert(err != nil || deleted) // internal consistency
		default:
			panic("unreachable")
		}

		if err != nil {
			return err
		}
	}
	return nil
}

// insert/update a row
func (tx *DBTX) Set(table string, dbReq *DBUpdateReq) (bool, error) {
	tdef := getTableDef(tx, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbUpdate(tx, tdef, dbReq)
}

func (tx *DBTX) Insert(table string, rec Record) (bool, error) {
	return tx.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_INSERT_ONLY,
	})
}

func (tx *DBTX) Update(table string, rec Record) (bool, error) {
	return tx.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_UPDATE_ONLY,
	})
}

func (tx *DBTX) Upsert(table string, rec Record) (bool, error) {
	return tx.Set(table, &DBUpdateReq{
		Record: rec,
		Mode:   MODE_UPSERT,
	})
}

// delete a row by primary key
func dbDelete(tx *DBTX, tdef *TableDef, rec Record) (bool, error) {
	// extract primary key
	values, err := getValues(tdef, rec, tdef.Indexes[0])
	if err != nil {
		return false, nil
	}
	key := encodeKey(nil, tdef.Prefixes[0], values)

	// delete the row
	req := DeleteReq{Key: key}
	if deleted, err := tx.kv.Del(&req); !deleted {
		return false, err
	}

	// === maintain secondary indexes ===

	// construct old record
	for _, c := range nonPrimaryKeyCols(tdef) {
		type_ := tdef.Types[slices.Index(tdef.Cols, c)]
		values = append(values, Value{Type: type_})
	}
	npk := len(tdef.Indexes[0]) // number of primary key columns
	decodeValues(req.Old, values[npk:])
	old := Record{tdef.Cols, values}

	// delete old index keys
	if err = indexOp(tx, tdef, INDEX_DEL, old); err != nil {
		return false, err
	}

	return true, nil
}

// delete a row by primary key
func (tx *DBTX) Delete(table string, rec Record) (bool, error) {
	tdef := getTableDef(tx, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}
	return dbDelete(tx, tdef, rec)
}

func (db *DB) Open() error {
	db.kv.Path = db.Path
	db.tables = map[string]*TableDef{}
	return db.kv.Open()
}

func (db *DB) Close() {
	db.kv.Close()
}

// iterator for range queries
type Scanner struct {
	// === range ===

	Cmp1 int // CMP_xx
	Cmp2 int // CMP_xx
	Key1 Record
	Key2 Record

	// === internal ===

	tx    *DBTX
	tdef  *TableDef
	index int    // which index?
	iter  KVIter // underlying KV iterator
}

// currently within range?
func (sc *Scanner) Valid() bool {
	return sc.iter.Valid()
}

// move the underlying KV iterator
func (sc *Scanner) Next() {
	sc.iter.Next()
}

// return current row
func (sc *Scanner) Deref(rec *Record) {
	assert(sc.Valid())
	tdef := sc.tdef

	// prepare output record
	// (reorder columns to start with the primary key)
	rec.Cols = slices.Concat(tdef.Indexes[0], nonPrimaryKeyCols(tdef))
	rec.Vals = rec.Vals[:0]
	for _, c := range rec.Cols {
		type_ := tdef.Types[slices.Index(tdef.Cols, c)]
		rec.Vals = append(rec.Vals, Value{Type: type_})
	}

	// fetch KV
	key, val := sc.iter.Deref()

	if sc.index == 0 { // primary key
		// decode the full row
		npk := len(tdef.Indexes[0]) // number of PK columns
		decodeKey(key, rec.Vals[:npk])
		decodeValues(val, rec.Vals[npk:])
	} else { // secondary index
		// decode the index key
		assert(len(val) == 0)
		index := tdef.Indexes[sc.index]
		idxRec := Record{Cols: index, Vals: make([]Value, len(index))}
		for i, c := range index {
			idxRec.Vals[i].Type = tdef.Types[slices.Index(tdef.Cols, c)]
		}
		decodeKey(key, idxRec.Vals)

		// extract the primary key
		for i, c := range tdef.Indexes[0] {
			rec.Vals[i] = *idxRec.Get(c)
		}

		// fetch the row by primary key
		ok, err := dbGet(sc.tx, tdef, rec)
		assert(ok && err == nil) // internal consistency
	}
}

// range query
func dbScan(tx *DBTX, tdef *TableDef, req *Scanner) error {
	// verify range
	switch {
	case req.Cmp1 > 0 && req.Cmp2 < 0: // scan forward
	case req.Cmp2 > 0 && req.Cmp1 < 0: // scan backward
	default:
		return fmt.Errorf("bad range")
	}
	// !(key1 Cmp2 key2) -> scan 0 rows

	// verify boundary keys
	if err := checkTypes(tdef, req.Key1); err != nil {
		return err
	}
	if err := checkTypes(tdef, req.Key2); err != nil {
		return err
	}

	req.tx = tx
	req.tdef = tdef

	// key1's columns and key2's columns can be different
	// -> select the 1st index that covers both
	covered := func(key []string, index []string) bool {
		return len(index) >= len(key) && slices.Equal(index[:len(key)], key)
	}
	req.index = slices.IndexFunc(tdef.Indexes, func(index []string) bool {
		return covered(req.Key1.Cols, index) && covered(req.Key2.Cols, index)
	})
	if req.index < 0 {
		return fmt.Errorf("no index")
	}

	// encode start/end key
	prefix := tdef.Prefixes[req.index]
	keyStart := encodeKeyPartial(nil, prefix, req.Key1.Vals, req.Cmp1)
	keyEnd := encodeKeyPartial(nil, prefix, req.Key2.Vals, req.Cmp2)

	// seek to start key
	req.iter = tx.kv.Seek(keyStart, req.Cmp1, keyEnd, req.Cmp2)
	return nil
}

// check column existence and type
func checkTypes(tdef *TableDef, rec Record) error {
	if len(rec.Cols) != len(rec.Vals) {
		return fmt.Errorf("bad record")
	}
	for i, c := range rec.Cols {
		j := slices.Index(tdef.Cols, c)
		if j < 0 || tdef.Types[j] != rec.Vals[i].Type {
			return fmt.Errorf("bad column: %s", c)
		}
	}
	return nil
}

// range query
func (tx *DBTX) Scan(table string, req *Scanner) error {
	tdef := getTableDef(tx, table)
	if tdef == nil {
		return fmt.Errorf("table not found: %s", table)
	}
	return dbScan(tx, tdef, req)
}
