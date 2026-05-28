package db

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// expression type
const (
	// literals
	QL_STR = TYPE_BYTES
	QL_I64 = TYPE_INT64

	// binary ops
	QL_CMP_GE = 10
	QL_CMP_GT = 11
	QL_CMP_LT = 12
	QL_CMP_LE = 13
	QL_CMP_EQ = 14
	QL_CMP_NE = 15
	QL_ADD    = 20
	QL_SUB    = 21
	QL_MUL    = 22
	QL_DIV    = 23
	QL_MOD    = 24
	QL_AND    = 30
	QL_OR     = 31

	// unary ops
	QL_NOT = 50
	QL_NEG = 51

	QL_SYM  = 100 // column
	QL_TUP  = 101 // tuple
	QL_STAR = 102 // select *

	QL_ERR = 200 // parsing or evaluation error
)

// expression
type QLNode struct {
	Type uint32
	I64  int64
	Str  []byte
	Kids []QLNode // operands
}

// stmt: create table
type QLCreateTable struct {
	Def TableDef
}

// common structure for queries: INDEX BY, FILTER, LIMIT
type QLScan struct {
	Table  string
	Key1   QLNode // index by
	Key2   QLNode // |
	Filter QLNode // filter
	Offset int64
	Limit  int64
}

// stmt: select
type QLSelect struct {
	QLScan
	Names  []string // expr AS name
	Output []QLNode
}

// stmt: update
type QLUpdate struct {
	QLScan
	Names  []string
	Values []QLNode
}

// stmt: delete
type QLDelete struct {
	QLScan
}

// stmt: insert
type QLInsert struct {
	Table  string
	Mode   int // insert / upsert / replace
	Names  []string
	Values [][]QLNode
}

type Parser struct {
	input []byte
	idx   int
	err   error
}

func isSpace(ch byte) bool {
	return unicode.IsSpace(rune(ch))
}

func (p *Parser) skipSpaces() {
	for p.idx < len(p.input) && isSpace(p.input[p.idx]) {
		p.idx++
	}
}

var keywordSet = map[string]bool{
	"create":  true,
	"table":   true,
	"primary": true,
	"key":     true,
	"insert":  true,
	"replace": true,
	"upsert":  true,
	"into":    true,
	"values":  true,
	"update":  true,
	"set":     true,
	"delete":  true,
	"select":  true,
	"as":      true,
	"from":    true,
	"index":   true,
	"by":      true,
	"filter":  true,
	"limit":   true,
}

// match multiple tokens sequentially (use lowercase for named keyword):
// - return False if not match
// - return True and advance pointer if match
func (p *Parser) match(tokens ...string) bool {
	start := p.idx
	for _, tok := range tokens {
		p.skipSpaces()
		end := p.idx + len(tok)
		if end > len(p.input) {
			p.idx = start
			return false
		}

		// case-insensitive match
		match := strings.EqualFold(string(p.input[p.idx:end]), tok)

		// if token is named keyword,
		// don't match if this is actually a name that
		// starts with the same characters
		if match && keywordSet[tok] && end < len(p.input) {
			match = !isSym(p.input[end])
		}

		if !match {
			p.idx = start
			return false
		}

		p.idx += len(tok)
	}
	return true
}

// report parsing error; skip if already has error
func (p *Parser) pErr(format string, args ...interface{}) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

// match the specified token; report error if not match
func (p *Parser) consume(tok string, format string, args ...interface{}) {
	if !p.match(tok) {
		p.pErr(format, args...)
	}
}

// parse comma-separated list of items
func (p *Parser) pCommaList(pItem func()) {
	pItem()
	for p.match(",") {
		pItem()
	}
}

// parse comma-separated list of items enclosed in parentheses
// ('(' has been matched)
func (p *Parser) pGroupedCommaList(pItem func()) {
	needComma := false
	for p.err == nil && !p.match(")") {
		if needComma {
			p.consume(",", "expect ','")
		}
		pItem()
		needComma = true
	}
}

