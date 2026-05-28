package db

import (
	"bytes"
	"encoding/binary"
	"errors"
)

/*
B+Tree:
. a height-balanced n-ary tree
. insert a key may cause splitting, which may increase tree height
. delete a key may allow merging, which may decrease tree height

Node format:
| type | nkeys | pointers   | offsets    | KVs | unused |
| 2B   | 2B    | nkeys x 8B | nkeys x 2B | ... |        |
. type: leaf or internal
. nkeys: number of keys (and number of child pointers)
. offets: offset of the 1st KV is always 0, so it is not stored.
  last item is offset just past the end.

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

const HEADER_SIZE = 4
const BTREE_PAGE_SIZE = 4096 // typical OS page size

// limit KV size to fit inside a node
const BTREE_MAX_KEY_SIZE = 1000
const BTREE_MAX_VAL_SIZE = 3000

func init() {
	node1max := HEADER_SIZE + 8 + 2 + 4 + BTREE_MAX_KEY_SIZE + BTREE_MAX_VAL_SIZE
	assert(node1max <= BTREE_PAGE_SIZE)
}

const (
	BNODE_NODE = 1 // internal nodes without values
	BNODE_LEAF = 2 // leaf nodes with values
)

type BNode []byte // can be dumped to disk

// === header ===

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

// === child pointers ===

func (node BNode) getPtr(idx uint16) uint64 {
	assert(idx < node.nkeys())
	pos := HEADER_SIZE + 8*idx
	return binary.LittleEndian.Uint64(node[pos:])
}
func (node BNode) setPtr(idx uint16, val uint64) {
	assert(idx < node.nkeys())
	pos := HEADER_SIZE + 8*idx
	binary.LittleEndian.PutUint64(node[pos:], val)
}

// === offsets ===

func offsetPos(node BNode, idx uint16) uint16 {
	assert(1 <= idx && idx <= node.nkeys()) // allow offset just past the end
	return HEADER_SIZE + 8*node.nkeys() + 2*(idx-1)
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

// === KV ===

func (node BNode) kvPos(idx uint16) uint16 {
	assert(idx <= node.nkeys()) // allow offset just past the end
	return HEADER_SIZE + 8*node.nkeys() + 2*node.nkeys() + node.getOffset(idx)
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
func nodeAppendKV(
	new BNode, idx uint16, ptr uint64, key []byte, val []byte,
) {
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

type BTree struct {
	root uint64 // root pointer (a nonzero page number)

	// === callbacks for managing on-disk pages ===

	get func(uint64) []byte // read data from a page number
	new func([]byte) uint64 // allocate a new page with data
	del func(uint64)        // deallocate a page
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
- Save a copy of just the node to update, and update that copy
  fsync the saved copy.
- Actually update the node to update in-place.
  fsync the update.
- After a crash, data may be half-updated, but we always apply the saved copy,
  so the data structure ends with updated state.
- Use checksum to detect corrupted double-write before the 1st fsync,
  don't update main data if bad double-write detected
*/

