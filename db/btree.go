package db

import (
	"bytes"
	"encoding/binary"
)

/*
Node format:
| type | nkeys | pointers   | offsets    | KVs | unused |
| 2B   | 2B    | nkeys × 8B | nkeys × 2B | ... |        |
. type: leaf or internal
. nkeys: number of keys (and number of child pointers)
. offset of the 1st KV is always 0, so it is not stored.
  last item is offset just past the end

KV format:
| key_size | val_size | key | val |
| 2B       | 2B       | ... | ... |
. val_size = 0 for internal nodes

Sub-ranges by keys:
. To divide a range into n sub-ranges, we need n-1 keys.
. But our format uses n keys, each key represents start of sub-range.
  (1st key in an internal node is redundant,
   since range start is inherited from parent node)
*/

const BTREE_PAGE_SIZE = 4096 // typical OS page size

// limit KV size to fit inside a node
const BTREE_MAX_KEY_SIZE = 1000
const BTREE_MAX_VAL_SIZE = 3000

func init() {
	node1max := 4 + 1*8 + 1*2 + BTREE_MAX_KEY_SIZE + BTREE_MAX_VAL_SIZE
	assert(node1max <= BTREE_PAGE_SIZE) // maximum KV
}

const (
	BNODE_NODE = 1 // internal nodes without values
	BNODE_LEAF = 2 // leaf nodes with values
)

type BNode []byte // can be dumped to disk

// ===== header =====

func (node BNode) btype() uint16 {
	return binary.LittleEndian.Uint16(node[0:2])
}
func (node BNode) nkeys() uint16 {
	return binary.LittleEndian.Uint16(node[2:4])
}
func (node BNode) setHeader(btype uint16, nkeys uint16) {
	binary.LittleEndian.PutUint16(node[0:2], btype)
	binary.LittleEndian.PutUint16(node[2:4], nkeys)
}

// ===== child pointers =====

func (node BNode) getPtr(idx uint16) uint64 {
	assert(idx < node.nkeys())
	pos := 4 + 8*idx
	return binary.LittleEndian.Uint64(node[pos:])
}
func (node BNode) setPtr(idx uint16, val uint64) {
	assert(idx < node.nkeys())
	pos := 4 + 8*idx
	binary.LittleEndian.PutUint64(node[pos:], val)
}

// ===== offsets =====

func offsetPos(node BNode, idx uint16) uint16 {
	assert(1 <= idx && idx <= node.nkeys()) // allow offset just past the end
	return 4 + 8*node.nkeys() + 2*(idx-1)
}
func (node BNode) getOffset(idx uint16) uint16 {
	if idx == 0 {
		return 0 // offset of the 1st KV is always 0 (not stored in 'offsets')
	}
	pos := offsetPos(node, idx)
	return binary.LittleEndian.Uint16(node[pos:])
}
func (node BNode) setOffset(idx uint16, offset uint16) {
	pos := offsetPos(node, idx)
	binary.LittleEndian.PutUint16(node[pos:], offset)
}

// ===== KV =====

func (node BNode) kvPos(idx uint16) uint16 {
	assert(idx <= node.nkeys()) // allow offset just past the end
	return 4 + 8*node.nkeys() + 2*node.nkeys() + node.getOffset(idx)
}
func (node BNode) getKey(idx uint16) []byte {
	assert(idx < node.nkeys())
	pos := node.kvPos(idx)
	klen := binary.LittleEndian.Uint16(node[pos:])
	return node[pos+4:][:klen]
}
func (node BNode) getVal(idx uint16) []byte {
	assert(idx < node.nkeys())
	pos := node.kvPos(idx)
	klen := binary.LittleEndian.Uint16(node[pos:])
	vlen := binary.LittleEndian.Uint16((node[pos+2:]))
	return node[pos+4+klen:][:vlen]
}

