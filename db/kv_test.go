package db

import (
	"crypto/rand"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

type KVTestCtx struct {
	db  KV
	ref map[string]string // reference data (track inserted KVs)
}

// ===== fsync mocks =====
func fsyncSkip(int) error {
	return nil
}

// calls to 'db.Fsync' return errors based on errList
//   - return error if the head element is not 0
//   - the returned function retains access to errList, which are mutated in every call (cut head)
//
// see 'updateFile' and 'updateOrRevert' for details
func fsyncErr(errList ...int) func(int) error {
	return func(int) error {
		fail := errList[0]
		errList = errList[1:]
		if fail != 0 {
			return fmt.Errorf("fsync error!")
		}
		return nil
	}
}

func newKVTestCtx() *KVTestCtx {
	dbPath := "test.db"
	os.Remove(dbPath)

	c := &KVTestCtx{}
	c.ref = map[string]string{}
	c.db.Path = dbPath
	c.db.FSync = fsyncSkip

	err := c.db.Open()
	assert(err == nil)
	return c
}

func (c *KVTestCtx) reopen() {
	c.db.Close()
	c.db = KV{Path: c.db.Path, FSync: c.db.FSync}
	err := c.db.Open()
	assert(err == nil)
}

func (c *KVTestCtx) dispose() {
	c.db.Close()
	os.Remove(c.db.Path)
}

func (c *KVTestCtx) add(key string, val string) {
	err := c.db.Set([]byte(key), []byte(val))
	assert(err == nil)
	c.ref[key] = val
}

func (c *KVTestCtx) del(key string) bool {
	delete(c.ref, key)
	deleted, err := c.db.Delete([]byte(key))
	assert(err == nil)
	return deleted
}

// dump key-value pairs in B+tree
func (c *KVTestCtx) dump() ([]string, []string) {
	keys := []string{}
	vals := []string{}

	var nodeDump func(uint64)
	nodeDump = func(ptr uint64) {
		node := BNode(c.db.tree.get(ptr))
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

	nodeDump(c.db.tree.root)
	assert(keys[0] == "") // string(nil) - sentinel value
	assert(vals[0] == "") // string(nil) - sentinel value
	return keys[1:], vals[1:]
}

func (c *KVTestCtx) verify(t *testing.T) {
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
			kid := BNode(c.db.tree.get(node.getPtr(i)))
			require.Equal(t, key, kid.getKey(0))
			nodeVerify(kid)
		}
	}

	nodeVerify(c.db.tree.get(c.db.tree.root))
}

func funcTestKVBasic(t *testing.T, reopen bool) {
	c := newKVTestCtx()
	defer c.dispose()

	c.add("k", "v")
	c.verify(t)

	// insert
	for i := 0; i < 2500; i++ {
		key := fmt.Sprintf("key%d", fmix32(uint32(i)))
		val := fmt.Sprintf("val%d", fmix32(uint32(-i)))
		c.add(key, val)
		if i < 200 {
			c.verify(t)
		}
	}
	c.verify(t)
	if reopen {
		c.reopen()
		c.verify(t)
	}
	t.Log("insertion done")

	// del
	for i := 200; i < 2500; i++ {
		key := fmt.Sprintf("key%d", fmix32(uint32(i)))
		require.True(t, c.del(key))
	}
	c.verify(t)
	if reopen {
		c.reopen()
		c.verify(t)
	}
	t.Log("deletion done")

	// overwrite
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%d", fmix32(uint32(i)))
		val := fmt.Sprintf("vvv%d", fmix32(uint32(i)))
		c.add(key, val)
		c.verify(t)
	}

	require.False(t, c.del("kk"))

	// remove all
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%d", fmix32(uint32(i)))
		require.True(t, c.del(key))
		c.verify(t)
	}
	if reopen {
		c.reopen()
		c.verify(t)
	}

	c.add("k", "v2")
	c.verify(t)
	c.del("k")
	c.verify(t)
}

func TestKVBasicNoReopen(t *testing.T) {
	funcTestKVBasic(t, false)
}

func TestKVBasicWithReopen(t *testing.T) {
	funcTestKVBasic(t, true)
}

func TestKVFsyncErr(t *testing.T) {
	c := newKVTestCtx()
	defer c.dispose()

	set := c.db.Set
	get := c.db.Get

	err := set([]byte("k"), []byte("1"))
	assert(err == nil)
	val, ok := get([]byte("k"))
	assert(ok && string(val) == "1")

	c.db.FSync = fsyncErr(1)
	err = set([]byte("k"), []byte("2"))
	assert(err != nil)
	val, ok = get([]byte("k"))
	assert(ok && string(val) == "1")

	c.db.FSync = fsyncSkip
	err = set([]byte("k"), []byte("3"))
	assert(err == nil)
	val, ok = get([]byte("k"))
	assert(ok && string(val) == "3")

	c.db.FSync = fsyncErr(0, 1)
	err = set([]byte("k"), []byte("4"))
	assert(err != nil)
	val, ok = get([]byte("k"))
	assert(ok && string(val) == "3")

	c.db.FSync = fsyncSkip
	err = set([]byte("k"), []byte("5"))
	assert(err == nil)
	val, ok = get([]byte("k"))
	assert(ok && string(val) == "5")

	c.db.FSync = fsyncErr(0, 1)
	err = set([]byte("k"), []byte("6"))
	assert(err != nil)
	val, ok = get([]byte("k"))
	assert(ok && string(val) == "5")
}

func TestKVRandLength(t *testing.T) {
	c := newKVTestCtx()
	defer c.dispose()

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

func TestKVIncLength(t *testing.T) {
	for l := 1; l < BTREE_MAX_KEY_SIZE+BTREE_MAX_VAL_SIZE; l += 40 {
		c := newKVTestCtx()

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

		c.dispose()
	}
}