func (p *Parser) pStmt() (stmt interface{}, err error) {
	switch {
	case p.match("create", "table"):
		stmt = p.pCreateTable()
	case p.match("select"):
		stmt = p.pSelect()
	case p.match("insert", "into"):
		stmt = p.pInsert(MODE_INSERT_ONLY)
	case p.match("replace", "into"):
		stmt = p.pInsert(MODE_UPDATE_ONLY)
	case p.match("upsert", "into"):
		stmt = p.pInsert(MODE_UPSERT)
	case p.match("delete", "from"):
		stmt = p.pDelete()
	case p.match("update"):
		stmt = p.pUpdate()
	default:
		p.pErr("unknown stmt")
	}

	p.consume(";", "expect ';' after statement")

	if p.err != nil {
		return nil, p.err
	}
	return stmt, nil
}

func (p *Parser) pCreateTable() *QLCreateTable {
	// CREATE TABLE <name>
	stmt := QLCreateTable{}
	stmt.Def.Name = p.pSym()
	p.consume("(", "expect '(' after 'create table <name>'")

	// reserve 1st index for primary key
	stmt.Def.Indexes = append(stmt.Def.Indexes, nil)

	p.pGroupedCommaList(func() {
		switch {
		case p.match("index"): // index (c1, c2, ...)
			stmt.Def.Indexes = append(stmt.Def.Indexes, p.pNameList())
		case p.match("primary", "key"): // primary key (c1, c2, ...)
			if stmt.Def.Indexes[0] != nil {
				p.pErr("duplicate primary key")
				return
			} else {
				stmt.Def.Indexes[0] = p.pNameList()
			}
		default: // name type
			stmt.Def.Cols = append(stmt.Def.Cols, p.pSym())
			stmt.Def.Types = append(stmt.Def.Types, p.pColType())
		}
	})

	// note: check bad schema when executing (by DBTX.TableNew())
	return &stmt
}

// parse a comma-separated list of names enclosed in parentheses
func (p *Parser) pNameList() (names []string) {
	p.consume("(", "expect '(' before names list")
	p.pGroupedCommaList(func() {
		names = append(names, p.pSym())
	})
	return names
}

func (p *Parser) pColType() uint32 {
	type_ := p.pSym()
	switch strings.ToLower(type_) {
	case "string", "bytes":
		return TYPE_BYTES
	case "int", "int64":
		return TYPE_INT64
	default:
		p.pErr("bad column type: %s", type_)
		return 0
	}
}

func (p *Parser) pSelect() *QLSelect {
	// SELECT ...
	stmt := QLSelect{}
	p.pCommaList(func() {
		p.pSelectExpr(&stmt)
	})

	// FROM <table>
	p.consume("from", "expect 'from' after 'select ...'")
	stmt.Table = p.pSym()

	// INDEX BY ... FILTER ... LIMIT ...
	p.pScan(&stmt.QLScan)

	return &stmt
}

func (p *Parser) pSelectExpr(stmt *QLSelect) {
	if p.match("*") {
		stmt.Names = append(stmt.Names, "*")
		stmt.Output = append(stmt.Output, QLNode{Type: QL_STAR})
		return
	}

	expr := QLNode{}
	p.pExprOr(&expr)
	stmt.Output = append(stmt.Output, expr)

	name := "" // alias
	if p.match("as") {
		name = p.pSym()
	}
	stmt.Names = append(stmt.Names, name)
}

// parse query condition: INDEX BY, FILTER, LIMIT
func (p *Parser) pScan(node *QLScan) {
	if p.match("index", "by") {
		p.pIndexBy(node)
	}

	if p.match("filter") {
		p.pExprOr(&node.Filter)
	}

	node.Offset, node.Limit = 0, math.MaxInt64
	if p.match("limit") {
		p.pLimit(node)
	}
}