// add KV pair or pointer to node; keys are added in order
//   - ptr: child pointer, unused for leaf nodes.
//   - key & val: use empty values for internal nodes.
func nodeAppendKV(new BNode, idx uint16, ptr uint64, key []byte, val []byte) {
	// ptr
	new.setPtr(idx, ptr)

	// KV
	pos := new.kvPos(idx) // use offset value set by previous key
	binary.LittleEndian.PutUint16(new[pos:], uint16(len(key)))
	binary.LittleEndian.PutUint16(new[pos+2:], uint16(len(val)))
	copy(new[pos+4:], key)
	copy(new[pos+4+uint16(len(key)):], val)

	// update offset value for the next key
	new.setOffset(idx+1, new.getOffset(idx)+4+uint16(len(key)+len(val)))
}

// node size in bytes
func (node BNode) nbytes() uint16 {
	return node.kvPos(node.nkeys()) // use offset just past the last KV
}

/*
Copy-on-write:
- Insertion/deletion starts at leaf node.
  After making a copy with modification, parent node is updated to point to new node.
  which is also done on its copy.
  The copying propagates to root node, resulting in a new tree root.
- The original tree remains intact and accessible from the old root.
- The new root shares all other nodes with the original tree
  (outside of the updated path).

Alternative (not used): Double-write
- Save a copy of the entire node to update, and update that copy
  (similar to copy on write but without copying parent).
  fsync the saved copy.
- Actually update the node to update in-place.
  fsync the update.
- After a crash, data may be half-updated, but we always apply the saved copy,
  so the data structure ends with updated state.
- Use checksum to detect corrupted double-write before the 1st fsync,
  don't update main data if bad double-write detected
*/

// leaf node: insert new key at 'idx'
func leafInsert(new BNode, old BNode, idx uint16, key []byte, val []byte) {
	new.setHeader(BNODE_LEAF, old.nkeys()+1)
	nodeAppendRange(new, old, 0, 0, idx)                   // copy keys before 'idx'
	nodeAppendKV(new, idx, 0, key, val)                    // insert new key
	nodeAppendRange(new, old, idx+1, idx, old.nkeys()-idx) // copy keys from 'idx'
}

// copy multiple KV pairs and pointers from 'old' to 'new'
func nodeAppendRange(
	new BNode, old BNode, dstNew uint16, srcOld uint16, n uint16,
) {
	for i := uint16(0); i < n; i++ {
		dst, src := dstNew+i, srcOld+i
		nodeAppendKV(new, dst,
			old.getPtr(src), old.getKey(src), old.getVal(src))
	}
}

// leaf node: update value of existing key
func leafUpdate(
	new BNode, old BNode, idx uint16, key []byte, val []byte,
) {
	new.setHeader(BNODE_LEAF, old.nkeys())
	nodeAppendRange(new, old, 0, 0, idx)                         // copy keys before 'idx'
	nodeAppendKV(new, idx, 0, key, val)                          // insert key with new value
	nodeAppendRange(new, old, idx+1, idx+1, old.nkeys()-(idx+1)) // copy keys from 'idx + 1'
}

// find insert/update position (keep keys in increasing order)
//   - find last position <= key
//   - TODO (improvement): use binary search
func nodeLookupLE(node BNode, key []byte) uint16 {
	nkeys := node.nkeys()
	var i uint16
	for i = 0; i < nkeys; i++ {
		cmp := bytes.Compare(node.getKey(i), key)
		if cmp == 0 {
			return i // there is only 1 position == key
		}
		if cmp > 0 {
			return i - 1 // can be -1
		}
	}
	return i - 1
}

