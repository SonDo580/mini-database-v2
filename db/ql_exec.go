package db

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// context for evaluating expression
type QLEvalCtx struct {
	env Record // optional row
	out Value
	err error // evaluation error
}

// evaluate an expression
func (ctx *QLEvalCtx) evalExpr(node QLNode) {
	if ctx.err != nil {
		return
	}

	switch node.Type {
	case QL_SYM: // symbol (column name)
		if v := ctx.env.Get(string(node.Str)); v != nil {
			ctx.out = *v
		} else {
			ctx.eErr("unknown column: %s", node.Str)
		}
	case QL_I64, QL_STR: // literal (int64 or string)
		ctx.out = Value{Type: node.Type, I64: node.I64, Str: node.Str}
	case QL_TUP: // tuple -> not evaluate directly
		ctx.eErr("unexpected tuple")

	// unary ops
	case QL_NEG:
		ctx.evalExpr(node.Kids[0])
		if ctx.out.Type == TYPE_INT64 {
			ctx.out.I64 = -ctx.out.I64
		} else {
			ctx.eErr("QL_NEG type error")
		}
	case QL_NOT:
		ctx.evalExpr(node.Kids[0])
		if ctx.out.Type == TYPE_INT64 {
			// 0 -> false -> 0;
			// other integers -> true -> 1
			ctx.out.I64 = b2i(ctx.out.I64 == 0)
		} else {
			ctx.eErr("QL_NOT type error")
		}

	// binary ops
	case QL_CMP_EQ, QL_CMP_NE, QL_CMP_GE, QL_CMP_LE, QL_CMP_GT, QL_CMP_LT, // comparison
		QL_ADD, QL_SUB, QL_MUL, QL_DIV, QL_MOD, // arithmetic
		QL_AND, QL_OR: // logic
		ctx.evalBinOp(node)

	default:
		panic("unreachable")
	}

}

// report expr evaluation error; skip if already has error
func (ctx *QLEvalCtx) eErr(format string, args ...interface{}) {
	if ctx.err == nil {
		ctx.out.Type = QL_ERR
		ctx.err = fmt.Errorf(format, args...)
	}
}

// false -> 0; true -> 1
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// comparison result -> boolean
func cmp2bool(result int, cmp uint32) bool {
	switch cmp {
	case QL_CMP_EQ:
		return result == 0
	case QL_CMP_NE:
		return result != 0
	case QL_CMP_GE:
		return result >= 0
	case QL_CMP_GT:
		return result > 0
	case QL_CMP_LE:
		return result <= 0
	case QL_CMP_LT:
		return result < 0
	default:
		panic("unreachable")
	}
}

// evaluate binary operation
func (ctx *QLEvalCtx) evalBinOp(node QLNode) {
	// logic with short-circuit
	switch node.Type {
	case QL_AND, QL_OR:
		ctx.evalLogic(node)
		return
	}

	isCmp := false
	switch node.Type {
	case QL_CMP_EQ, QL_CMP_NE, QL_CMP_GE, QL_CMP_LE, QL_CMP_GT, QL_CMP_LT:
		isCmp = true
	}

	// tuple comparison
	if isCmp && node.Kids[0].Type == QL_TUP && node.Kids[1].Type == QL_TUP {
		r := ctx.tupleCmp(node.Kids[0], node.Kids[1])
		ctx.out.Type = TYPE_INT64
		ctx.out.I64 = b2i(cmp2bool(r, node.Type))
		return
	}

	// evaluate sub-expressions
	ctx.evalExpr(node.Kids[0])
	val1 := ctx.out
	ctx.evalExpr(node.Kids[1])
	val2 := ctx.out

	// scalar comparison
	if isCmp {
		r := ctx.valueCmp(val1, val2)
		ctx.out.Type = TYPE_INT64
		ctx.out.I64 = b2i(cmp2bool(r, node.Type))
		return
	}

	switch {
	case ctx.err != nil:
		return
	case val1.Type != val2.Type:
		ctx.eErr("binop type mismatch")
	case val1.Type == TYPE_INT64: // i64 arithmetic
		ctx.out.Type = TYPE_INT64
		ctx.out.I64 = ctx.binOpI64(node.Type, val1.I64, val2.I64)
	case val1.Type == TYPE_BYTES: // str
		ctx.out.Type = TYPE_BYTES
		ctx.out.Str = ctx.binOpStr(node.Type, val1.Str, val2.Str)
	default:
		panic("unreachable")
	}
}