// 2 forms:
// - INDEX BY cols <cmp> vals
// - INDEX BY cols1 <cmp1> vals1 AND cols1 <cmp2> vals1
//
// ===
//   - cmp: comparison operators, except '!=' ('=' can only be use in the 1st form)
//   - cols, vals: must be 2 non-tuple items, or 2 tuples with the same number of items
//   - cols: must contain only symbols (column names)
func (p *Parser) pIndexBy(node *QLScan) {
	index := QLNode{}
	p.pExprAnd(&index)

	if index.Type == QL_AND {
		node.Key1, node.Key2 = index.Kids[0], index.Kids[1]
	} else {
		node.Key1 = index
	}

	if node.Key1.Type == QL_CMP_EQ && node.Key2.Type != 0 {
		p.pErr("bad 'INDEX BY': expect only 1 '='")
		return
	}

	p.verifyScanKey(&node.Key1)
	if node.Key2.Type != 0 {
		p.verifyScanKey(&node.Key2)
	}

}

func (p *Parser) verifyScanKey(node *QLNode) {
	switch node.Type {
	case QL_CMP_EQ, QL_CMP_GE, QL_CMP_GT, QL_CMP_LT, QL_CMP_LE:
	case QL_CMP_NE:
		p.pErr("bad 'INDEX BY': '!=' not allowed")
		return
	default:
		p.pErr("bad 'INDEX BY': expect comparison")
		return
	}

	left, right := node.Kids[0], node.Kids[1]

	// convert both sides to 1-element tuple if both are not tuple
	if left.Type != QL_TUP && right.Type != QL_TUP {
		left = QLNode{Type: QL_TUP, Kids: []QLNode{left}}
		right = QLNode{Type: QL_TUP, Kids: []QLNode{right}}
	}

	// cannot compare tuple and single value
	if left.Type != QL_TUP || right.Type != QL_TUP {
		p.pErr("bad 'INDEX BY': compare tuple and single value")
		return
	}

	// both sides must have the same numbers of elements
	if len(left.Kids) != len(right.Kids) {
		p.pErr("bad 'INDEX BY': columns count not match values count")
		return
	}

	// left side must contain only column names
	for _, name := range left.Kids {
		if name.Type != QL_SYM {
			p.pErr(
				"bad 'INDEX BY': left side of comparison must be column name(s)",
			)
			return
		}
	}

	// update original AST node with normalized tuples
	node.Kids[0], node.Kids[1] = left, right
}

// 2 forms:
// - LIMIT count
// - LIMIT offset, count
func (p *Parser) pLimit(node *QLScan) {
	offset, count := QLNode{}, QLNode{}
	ok := p.tryNum(&count)
	if p.match(",") {
		offset = count
		ok = ok && p.tryNum(&count)
	}
	if !ok {
		p.pErr("bad 'limit'")
		return
	}

	node.Offset = offset.I64
	if count.Type != 0 {
		node.Limit = count.I64
	}
}

func (p *Parser) pInsert(mode int) *QLInsert {
	// INSERT INTO <table> (col, ...)
	stmt := QLInsert{}
	stmt.Mode = mode
	stmt.Table = p.pSym()
	stmt.Names = p.pNameList()

	// VALUES (expr, ...), ...
	p.consume("values", "expect 'values' after 'insert into <table> ...'")
	p.pCommaList(func() {
		stmt.Values = append(stmt.Values, p.pValueList())
	})

	for _, row := range stmt.Values {
		if len(row) != len(stmt.Names) {
			p.pErr("values length not match columns length")
			return nil
		}
	}

	return &stmt
}

// parse a comma-separated list of expressions enclosed in parentheses
func (p *Parser) pValueList() (vals []QLNode) {
	p.consume("(", "expect '(' before values list")
	p.pGroupedCommaList(func() {
		val := QLNode{}
		p.pExprOr(&val)
		vals = append(vals, val)
	})
	return vals
}

func (p *Parser) pUpdate() *QLUpdate {
	// UPDATE <table>
	stmt := QLUpdate{}
	stmt.Table = p.pSym()

	// SET a = ..., ...
	p.consume("set", "expect 'set' after 'update <table>'")
	p.pCommaList(func() {
		p.pAssign(&stmt)
	})

	// INDEX BY ... FILTER ... LIMIT ...
	p.pScan(&stmt.QLScan)

	return &stmt
}

