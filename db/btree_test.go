package db

import (
	"crypto/rand"
	"fmt"
	"sort"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

type BTreeTestCtx struct {
	tree  BTree
	pages map[uint64]BNode  // simulate pages in memory
	ref   map[string]string // reference data (track inserted KVs)
}

func newBTreeTestCtx() *BTreeTestCtx {
	pages := map[uint64]BNode{}
	return &BTreeTestCtx{
		tree: BTree{
			get: func(ptr uint64) []byte {
				node, ok := pages[ptr]
				assert(ok)
				return node
			},
			new: func(node []byte) uint64 {
				assert(BNode(node).nbytes() <= BTREE_PAGE_SIZE)
				ptr := uint64(uintptr(unsafe.Pointer(&node[0])))
				assert(pages[ptr] == nil)
				pages[ptr] = node
				return ptr
			},
			del: func(ptr uint64) {
				assert(pages[ptr] != nil)
				delete(pages, ptr)
			},
		},
		pages: pages,
		ref:   map[string]string{},
	}
}

func (c *BTreeTestCtx) add(key string, val string) {
	err := c.tree.Insert([]byte(key), []byte(val))
	assert(err == nil)
	c.ref[key] = val
}

func (c *BTreeTestCtx) del(key string) bool {
	delete(c.ref, key)
	deleted, err := c.tree.Delete([]byte(key))
	assert(err == nil)
	return deleted
}

// dump KVs in tree
func (c *BTreeTestCtx) dump() ([]string, []string) {
	keys := []string{}
	vals := []string{}

	var nodeDump func(uint64)
	nodeDump = func(ptr uint64) {
		node := BNode(c.tree.get(ptr))
		nkeys := node.nkeys()

		if node.btype() == BNODE_LEAF {
			for i := uint16(0); i < nkeys; i++ {
				keys = append(keys, string(node.getKey(i)))
				vals = append(vals, string(node.getVal(i)))
			}
		} else { // internal node
			for i := uint16(0); i < nkeys; i++ {
				ptr := node.getPtr(i)
				nodeDump(ptr)
			}
		}
	}

	nodeDump(c.tree.root)
	assert(keys[0] == "") // string(nil) - sentinel value
	assert(vals[0] == "") // string(nil) - sentinel value
	return keys[1:], vals[1:]
}

func (c *BTreeTestCtx) verify(t *testing.T) {
	keys, vals := c.dump()

	// reference
	rkeys, rvals := []string{}, []string{}
	for k, v := range c.ref {
		rkeys = append(rkeys, k)
		rvals = append(rvals, v)
	}

	require.Equal(t, len(rkeys), len(keys))

	sort.Stable(sortIF{
		len: len(rkeys),
		less: func(i, j int) bool {
			return rkeys[i] < rkeys[j] // strings are compared lexicographically
		},
		swap: func(i, j int) {
			k, v := rkeys[i], rvals[i]
			rkeys[i], rvals[i] = rkeys[j], rvals[j]
			rkeys[j], rvals[j] = k, v
		},
	})

	require.Equal(t, rkeys, keys)
	require.Equal(t, rvals, vals)

	var nodeVerify func(BNode)
	nodeVerify = func(node BNode) {
		nkeys := node.nkeys()
		assert(nkeys >= 1)

		if node.btype() == BNODE_LEAF {
			return
		}

		// internal node
		for i := uint16(0); i < nkeys; i++ {
			key := node.getKey(i)
			kid := BNode(c.tree.get(node.getPtr(i)))
			require.Equal(t, key, kid.getKey(0))
			nodeVerify(kid)
		}
	}

	nodeVerify(c.tree.get(c.tree.root))
}

func commonTestBasic(t *testing.T, hasher func(uint32) uint32) {
	c := newBTreeTestCtx()
	c.add("k", "v")
	c.verify(t)

	// insert
	for i := 0; i < 2500; i++ {
		key := fmt.Sprintf("key%d", hasher(uint32(i)))
		val := fmt.Sprintf("val%d", hasher(uint32(-i)))
		c.add(key, val)
		if i < 200 {
			c.verify(t)
		}
	}
	c.verify(t)

	// del
	for i := 200; i < 2500; i++ {
		key := fmt.Sprintf("key%d", hasher(uint32(i)))
		require.True(t, c.del(key))
	}
	c.verify(t)

	// overwrite
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%d", hasher(uint32(i)))
		val := fmt.Sprintf("vvv%d", hasher(uint32(+i)))
		c.add(key, val)
		c.verify(t)
	}

	require.False(t, c.del("kk"))

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%d", hasher(uint32(i)))
		require.True(t, c.del(key))
		c.verify(t)
	}

	c.add("k", "v2")
	c.verify(t)
	c.del("k")
	c.verify(t)

	// the dummy empty key remains
	require.Equal(t, 1, len(c.pages))
	require.Equal(t, uint16(1), BNode(c.tree.get(c.tree.root)).nkeys())
}

func TestBTreeBasicAscending(t *testing.T) {
	// keys are generated in increasing order
	commonTestBasic(t, func(h uint32) uint32 { return +h })
}

func TestBTreeBasicDescending(t *testing.T) {
	// keys are generated in decreasing order
	commonTestBasic(t, func(h uint32) uint32 { return -h })
}

func TestBTreeBasicRand(t *testing.T) {
	commonTestBasic(t, fmix32)
}

func TestBTreeRandLength(t *testing.T) {
	c := newBTreeTestCtx()
	for i := 0; i < 200; i++ {
		klen := fmix32(uint32(2*i)) % BTREE_MAX_KEY_SIZE
		vlen := fmix32(uint32(2*i+1)) % BTREE_MAX_VAL_SIZE
		if klen == 0 {
			continue
		}

		key := make([]byte, klen)
		rand.Read(key)
		val := make([]byte, vlen)
		c.add(string(key), string(val))
		c.verify(t)
	}
}

func TestBTreeIncLength(t *testing.T) {
	for l := 1; l < BTREE_MAX_KEY_SIZE+BTREE_MAX_VAL_SIZE; l += 40 {
		c := newBTreeTestCtx()

		klen := l
		if klen > BTREE_MAX_KEY_SIZE {
			klen = BTREE_MAX_KEY_SIZE
		}
		vlen := l - klen

		key := make([]byte, klen)
		val := make([]byte, vlen)

		// maximum fan-out factor
		factor := BTREE_PAGE_SIZE / l

		// number of items to insert to reach height >= 3 (root -> internal -> leaf)
		// . reach height 2: need roughly 'factor' items
		// . reach height 3: need roughly 'factor*factor' items
		size := factor * factor * 2
		if size > 4000 {
			size = 4000
		}
		if size < 10 {
			size = 10
		}

		for i := 0; i < size; i++ {
			rand.Read(key)
			c.add(string(key), string(val))
		}
		c.verify(t)
	}
}
