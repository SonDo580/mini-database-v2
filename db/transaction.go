package db

import (
	"bytes"
	"errors"
	"slices"
)

// start <= key <= stop
type KeyRange struct {
	start []byte
	stop  []byte
}

// KV transaction
type KVTX struct {
	snapshot BTree  // read-only snapshot
	version  uint64 // based on KV.version

	pending BTree      // in-memory tree to capture KV updates
	reads   []KeyRange // involved intervals of keys for detecting conflicts

	updateAttempted bool // should check for update even if update changes nothing
	done            bool // transaction already committed or rolled-back
}

// for reverting updates by a single statement inside a transaction
type TXSaved struct {
	root  uint64
	reads []KeyRange
}

// save state before executing statement
func (tx *KVTX) Save(saved *TXSaved) {
	saved.root = tx.pending.root
	saved.reads = tx.reads
}

// revert any updates by the statement
func (tx *KVTX) Revert(saved *TXSaved) {
	tx.pending.root = saved.root
	tx.reads = saved.reads
}

// return True if version a < version b
func versionBefore(a, b uint64) bool {
	return a < b
	// TODO: handle wraparounds
	// return a - b > (1 << 63)
}

// prefix for values in KVTX.pending
const (
	FLAG_DELETED = byte(1) // deleted
	FLAG_UPDATED = byte(2) // inserted/updated
)

// begin a transaction
func (kv *KV) Begin(tx *KVTX) {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	// read-only snapshot
	tx.snapshot.root = kv.tree.root
	tx.version = kv.version
	chunks := kv.mmap.chunks // copied current mmap to avoid updates from writers
	tx.snapshot.get = func(ptr uint64) []byte {
		return mmapRead(ptr, chunks)
	}
	// Why shallow copy of 'kv.mmap.chunks' slice (ptr,len,cap) is enough:
	// - a commit may modify 'kv.mmap.chunks', but it only appends to it.
	// - free-list doesn't allow items still in use by readers.

	// in-memory tree to capture updates
	// - adjust callbacks to avoid ptr = 0
	pages := [][]byte(nil)
	tx.pending.get = func(ptr uint64) []byte {
		return pages[ptr-1]
	}
	tx.pending.new = func(node []byte) uint64 {
		pages = append(pages, node)
		return uint64(len(pages))
	}
	tx.pending.del = func(uint64) {}

	// keep track of concurrent TXs
	kv.ongoing = append(kv.ongoing, tx.version)
}

var ErrorConflict = errors.New("cannot commit due to conflict")

// end a transaction: commit updates, rollback on error
func (kv *KV) Commit(tx *KVTX) error {
	assert(!tx.done)
	tx.done = true

	// only 1 writer at a time
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	defer txFinalize(kv, tx)
	// ('defer' statements is run in LIFO order)

	// error if conflicts
	if tx.updateAttempted && detectConflicts(kv, tx) {
		return ErrorConflict
	}

	// save meta data
	meta := saveMeta(kv)
	root := kv.tree.root

	// update free-list's current version
	kv.free.curVer = kv.version + 1 // expected value if commit is successful

	// transfer updates to current tree
	writes := []KeyRange(nil) // collect updated ranges in this TX
	for iter := tx.pending.Seek(nil, CMP_GT); iter.Valid(); iter.Next() {
		modified := false
		key, val := iter.Deref()
		oldVal, oldFound := tx.snapshot.Get(key)

		switch val[0] {
		case FLAG_DELETED:
			modified = oldFound
			deleted, err := kv.tree.Delete(&DeleteReq{Key: key})
			if err != nil {
				return err
			}
			assert(deleted == modified)
		case FLAG_UPDATED: // insert/update
			modified = (!oldFound || !bytes.Equal(oldVal, val[1:]))
			updated, err := kv.tree.Update(&UpdateReq{Key: key, Val: val[1:]})
			if err != nil {
				return err
			}
			assert(updated == modified)
		default:
			panic("unreachable")
		}

		if modified && len(kv.ongoing) > 1 {
			writes = append(writes, KeyRange{start: key, stop: key})
		}
		// len(kv.ongoing) == 1:
		// - no other concurrent transactions running.
		//   -> don't need to capture writes to resolve conflict.
		// - a TX is added to 'kv.ongoing' on Begin(),
		//   but the current writer is still holding the 'kv.mutex' lock,
		//   so 'kv.ongoing' will not changed during this time
	}

	// commit the update
	if kv.tree.root != root {
		kv.version++
		if err := updateOrRevert(kv, meta); err != nil {
			return err
		}
	}

	// save updated ranges by just-committed TX to history
	if len(writes) > 0 {
		// sort the ranges for easier overlap detection
		slices.SortFunc(writes, keyRangeCmp)

		kv.history = append(kv.history, CommittedTX{
			version: kv.version, writes: writes,
		})
		// each committed TX increments 'kv.version'
		// -> 'kv.history' is sorted in 'version ASC'
	}

	return nil
}