// parse 'col = expr'
func (p *Parser) pAssign(stmt *QLUpdate) {
	stmt.Names = append(stmt.Names, p.pSym())
	p.consume("=", "expect '=' in 'set ... col = expr ...'")

	val := QLNode{}
	p.pExprOr(&val)
	stmt.Values = append(stmt.Values, val)
}

func (p *Parser) pDelete() *QLDelete {
	// DELETE FROM <table>
	stmt := QLDelete{}
	stmt.Table = p.pSym()

	// INDEX BY ... FILTER ... LIMIT ...
	p.pScan(&stmt.QLScan)

	return &stmt
}

/*
Operator precedence (lowest to highest):
. OR
. AND
. NOT
. =, !=, <, <=, >, >=
. +, -
. *, /, %
. - (negation)
*/

// parse binary operation (left-associative)
func (p *Parser) pExprBinOp(
	node *QLNode,
	ops []string, // operator tokens
	types []uint32, // expression types correspond to operator tokens
	next func(*QLNode), // function to parse next higher precedence level
) {
	assert(len(ops) == len(types))
	left := QLNode{}
	next(&left)

	for {
		i := 0
		for i < len(ops) && !p.match(ops[i]) {
			i++
		}

		if i == len(ops) {
			*node = left
			return
		}

		right := QLNode{}
		next(&right)
		left = QLNode{Type: types[i], Kids: []QLNode{left, right}}
	}
}

// parse unary operation
func (p *Parser) pExprUnOp(
	node *QLNode,
	op string, // operator token
	type_ uint32, // expression type correspond to operator token
	next func(*QLNode), // function to parse next higher precedence level
) {
	if p.match(op) {
		node.Type = type_
		kid := QLNode{}
		next(&kid)
		node.Kids = []QLNode{kid}
	} else {
		next(node)
	}
}

func (p *Parser) pExprOr(node *QLNode) {
	p.pExprBinOp(node, []string{"or"}, []uint32{QL_OR}, p.pExprAnd)
}

func (p *Parser) pExprAnd(node *QLNode) {
	p.pExprBinOp(node, []string{"and"}, []uint32{QL_AND}, p.pExprNot)
}

func (p *Parser) pExprNot(node *QLNode) {
	p.pExprUnOp(node, "not", QL_NOT, p.pExprCmp)
}

func (p *Parser) pExprCmp(node *QLNode) {
	p.pExprBinOp(node,
		[]string{"=", "!=", ">=", "<=", ">", "<"},
		[]uint32{QL_CMP_EQ, QL_CMP_NE,
			QL_CMP_GE, QL_CMP_LE, QL_CMP_GT, QL_CMP_LT},
		p.pExprAdd,
	)
}

// parse +, -
func (p *Parser) pExprAdd(node *QLNode) {
	p.pExprBinOp(node, []string{"+", "-"},
		[]uint32{QL_ADD, QL_SUB}, p.pExprMul)
}

// parse *, /, %
func (p *Parser) pExprMul(node *QLNode) {
	p.pExprBinOp(node, []string{"*", "/", "%"},
		[]uint32{QL_MUL, QL_DIV, QL_MOD}, p.pExprNeg)
}

func (p *Parser) pExprNeg(node *QLNode) {
	p.pExprUnOp(node, "-", QL_NEG, p.pExprAtom)
}

// parse comma-separated list of symbols enclosed in parentheses
func (p *Parser) pExprTuple(node *QLNode) {
	kids := []QLNode{}
	p.pGroupedCommaList(func() {
		kid := QLNode{}
		p.pExprOr(&kid)
		kids = append(kids, kid)
	})

	if len(kids) == 1 { // single kid
		*node = kids[0]
	} else {
		node.Type = QL_TUP
		node.Kids = kids
	}
}

