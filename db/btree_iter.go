package db

import "bytes"

// iterator over B+tree
type BIter struct {
	tree *BTree
	path []BNode  // from root to leaf
	pos  []uint16 // current position in each node on 'path'
}

// seek to last position <= key
func (tree *BTree) SeekLE(key []byte) *BIter {
	iter := &BIter{tree: tree}
	for ptr := tree.root; ptr != 0; {
		node := BNode(tree.get(ptr))
		idx := nodeLookupLE(node, key)
		iter.path = append(iter.path, node)
		iter.pos = append(iter.pos, idx)
		ptr = node.getPtr(idx)
	}
	return iter
}

// get current KV pair
func (iter *BIter) Deref() ([]byte, []byte) {
	assert(iter.Valid())
	last := len(iter.path) - 1
	node := iter.path[last] // leaf node
	pos := iter.pos[last]
	return node.getKey(pos), node.getVal(pos)
}

// currently at first key (dummy entry)
func iterIsFirst(iter *BIter) bool {
	for _, pos := range iter.pos {
		if pos != 0 {
			return false
		}
	}
	return true
}

// empty tree or past the last key
// (past-last-key logic: see iterNext())
func iterIsEnd(iter *BIter) bool {
	last := len(iter.path) - 1
	return last < 0 || iter.pos[last] == iter.path[last].nkeys()
}

// precondition of Deref()
func (iter *BIter) Valid() bool {
	return !(iterIsFirst(iter) || iterIsEnd(iter))
}

// move forward; stay if already past last key
func (iter *BIter) Next() {
	if !iterIsEnd(iter) {
		// try within current leaf first
		iterNext(iter, len(iter.path)-1)
	}
}

// move backward; stay if already at dummy key
func (iter *BIter) Prev() {
	if !iterIsFirst(iter) {
		// try within current leaf first
		iterPrev(iter, len(iter.path)-1)
	}
}

func iterNext(iter *BIter, level int) {
	if iter.pos[level]+1 < iter.path[level].nkeys() {
		iter.pos[level]++ // move within current node
	} else if level > 0 {
		iterNext(iter, level-1) // go back to parent to move to sibling
	} else {
		leaf := len(iter.pos) - 1
		iter.pos[leaf]++ // past the last key
		assert(iter.pos[leaf] == iter.path[leaf].nkeys())
		return
	}

	if level < len(iter.pos)-1 { // internal node
		node := iter.path[level] // initial node OR moved to sibling
		// update next node on path (to child at current 'pos')
		kid := BNode(iter.tree.get(node.getPtr(iter.pos[level])))
		iter.path[level+1] = kid
		iter.pos[level+1] = 0 // 1st key
	}
}

func iterPrev(iter *BIter, level int) {
	if iter.pos[level] > 0 {
		iter.pos[level]-- // move within current node
	} else if level > 0 {
		iterPrev(iter, level-1) // go back to parent to move to sibling
	} else {
		panic("unreachable") // currently at dummy key
	}

	if level < len(iter.pos)-1 { // internal node
		node := iter.path[level] // initial node OR moved to sibling
		// update next node on path (to child at current 'pos')
		kid := BNode(iter.tree.get(node.getPtr(iter.pos[level])))
		iter.path[level+1] = kid
		iter.pos[level+1] = kid.nkeys() - 1 // last key
	}
}

const (
	CMP_GE = 3  // >=
	CMP_GT = 2  // >
	CMP_LT = -2 // <
	CMP_LE = -3 // <=
)

// check if current key and sought key satisfy 'cmp' relation
func cmpOK(curr []byte, cmp int, key []byte) bool {
	r := bytes.Compare(curr, key)
	switch cmp {
	case CMP_GE:
		return r >= 0
	case CMP_GT:
		return r > 0
	case CMP_LT:
		return r < 0
	case CMP_LE:
		return r <= 0
	default:
		panic("unreachable")
	}
}

// seek to closest position that satisfies 'cmp' relation with 'key'
func (tree *BTree) Seek(key []byte, cmp int) *BIter {
	iter := tree.SeekLE(key)
	assert(iterIsFirst(iter) || !iterIsEnd(iter))

	if cmp != CMP_LE {
		curr := []byte(nil) // dummy key
		if !iterIsFirst(iter) {
			curr, _ = iter.Deref()
		}

		if !cmpOK(curr, cmp, key) {
			// only adjust by 1 since keys are unique
			if cmp > 0 { // >= or >
				iter.Next()
			} else { // <
				iter.Prev()
			}
		}
	}

	if iter.Valid() {
		curr, _ := iter.Deref()
		assert(cmpOK(curr, cmp, key))
	}
	return iter
}