// leaf node: insert new key at 'idx'
func leafInsert(
	new BNode, old BNode, idx uint16, key []byte, val []byte,
) {
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

// binary search for last position <= key
// (keys in each node are unique and kept in increasing order)
func nodeLookupLE(node BNode, key []byte) uint16 {
	left := 0
	right := int(node.nkeys()) - 1

	for left <= right {
		mid := (left + right) / 2
		cmp := bytes.Compare(node.getKey(uint16(mid)), key)

		if cmp == 0 { // found (unique) key == search_key
			return uint16(mid)
		}
		if cmp > 0 { // key > search_key
			right = mid - 1
		} else { // key < search_key
			left = mid + 1
		}
	}

	return uint16(right) // can be -1 (wrap around to UIN16_MAX)
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
		return HEADER_SIZE + 8*nleft + 2*nleft + old.getOffset(nleft)
	}
	right_bytes := func() uint16 {
		return old.nbytes() - left_bytes() + HEADER_SIZE
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
	nodeAppendRange(right, old, 0, nleft, nright)

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

// replace the kid at idx with new kid(s)
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

// update modes
const (
	MODE_UPSERT      = 0
	MODE_UPDATE_ONLY = 1
	MODE_INSERT_ONLY = 2
)

type UpdateReq struct {
	tree *BTree

	// === in ====

	Key  []byte
	Val  []byte
	Mode int

	// === out ===

	Added   bool   // added a new key
	Updated bool   // added a new key or updated an old key
	Old     []byte // value before update
}

// insert/update a key at a node; return updated node (copied)
func treeUpdate(req *UpdateReq, node BNode) BNode {
	new := BNode(make([]byte, 2*BTREE_PAGE_SIZE)) // allow exceeding 1 page temporarily
	idx := nodeLookupLE(node, req.Key)            // node.getKey(idx) <= key

	switch node.btype() {
	case BNODE_LEAF: // leaf node
		if bytes.Equal(req.Key, node.getKey(idx)) { // key found
			if req.Mode == MODE_INSERT_ONLY {
				return BNode{}
			}
			if bytes.Equal(req.Val, node.getVal(idx)) {
				return BNode{}
			}
			leafUpdate(new, node, idx, req.Key, req.Val)
			req.Updated = true
			req.Old = node.getVal(idx)
		} else { // key not found
			if req.Mode == MODE_UPDATE_ONLY {
				return BNode{}
			}
			leafInsert(new, node, idx+1, req.Key, req.Val)
			req.Updated = true
			req.Added = true
		}
		return new
	case BNODE_NODE: // internal node
		return nodeUpdate(req, new, node, idx)
	default:
		panic("invalid node type!")
	}
}

// insert/update a key at an internal node; part of treeUpdate()
func nodeUpdate(req *UpdateReq, new BNode, node BNode, idx uint16) BNode {
	// recursive insert/update to the kid node
	kptr := node.getPtr(idx)
	updated := treeUpdate(req, req.tree.get(kptr))
	if len(updated) == 0 { // not updated
		return BNode{}
	}

	// split (if needed) after insertion
	nsplit, split := nodeSplit3(updated)

	// deallocate the old kid node
	req.tree.del(kptr)

	// point to the new kid(s) after splitting
	nodeReplaceKidN(req.tree, new, node, idx, split[:nsplit]...)

	return new
}

// leaf node: remove a key
func leafDelete(new BNode, old BNode, idx uint16) {
	new.setHeader(BNODE_LEAF, old.nkeys()-1)
	nodeAppendRange(new, old, 0, 0, idx)
	nodeAppendRange(new, old, idx, idx+1, old.nkeys()-(idx+1))
}

// merge 2 nodes into 1
func nodeMerge(new BNode, left BNode, right BNode) {
	new.setHeader(left.btype(), left.nkeys()+right.nkeys())
	nodeAppendRange(new, left, 0, 0, left.nkeys())
	nodeAppendRange(new, right, left.nkeys(), 0, right.nkeys())
	assert(new.nbytes() <= BTREE_PAGE_SIZE)
}

// replace 2 adjacent kids (idx and idx+1) with 1
func nodeReplace2Kids(
	new BNode, old BNode, idx uint16, ptr uint64, key []byte,
) {
	new.setHeader(BNODE_NODE, old.nkeys()-1)
	nodeAppendRange(new, old, 0, 0, idx)
	nodeAppendKV(new, idx, ptr, key, nil)
	nodeAppendRange(new, old, idx+1, idx+2, old.nkeys()-(idx+2))
}

// should the updated kid be merged with an adjacent sibling
func shouldMerge(
	tree *BTree, node BNode, idx uint16, updated BNode,
) (mergeDirection int, sibling BNode) {
	if updated.nbytes() > BTREE_PAGE_SIZE/4 {
		return 0, BNode{}
	}
	if idx > 0 {
		sibling := BNode(tree.get(node.getPtr(idx - 1)))
		mergedSize := sibling.nbytes() + updated.nbytes() - HEADER_SIZE
		if mergedSize <= BTREE_PAGE_SIZE {
			return -1, sibling // left
		}
	}
	if idx < node.nkeys()-1 {
		sibling := BNode(tree.get(node.getPtr(idx + 1)))
		mergedSize := sibling.nbytes() + updated.nbytes() - HEADER_SIZE
		if mergedSize <= BTREE_PAGE_SIZE {
			return 1, sibling // right
		}
	}
	return 0, BNode{}
}

type DeleteReq struct {
	tree *BTree

	// === in ===

	Key []byte

	// === out ===

	Old []byte // deleted value
}

// delete a key from a node; return updated node (copied)
func treeDelete(req *DeleteReq, node BNode) BNode {
	idx := nodeLookupLE(node, req.Key) // node.getKey(idx) <= key
	switch node.btype() {
	case BNODE_LEAF:
		if !bytes.Equal(req.Key, node.getKey(idx)) { // key not found
			return BNode{}
		}

		// delete the key
		req.Old = node.getVal(idx)
		new := BNode(make([]byte, BTREE_PAGE_SIZE))
		leafDelete(new, node, idx)
		return new
	case BNODE_NODE:
		return nodeDelete(req, node, idx)
	default:
		panic("invalid node type!")
	}
}

// delete a key from an internal node; part of treeDelete()
func nodeDelete(req *DeleteReq, node BNode, idx uint16) BNode {
	tree := req.tree

	// recurse into the kid
	kptr := node.getPtr(idx)
	updated := treeDelete(req, tree.get(kptr))
	if len(updated) == 0 { // key not found
		return BNode{}
	}

	tree.del(kptr)
	new := BNode(make([]byte, BTREE_PAGE_SIZE))

	// check if need to merge
	mergeDirection, sibling := shouldMerge(tree, node, idx, updated)
	switch {
	case mergeDirection < 0: // left
		merged := BNode(make([]byte, BTREE_PAGE_SIZE))
		nodeMerge(merged, sibling, updated)
		tree.del(node.getPtr(idx - 1))
		nodeReplace2Kids(new, node, idx-1, tree.new(merged), merged.getKey(0))
	case mergeDirection > 0: // right
		merged := BNode(make([]byte, BTREE_PAGE_SIZE))
		nodeMerge(merged, updated, sibling)
		tree.del(node.getPtr(idx + 1))
		nodeReplace2Kids(new, node, idx, tree.new(merged), merged.getKey(0))
	case mergeDirection == 0 && updated.nkeys() == 0:
		assert(node.nkeys() == 1 && idx == 0) // 1 empty child but no sibling
		new.setHeader(BNODE_NODE, 0)          // the parent becomes empty too
	case mergeDirection == 0 && updated.nkeys() > 0: // no merge
		nodeReplaceKidN(tree, new, node, idx, updated)
	}

	return new
}

// check limit imposed by node format
func checkLimit(key []byte, val []byte) error {
	if len(key) == 0 {
		return errors.New("empty key")
	}
	if len(key) > BTREE_MAX_KEY_SIZE {
		return errors.New("key too long")
	}
	if len(val) > BTREE_MAX_VAL_SIZE {
		return errors.New("val too long")
	}
	return nil
}

// get value by key
func nodeGetKey(tree *BTree, node BNode, key []byte,
) (val []byte, found bool) {
	idx := nodeLookupLE(node, key)
	switch node.btype() {
	case BNODE_LEAF:
		if bytes.Equal(key, node.getKey(idx)) {
			return node.getVal(idx), true
		} else {
			return nil, false
		}
	case BNODE_NODE:
		return nodeGetKey(tree, tree.get(node.getPtr(idx)), key)
	default:
		panic("invalid node type!")
	}
}

// === interface ===

func (tree *BTree) Upsert(key []byte, val []byte) (bool, error) {
	return tree.Update(&UpdateReq{Key: key, Val: val})
}

// insert new key or update an existing key
func (tree *BTree) Update(req *UpdateReq) (bool, error) {
	// check limit imposed by node format
	if err := checkLimit(req.Key, req.Val); err != nil {
		return false, err
	}

	// create root node if tree is empty
	if tree.root == 0 {
		root := BNode(make([]byte, BTREE_PAGE_SIZE))

		// nodeLookupLE can return -1 if key < node's range
		// -> insert an empty key so lookup always finds a position
		root.setHeader(BNODE_LEAF, 2)
		nodeAppendKV(root, 0, 0, nil, nil)         // sentinel value
		nodeAppendKV(root, 1, 0, req.Key, req.Val) // current KV

		tree.root = tree.new(root)
		req.Added = true
		req.Updated = true
		return true, nil
	}

	req.tree = tree
	updated := treeUpdate(req, tree.get(tree.root))
	if len(updated) == 0 { // not updated
		return false, nil
	}

	// grow the tree if the root is split
	nsplit, split := nodeSplit3(updated)
	tree.del(tree.root)
	if nsplit > 1 { // root was split, add a new level
		root := BNode(make([]byte, BTREE_PAGE_SIZE))
		root.setHeader(BNODE_NODE, nsplit)
		for i, knode := range split[:nsplit] {
			ptr, key := tree.new(knode), knode.getKey(0)
			nodeAppendKV(root, uint16(i), ptr, key, nil)
		}
		tree.root = tree.new(root)
	} else {
		tree.root = tree.new(split[0])
	}

	return true, nil
}

// delete a key and returns whether the key exists
func (tree *BTree) Delete(req *DeleteReq) (deleted bool, err error) {
	if err := checkLimit(req.Key, nil); err != nil {
		return false, err
	}

	if tree.root == 0 { // empty tree
		return false, nil
	}

	req.tree = tree
	updated := treeDelete(req, tree.get(tree.root))
	if len(updated) == 0 { // key not found
		return false, nil
	}

	tree.del(tree.root)
	if updated.btype() == BNODE_NODE && updated.nkeys() == 1 {
		// remove a level
		tree.root = updated.getPtr(0)
	} else {
		tree.root = tree.new(updated)
	}
	return true, nil
}

// get value by key
func (tree *BTree) Get(key []byte) (val []byte, found bool) {
	if tree.root == 0 { // empty tree
		return nil, false
	}
	return nodeGetKey(tree, tree.get(tree.root), key)
}