// logic operation with short-circuit
func (ctx *QLEvalCtx) evalLogic(node QLNode) {
	// 1st operand
	ctx.evalExpr(node.Kids[0])
	if ctx.out.Type != TYPE_INT64 {
		ctx.eErr("invalid AND operand type")
	}
	if ctx.err != nil {
		return
	}

	// short-circuit:
	// - AND: stop if 1st operand is falsy
	// - OR: stop if 1st operand is truthy
	if node.Type == QL_AND && ctx.out.I64 == 0 {
		return
	}
	if node.Type == QL_OR && ctx.out.I64 != 0 {
		ctx.out.I64 = 1
		return
	}

	// 2nd operand
	ctx.evalExpr(node.Kids[1])
	if ctx.out.Type != TYPE_INT64 {
		ctx.eErr("invalid AND operand type")
	}
	if ctx.err != nil {
		return
	}

	ctx.out.I64 = b2i(ctx.out.I64 != 0)
}

func (ctx *QLEvalCtx) valueCmp(val1, val2 Value) int {
	switch {
	case ctx.err != nil:
		return 0
	case val1.Type != val2.Type:
		ctx.eErr("compare different types")
		return 0
	case val1.Type == TYPE_INT64:
		return cmp.Compare(val1.I64, val2.I64)
	case val1.Type == TYPE_BYTES:
		return bytes.Compare(val1.Str, val2.Str)
	default:
		panic("unreachable")
	}
}