// parse tuple/symbol/number/string
func (p *Parser) pExprAtom(node *QLNode) {
	switch {
	case p.match("("):
		p.pExprTuple(node)
	case p.trySym(node):
	case p.tryNum(node):
	case p.tryStr(node):
	default:
		p.pErr("expect symbol/number/string/tuple")
	}
}

// is letter / number / '_'
func isSym(ch byte) bool {
	r := rune(ch)
	return unicode.IsLetter(r) || unicode.IsNumber(r) || ch == '_'
}

// is letter / '_'
func isSymStart(ch byte) bool {
	return unicode.IsLetter(rune(ch)) || ch == '_'
}

// parse symbol (name)
func (p *Parser) pSym() string {
	name := QLNode{}
	if !p.trySym(&name) {
		p.pErr("expect name")
		return ""
	}
	return string(name.Str)
}

// try parsing symbol (name); return true and advance pointer if match
func (p *Parser) trySym(node *QLNode) bool {
	p.skipSpaces()
	curr := p.idx

	if !(curr < len(p.input) && isSymStart(p.input[curr])) {
		return false
	}
	curr++
	for curr < len(p.input) && isSym(p.input[curr]) {
		curr++
	}

	nameBytes := p.input[p.idx:curr]
	if keywordSet[strings.ToLower(string(nameBytes))] {
		return false // reserved keyword, not allowed
	}

	node.Type = QL_SYM
	node.Str = nameBytes
	p.idx = curr
	return true
}

// try parsing POSITIVE int64; return true and advance pointer if match
func (p *Parser) tryNum(node *QLNode) bool {
	p.skipSpaces()
	curr := p.idx

	for curr < len(p.input) && unicode.IsNumber(rune(p.input[curr])) {
		curr++
	}

	if curr == p.idx {
		return false
	}
	if curr < len(p.input) && isSym(p.input[curr]) {
		return false
	}

	i64, err := strconv.ParseInt(string(p.input[p.idx:curr]), 10, 64)
	if err != nil {
		return false
	}

	node.Type = TYPE_INT64
	node.I64 = i64
	p.idx = curr
	return true
}

// try parsing string; return true and advance pointer if match
func (p *Parser) tryStr(node *QLNode) bool {
	p.skipSpaces()
	curr := p.idx

	if !(curr < len(p.input) &&
		(p.input[curr] == '"' || p.input[curr] == '\'')) {
		return false
	}

	quote := p.input[curr]
	curr++ // opening quote

	var s []byte
	for curr < len(p.input) && p.input[curr] != quote {
		if p.input[curr] == '\\' { // escape
			curr++
			if curr == len(p.input) {
				p.pErr("string not terminated")
				return false
			}

			switch p.input[curr] {
			case '"', '\'', '\\':
				s = append(s, p.input[curr])
				curr++
			default:
				p.pErr("unknown escape")
				return false
			}
		} else {
			s = append(s, p.input[curr])
			curr++
		}
	}

	if !(curr < len(p.input) && p.input[curr] == quote) {
		p.pErr("string not terminated")
		return false
	}
	curr++ // closing quote

	node.Type = QL_STR
	node.Str = s
	p.idx = curr
	return true
}

// parse a single QL statement
func (p *Parser) Parse() (r interface{}, err error) {
	return p.pStmt()
}

type StmtScanner struct {
	input []byte
	idx   int
}

func (s *StmtScanner) skipSpaces() {
	for s.idx < len(s.input) && isSpace(s.input[s.idx]) {
		s.idx++
	}
}

// get next QL statement string from input (not verified)
func (s *StmtScanner) NextStmtStr() (stmtStr []byte, err error) {
	s.skipSpaces()
	if s.idx == len(s.input) {
		return nil, nil
	}

	end := s.idx
	for end < len(s.input) && s.input[end] != ';' {
		end++
	}

	if end == s.idx { // s.input[s.idx] == ';'
		return nil, errors.New("scan: empty stmt")
	}
	if end == len(s.input) {
		return nil, errors.New("scan: stmt not terminated")
	}

	stmtStr = s.input[s.idx : end+1] // include ';'
	s.idx = end + 1
	return stmtStr, nil
}