func keyRangeCmp(r1, r2 KeyRange) int {
	return bytes.Compare(r1.start, r2.start)
}

// return True if current TX's dependency ranges (tx.reads)
// overlaps with committed changes of a newer version
func detectConflicts(kv *KV, tx *KVTX) bool {
	// sort dependency ranges for easier overlap detection
	slices.SortFunc(tx.reads, keyRangeCmp)

	// check overlap with committed newer versions
	for i := len(kv.history) - 1; i >= 0; i-- {
		if !versionBefore(tx.version, kv.history[i].version) {
			// reached an older version than current TX's version
			break // kv.history is in 'version ASC' order
		}

		if sortedRangesOverlap(tx.reads, kv.history[i].writes) {
			return true
		}
	}

	return false
}

func sortedRangesOverlap(r1, r2 []KeyRange) bool {
	var i1, i2 int // = 0
	for i1 < len(r1) && i2 < len(r2) {
		if bytes.Compare(r1[i1].stop, r2[i2].start) < 0 {
			i1++
		} else if bytes.Compare(r2[i2].stop, r1[i1].start) < 0 {
			i2++
		} else {
			return true
		}
	}
	return false
}

// common routines when exiting a transaction
func txFinalize(kv *KV, tx *KVTX) {
	// still holding the 'kv.mutex' lock

	// remove TX from 'kv.ongoing'
	idx := slices.Index(kv.ongoing, tx.version)
	assert(idx != -1)
	kv.ongoing = slices.Delete(kv.ongoing, idx, idx+1)

	// find oldest in-use version
	minVersion := kv.version
	for _, other := range kv.ongoing {
		if versionBefore(other, minVersion) {
			minVersion = other
		}
	}

	// release the free list
	kv.free.SetMaxVer(minVersion)

	// trim 'kv.history' older than oldest in-use version
	// ('kv.history' is in 'version ASC' order)
	for idx = 0; idx < len(kv.history); idx++ {
		if versionBefore(minVersion, kv.history[idx].version) {
			break
		}
	}
	kv.history = kv.history[idx:]
}

// end a transaction: rollback
func (kv *KV) Abort(tx *KVTX) {
	assert(!tx.done)
	tx.done = true

	kv.mutex.Lock()
	txFinalize(kv, tx)
	kv.mutex.Unlock()
}

type KVIter interface {
	Deref() (key []byte, val []byte)
	Valid() bool
	Next()
}

// iterator that combines pending updates and snapshot
// (implement KVIter interface)
type CombinedIter struct {
	top       *BIter // KVTX.pending
	bottom    *BIter // KVTX.snapshot
	direction int    // +1 for >= or >, -1 for <= or <
	endKey    []byte
	endCmp    int
}

// get current KV
func (iter *CombinedIter) Deref() ([]byte, []byte) {
	var k1, v1, k2, v2 []byte
	topValid, bottomValid := iter.top.Valid(), iter.bottom.Valid()
	assert(topValid || bottomValid)
	if topValid {
		k1, v1 = iter.top.Deref()
	}
	if bottomValid {
		k2, v2 = iter.bottom.Deref()
	}

	// pick the smaller/larger key of the 2, or the only valid key.
	// prioritize top over bottom if k1 == k2.
	//
	// . k1 < k2 -> bytes.Compare(k1, k2) = -1
	//   direction = -1 (DESC) -> pick larger key (k2)
	//	 direction = 1 (ASC) -> pick smaller key (k1)
	// . k1 > k2 -> bytes.Compare(k1, k2) = 1
	//   direction = -1 (DESC) -> pick larger key (k1)
	//	 direction = 1 (ASC) -> pick smaller key (k2)
	// . k1 == k2 -> bytes.Compare(k1, k2) = 0
	//   -> always != iter.direction -> pick k1
	if topValid && bottomValid && bytes.Compare(k1, k2) == iter.direction {
		return k2, v2
	}
	if topValid {
		// topValid && bottomValid && bytes.Compare(k1, k2) != iter.direction
		// || topValid && !bottomValid
		return k1, v1
	} else {
		// bottomValid && !topValid
		return k2, v2
	}
}