func (ctx *QLEvalCtx) tupleCmp(n1, n2 QLNode) int {
	if len(n1.Kids) != len(n2.Kids) {
		ctx.eErr("compare tuples of different lengths")
		return 0
	}

	for i := 0; i < len(n1.Kids) && ctx.err == nil; i++ {
		ctx.evalExpr(n1.Kids[i])
		val1 := ctx.out
		ctx.evalExpr(n2.Kids[i])
		val2 := ctx.out
		if cmp := ctx.valueCmp(val1, val2); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// i64 arithmetic
func (ctx *QLEvalCtx) binOpI64(op uint32, i1, i2 int64) int64 {
	switch op {
	case QL_ADD:
		return i1 + i2
	case QL_SUB:
		return i1 - i2
	case QL_MUL:
		return i1 * i2
	case QL_DIV: // integer division
		if i2 == 0 {
			ctx.eErr("division by 0")
			return 0
		}
		return i1 / i2
	case QL_MOD:
		if i2 == 0 {
			ctx.eErr("division by 0")
			return 0
		}
		return i1 % i2
	default:
		ctx.eErr("bad i64 arithmetic binop")
		return 0
	}
}

func (ctx *QLEvalCtx) binOpStr(op uint32, s1, s2 []byte) []byte {
	switch op {
	case QL_ADD: // concatenate
		return slices.Concat(s1, s2)
	default:
		ctx.eErr("bad str binop")
		return nil
	}
}

type QLResult struct {
	Records RecordIter // select result iterator
	Added   uint64     // inserted
	Updated uint64     // inserted/updated
	Deleted uint64     // deleted
}

// execute a single statement
func (tx *DBTX) execStmt(stmt interface{}) (result QLResult, err error) {
	saved := TXSaved{}
	tx.Save(&saved) // save state before executing

	switch req := stmt.(type) {
	case *QLCreateTable:
		err = tx.execCreateTable(req)
	case *QLSelect:
		result.Records, err = tx.execSelect(req)
	case *QLInsert:
		result.Added, result.Updated, err = tx.execInsert(req)
	case *QLUpdate:
		result.Updated, err = tx.execUpdate(req)
	case *QLDelete:
		result.Deleted, err = tx.execDelete(req)
	default:
		panic("unreachable")
	}

	if err != nil {
		tx.Revert(&saved) // revert any partial updates
	}
	return
}

func (tx *DBTX) execCreateTable(req *QLCreateTable) error {
	return tx.TableNew(&req.Def)
}

type RecordIter interface {
	Valid() bool
	Next()
	Deref(*Record) error
}

// iterator for condition (index by ... filter ... limit ...);
// implement RecordIter
type qlScanIter struct {
	// === input ===

	req *QLScan
	sc  Scanner

	// === state ====

	count int64 // number of matched rows iterated (after 'offset')
	end   bool  // 'count' reached 'limit' OR out of Scanner's range?

	// === cached output item ===

	rec Record // current row
	err error  // error retrieving current row
}

// execute condition (index by ... filter ... limit ...)
func (tx *DBTX) execScan(req *QLScan) (RecordIter, error) {
	iter := qlScanIter{req: req}
	if err := iter.initScanner(); err != nil {
		return nil, err
	}
	if err := tx.Scan(req.Table, &iter.sc); err != nil {
		return nil, err
	}
	iter.Next() // trigger retrieving the 1st valid row
	return &iter, nil
}

// init Scanner from 'index by' clause
func (iter *qlScanIter) initScanner() (err error) {
	req := iter.req
	sc := &iter.sc

	// evaluate scan keys
	if sc.Key1, sc.Cmp1, err = evalScanKey(req.Key1); err != nil {
		return err
	}
	if sc.Key2, sc.Cmp2, err = evalScanKey(req.Key2); err != nil {
		return err
	}

	// calculate Scanner's range
	switch {
	case req.Key1.Type == 0 && req.Key2.Type == 0:
		// no 'index by' -> full table scan with primary key
		sc.Cmp1, sc.Cmp2 = CMP_GE, CMP_LE // range: -inf <= key <= +inf
	case req.Key1.Type == QL_CMP_EQ && req.Key2.Type == 0:
		// equal by a prefix: INDEX BY cols = vals
		sc.Key2 = sc.Key1
		sc.Cmp1, sc.Cmp2 = CMP_GE, CMP_LE // range: key = key1
	case req.Key1.Type != 0 && req.Key2.Type == 0:
		// open-ended range: INDEX BY cols <cmp> vals
		if sc.Cmp1 > 0 { // >=, >
			sc.Cmp2 = CMP_LE // range: key1 <= key <= +inf
		} else { // <=, <
			sc.Cmp2 = CMP_GE // range: key1 >= key >= -inf (descending)
		}
	case req.Key1.Type != 0 && req.Key2.Type != 0:
		// 2-side range: INDEX BY cols1 <cmp1> vals1 AND cols2 <cmp2> vals2
	default:
		panic("unreachable")
	}

	return nil
}

// convert 'index by' key to Record and CMP_xx
func evalScanKey(node QLNode) (Record, int, error) {
	var cmp int
	switch node.Type {
	case 0: // key not exist; handle in initScanner()
		return Record{}, 0, nil
	case QL_CMP_EQ:
		cmp = 0 // handle in initScanner()
	case QL_CMP_GE:
		cmp = CMP_GE
	case QL_CMP_GT:
		cmp = CMP_GT
	case QL_CMP_LE:
		cmp = CMP_LE
	case QL_CMP_LT:
		cmp = CMP_LT
	default:
		panic("unreachable")
	}

	names, exprs := node.Kids[0], node.Kids[1]
	assert(names.Type == QL_TUP && exprs.Type == QL_TUP)
	assert(len(names.Kids) == len(exprs.Kids))

	vals, err := evalMulti(Record{}, exprs.Kids)
	if err != nil {
		return Record{}, 0, err
	}

	cols := []string{}
	for _, name := range names.Kids {
		assert(name.Type == QL_SYM)
		cols = append(cols, string(name.Str))
	}

	return Record{Cols: cols, Vals: vals}, cmp, nil
}

// evaluate multiple expressions
func evalMulti(env Record, exprs []QLNode) (vals []Value, err error) {
	for _, expr := range exprs {
		ctx := QLEvalCtx{env: env}
		ctx.evalExpr(expr)
		if ctx.err != nil {
			return nil, ctx.err
		}
		vals = append(vals, ctx.out)
	}
	return vals, nil
}

// move until encounter a valid row or error (may be at current);
// stop if:
// - moved out of Scanner's range.
// - OR number of matched items reached 'limit'
func (iter *qlScanIter) Next() {
	for iter.sc.Valid() {
		// get current row and evaluate 'filter'
		matched, err := iter.pull()
		if err != nil {
			iter.err = err
			return
		}

		// move to next row
		iter.sc.Next()

		if matched { // row satisfies 'filter'
			iter.count++

			// skip 'offset' matched rows
			if iter.count <= iter.req.Offset {
				continue
			}

			// stop if number of matched rows reached 'limit'
			if iter.count == iter.req.Limit {
				break
			}

			return // current row is valid
		}
	}

	iter.end = true
}

// retrieve current row; return True if it satisfies 'filter'
func (iter *qlScanIter) pull() (bool, error) {
	iter.sc.Deref(&iter.rec)

	if iter.req.Filter.Type != 0 {
		ctx := QLEvalCtx{env: iter.rec}
		ctx.evalExpr(iter.req.Filter)
		if ctx.err != nil {
			return false, ctx.err
		}
		if ctx.out.Type != TYPE_INT64 {
			return false, errors.New("filter is not boolean")
		}
		if ctx.out.I64 == 0 {
			return false, nil
		}
	}
	return true, nil
}

// still in range?
func (iter *qlScanIter) Valid() bool {
	return !iter.end
}

// get cached item or pulling error
func (iter *qlScanIter) Deref(rec *Record) error {
	assert(iter.Valid())
	if iter.err != nil {
		*rec = iter.rec
	}
	return iter.err
}

// iterator for select stmt; implement RecordIter
type qlSelectIter struct {
	iter  RecordIter // iterator for condition
	names []string   // aliases
	exprs []QLNode   // select expressions
}

func (iter *qlSelectIter) Valid() bool {
	return iter.iter.Valid()
}

func (iter *qlSelectIter) Next() {
	iter.iter.Next()
}

func (iter *qlSelectIter) Deref(rec *Record) error {
	if err := iter.iter.Deref(rec); err != nil {
		return err
	}

	vals, err := evalMulti(*rec, iter.exprs)
	if err != nil {
		return err
	}

	*rec = Record{Cols: iter.names, Vals: vals}
	return nil
}

func (tx *DBTX) execSelect(req *QLSelect) (RecordIter, error) {
	// iterator for records that satisfy condition
	records, err := tx.execScan(&req.QLScan)
	if err != nil {
		return nil, err
	}

	tdef := getTableDef(tx, req.Table)
	names, exprs := []string{}, []QLNode{}
	for i := range req.Names {
		if req.Names[i] != "*" {
			names = append(names, req.Names[i])
			exprs = append(exprs, req.Output[i])
		} else { // expand "*" into table's columns
			names = append(names, tdef.Cols...)
			for _, col := range tdef.Cols {
				node := QLNode{Type: QL_SYM, Str: []byte(col)}
				exprs = append(exprs, node)
			}
		}
	}
	assert(len(names) == len(exprs))

	// assign column names if missing
	for i := range names {
		if names[i] != "" {
			continue
		}

		// TODO: string representation for each expr type
		if exprs[i].Type == QL_SYM { // column
			names[i] = string(exprs[i].Str)
		} else {
			names[i] = strconv.Itoa(i)
		}
	}

	return &qlSelectIter{
		iter: records, names: names, exprs: exprs,
	}, nil
}

func (tx *DBTX) execInsert(req *QLInsert) (uint64, uint64, error) {
	addedCount, updatedCount := uint64(0), uint64(0)

	for _, exprs := range req.Values {
		// evaluate values
		vals, err := evalMulti(Record{}, exprs)
		if err != nil {
			return 0, 0, err
		}

		// perform updates
		dbReq := DBUpdateReq{
			Record: Record{Cols: req.Names, Vals: vals},
			Mode:   req.Mode,
		}
		_, err = tx.Set(req.Table, &dbReq)
		if err != nil {
			return 0, 0, err
		}

		// stats
		if dbReq.Added {
			addedCount++
		}
		if dbReq.Updated {
			updatedCount++
		}
	}

	return addedCount, updatedCount, nil
}

func (tx *DBTX) execUpdate(req *QLUpdate) (uint64, error) {
	assert(len(req.Names) == len(req.Values))

	// - check if column exists
	// - don't allow updating primary key
	tdef := getTableDef(tx, req.Table)
	for _, col := range req.Names {
		if slices.Index(tdef.Cols, col) < 0 {
			return 0, fmt.Errorf("unknown column: %s", col)
		}
		if slices.Index(tdef.Indexes[0], col) >= 0 {
			return 0, fmt.Errorf("cannot update primary key")
		}
	}

	// iterator for records that satisfy condition
	records, err := tx.execScan(&req.QLScan)
	if err != nil {
		return 0, err
	}

	updatedCount := uint64(0)
	for ; records.Valid(); records.Next() {
		// current record
		rec := Record{}
		if err := records.Deref(&rec); err != nil {
			return 0, err
		}

		// update values of columns
		vals, err := evalMulti(rec, req.Values)
		if err != nil {
			return 0, err
		}
		for i, col := range req.Names {
			rec.Vals[slices.Index(tdef.Cols, col)] = vals[i]
		}

		// perform update
		dbReq := DBUpdateReq{Record: rec, Mode: MODE_UPDATE_ONLY}
		updated, err := tx.Set(req.Table, &dbReq)
		if err != nil {
			return 0, err
		}

		assert(updated && dbReq.Updated) // update existing row
		updatedCount++
	}

	return updatedCount, nil
}

func (tx *DBTX) execDelete(req *QLDelete) (uint64, error) {
	// iterator for records that satisfy condition
	records, err := tx.execScan(&req.QLScan)
	if err != nil {
		return 0, err
	}

	deletedCount := uint64(0)
	tdef := getTableDef(tx, req.Table)
	for ; records.Valid(); records.Next() {
		rec := Record{}
		if err := records.Deref(&rec); err != nil {
			return 0, err
		}

		// delete by primary key
		vals, err := getValues(tdef, rec, tdef.Indexes[0])
		if err != nil {
			return 0, err
		}
		deleted, err := tx.Delete(
			req.Table, Record{Cols: tdef.Indexes[0], Vals: vals},
		)
		if err != nil {
			return 0, err
		}

		assert(deleted) // delete existing row
		deletedCount++
	}

	return deletedCount, nil
}

// parse and execute a single QL statement
func (tx *DBTX) ExecStr(stmtStr []byte) (QLResult, error) {
	p := Parser{input: stmtStr}
	stmt, err := p.Parse()
	if err != nil {
		return QLResult{}, err
	}
	return tx.execStmt(stmt)
}