// split an oversized node into 2 nodes
//   - the 2nd node always fits
//   - the 1st node may still be too big
func nodeSplit2(left BNode, right BNode, old BNode) {
	assert(old.nkeys() >= 2)

	// initial guess
	nleft := old.nkeys() / 2

	// try to fit the right half
	left_bytes := func() uint16 {
		return 4 + 8*nleft + 2*nleft + old.getOffset(nleft)
	}
	right_bytes := func() uint16 {
		return old.nbytes() - left_bytes() + 4
	}
	for right_bytes() > BTREE_PAGE_SIZE {
		nleft++
	}
	assert(nleft < old.nkeys())
	nright := old.nkeys() - nleft

	// new nodes
	left.setHeader(old.btype(), nleft)
	right.setHeader(old.btype(), nright)
	nodeAppendRange(left, old, 0, 0, nleft)
	nodeAppendRange(left, old, 0, nleft, nright)

	// NOTE: the left half may still be too big
	assert(right.nbytes() <= BTREE_PAGE_SIZE)
}

// split a node if it's too big; results are 1~3 nodes
//   - we limit the size of 1 KV to 4000 (< BTREE_PAGE_SIZE)
//   - when a KV of that size is inserted in the middle,
//     3 new nodes are needed.
func nodeSplit3(old BNode) (uint16, [3]BNode) {
	if old.nbytes() <= BTREE_PAGE_SIZE {
		old = old[:BTREE_PAGE_SIZE]
		return 1, [3]BNode{old} // not split
	}

	left := BNode(make([]byte, 2*BTREE_PAGE_SIZE)) // might be split further
	right := BNode(make([]byte, BTREE_PAGE_SIZE))
	nodeSplit2(left, right, old)
	if left.nbytes() <= BTREE_PAGE_SIZE {
		left = left[:BTREE_PAGE_SIZE]
		return 2, [3]BNode{left, right}
	}

	leftleft := BNode(make([]byte, BTREE_PAGE_SIZE))
	middle := BNode(make([]byte, BTREE_PAGE_SIZE))
	nodeSplit2(leftleft, middle, left)
	assert(leftleft.nbytes() <= BTREE_PAGE_SIZE)
	return 3, [3]BNode{leftleft, middle, right}
}

// replace a kid with new kid(s) (after splitting)
func nodeReplaceKidN(
	tree *BTree, new BNode, old BNode, idx uint16,
	kids ...BNode,
) {
	nkeys_inc := uint16(len(kids)) - 1
	new.setHeader(BNODE_NODE, old.nkeys()+nkeys_inc)
	nodeAppendRange(new, old, 0, 0, idx)
	for i, node := range kids {
		nodeAppendKV(new, idx+uint16(i), tree.new(node), node.getKey(0), nil)
	}
	nodeAppendRange(new, old, idx+uint16(len(kids)), idx+1, old.nkeys()-(idx+1))
}

type BTree struct {
	root uint64 // root pointer (a nonzero page number)

	// callbacks for managing on-disk pages
	get func(uint64) []byte // read data from a page number
	new func([]byte) uint64 // allocate a new page number with data
	del func(uint64)        // deallocate a page number
}

func treeInsert(tree *BTree, node BNode, key []byte, val []byte) BNode {
	new := BNode(make([]byte, 2*BTREE_PAGE_SIZE)) // allow exceeding 1 page temporarily
	idx := nodeLookupLE(node, key)                // node.getKey(idx) <= key

	switch node.btype() {
	case BNODE_LEAF: // leaf node
		if bytes.Equal(key, node.getKey(idx)) { // key found -> update
			leafUpdate(new, node, idx, key, val)
		} else { // key not found -> insert
			leafInsert(new, node, idx+1, key, val)
		}
	case BNODE_NODE: // internal node
		// recursive insertion to the kid node
		kptr := node.getPtr(idx)
		knode := treeInsert(tree, tree.get(kptr), key, val)

		// split (if needed) after insertion
		nsplit, split := nodeSplit3(knode)

		// deallocate the old kid node
		tree.del(kptr)

		// point to the new kid(s) after splitting
		// propagate up the parent chain
		nodeReplaceKidN(tree, new, node, idx, split[:nsplit]...)
	default:
		panic("invalid node type!")
	}

	return new
}