func (iter *CombinedIter) Valid() bool {
	if !iter.top.Valid() && !iter.bottom.Valid() {
		return false
	}
	key, _ := iter.Deref()
	return cmpOK(key, iter.endCmp, iter.endKey) // still in range
}

func (iter *CombinedIter) Next() {
	// which B+tree iterator to move?
	// - if 2 keys are equal, move both.
	// - if there's only 1 valid iterator, move it.
	// - otherwise, move the iterator that contains current KV.
	topValid, bottomValid := iter.top.Valid(), iter.bottom.Valid()
	topMove, bottomMove := topValid, bottomValid
	if topValid && bottomValid {
		k1, _ := iter.top.Deref()
		k2, _ := iter.bottom.Deref()
		switch bytes.Compare(k1, k2) {
		case -iter.direction:
			// (k1 < k2 && direction = ASC) || (k1 > k2 && direction == DESC)
			// -> currently at k1 -> move top
			topMove, bottomMove = true, false
		case iter.direction:
			// (k1 < k2 && direction = DESC) || (k1 > k2 && direction == ASC)
			// -> currently at k2 -> move bottom
			topMove, bottomMove = false, true
		case 0: // equal -> move both
		}
	}
	assert(topMove || bottomMove)

	// move B+tree iterators w.r.t. direction
	if topMove {
		if iter.direction > 0 {
			iter.top.Next()
		} else {
			iter.top.Prev()
		}
	}
	if bottomMove {
		if iter.direction > 0 {
			iter.bottom.Next()
		} else {
			iter.bottom.Prev()
		}
	}
}

// === KV interface ===

// point query; check pending updates before snapshot (read your own write)
func (tx *KVTX) Get(key []byte) (val []byte, ok bool) {
	// add dependency range
	tx.reads = append(tx.reads, KeyRange{start: key, stop: key})

	val, ok = tx.pending.Get(key) // check pending updates
	switch {
	case ok && val[0] == FLAG_UPDATED: // updated in this TX
		return val[1:], true
	case ok && val[0] == FLAG_DELETED: // deleted in this TX
		return nil, false
	case !ok: // read from snapshot
		return tx.snapshot.Get(key)
	default:
		panic("unreachable")
	}
}

// (>=, > to 1; <=, < to -1)
func cmp2dir(cmp int) int {
	if cmp > 0 {
		return 1
	} else if cmp < 0 {
		return -1
	}
	panic("unreachable")
}

// range query: combined pending updates with snapshot
func (tx *KVTX) Seek(
	key1 []byte, cmp1 int, key2 []byte, cmp2 int,
) KVIter {
	direction := cmp2dir(cmp1)
	assert(direction != cmp2dir(cmp2))

	// add dependency range
	low, high := key1, key2
	if direction < 0 {
		low, high = key2, key1
	}
	tx.reads = append(tx.reads, KeyRange{start: low, stop: high})

	return &CombinedIter{
		top:       tx.pending.Seek(key1, cmp1),
		bottom:    tx.snapshot.Seek(key1, cmp1),
		direction: direction,
		endKey:    key2,
		endCmp:    cmp2,
	}
}

func (tx *KVTX) Update(req *UpdateReq) (bool, error) {
	tx.updateAttempted = true

	// check if need update
	currVal, exists := tx.Get(req.Key) // also add dependency range
	if (req.Mode == MODE_UPDATE_ONLY && !exists) ||
		(req.Mode == MODE_INSERT_ONLY && exists) ||
		(exists && bytes.Equal(currVal, req.Val)) {
		return false, nil
	}

	req.Old = currVal
	// capture pending update
	updated, err := tx.pending.Update(&UpdateReq{
		Key: req.Key,
		Val: append([]byte{FLAG_UPDATED}, req.Val...), // flagged
	})
	if err != nil {
		return false, err
	}
	assert(updated)

	req.Updated = true
	req.Added = !exists
	return true, nil
}

func (tx *KVTX) Set(key []byte, val []byte) (bool, error) {
	return tx.Update(&UpdateReq{Key: key, Val: val})
}

func (tx *KVTX) Del(req *DeleteReq) (bool, error) {
	tx.updateAttempted = true

	currVal, exists := tx.Get(req.Key) // also add dependency range
	if !exists {
		return false, nil
	}

	req.Old = currVal
	// capture pending update
	updated, err := tx.pending.Update(&UpdateReq{
		Key: req.Key,
		Val: []byte{FLAG_DELETED}, // flagged as deleted
	})
	if err != nil {
		return false, err
	}
	assert(updated)

	return true, nil
}
