package db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// KV store with a copy-on-write B+tree backed by a file
type KV struct {
	Path  string          // file name
	FSync func(int) error // overridable; for testing

	// ===== internals =====

	fd   int // file descriptor
	tree BTree
	mmap struct {
		total  int      // mmap size, can be larger than file size
		chunks [][]byte // multiple mmaps, can be non-continuous
	}
	page struct {
		flushed uint64   // DB size in number of pages
		temp    [][]byte // newly allocated pages
	}
	failed bool // did the last update failed?
}

/*
File layout:
- The DB is a single file divided into pages.
- Each page is a B+tree node, except for the 1st page.
- New nodes are appended like a log.

Meta page (1st page):
| sig | root_ptr | page_used |
| 16B |    8B    |     8B    |
. sig (signature): magic bytes to identify file type
. root_ptr: pointer to the latest root node
. page_used: number of pages

mmap():
- a way to read/write a file as if it's an in-memory buffer
- background: OS page, virtual/physical address, swapped data, page fault, dirty bit
*/

// 'BTree.get', read a page
func (db *KV) pageRead(ptr uint64) []byte {
	start := uint64(0)
	for _, chunk := range db.mmap.chunks {
		end := start + uint64(len(chunk))/BTREE_PAGE_SIZE
		if ptr < end {
			offset := BTREE_PAGE_SIZE * (ptr - start)
			return chunk[offset : offset+BTREE_PAGE_SIZE]
		}
		start = end
	}
	panic("bad ptr")
}

// 'BTree.new', allocate a new page
func (db *KV) pageAppend(node []byte) uint64 {
	ptr := db.page.flushed + uint64(len(db.page.temp)) // append
	db.page.temp = append(db.page.temp, node)
	return ptr
}

// write (append) new pages after B+tree updates
func writePages(db *KV) error {
	// extend mmap if needed
	size := (int(db.page.flushed) + len(db.page.temp)) * BTREE_PAGE_SIZE
	if err := extendMmap(db, size); err != nil {
		return err
	}

	// write data pages to the file
	offset := int64(db.page.flushed * BTREE_PAGE_SIZE)
	if _, err := unix.Pwritev(db.fd, db.page.temp, offset); err != nil {
		// pwritev() is a variant of write() that accept offset and multiple input buffers
		// . we have to control the offset since we also need to write meta page.
		//   (write() advances the file descriptor's cursor)
		return err
	}

	// discard in-memory data
	db.page.flushed += uint64(len(db.page.temp))
	db.page.temp = db.page.temp[:0]
	return nil
}

// extend mmap by adding new mappings
func extendMmap(db *KV, size int) error {
	if size <= db.mmap.total {
		return nil // enough range
	}

	// double current address space to avoid frequent remapping
	// start with 64MB (64 * 2^10 * 2^10)
	alloc := max(db.mmap.total, 64<<20) // additional
	for db.mmap.total+alloc < size {
		alloc *= 2 // grow further if not enough
	}

	chunk, err := syscall.Mmap(
		db.fd, int64(db.mmap.total), alloc,
		syscall.PROT_READ, syscall.MAP_SHARED, // shared, read-only
	)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}

	db.mmap.total += alloc
	db.mmap.chunks = append(db.mmap.chunks, chunk)
	return nil
}

const DB_SIG = "RelationalDB0123" // 16-byte string to identify file type

// | sig | root_ptr | page_used |
// | 16B |    8B    |     8B    |

// load DB metadata
func loadMeta(db *KV, data []byte) {
	db.tree.root = binary.LittleEndian.Uint64(data[16:])
	db.page.flushed = binary.LittleEndian.Uint64(data[24:])

}

// save DB metadata
func saveMeta(db *KV) []byte {
	var data [32]byte
	copy(data[:16], []byte(DB_SIG))
	binary.LittleEndian.PutUint64(data[16:], db.tree.root)
	binary.LittleEndian.PutUint64(data[24:], db.page.flushed)
	return data[:]
}

// read meta page (root + metadata)
func readRoot(db *KV, fileSize int64) error {
	if fileSize%BTREE_PAGE_SIZE != 0 {
		return errors.New("file is not multiple of pages")
	}

	if fileSize == 0 { // empty file
		db.page.flushed = 1 // meta page is initialized on 1st write
		return nil
	}

	// read
	data := db.mmap.chunks[0]
	loadMeta(db, data)

	// verify
	bad := !bytes.Equal([]byte(DB_SIG), data[:16])
	bad = bad || !(0 < db.tree.root && db.tree.root < db.page.flushed)
	maxPages := uint64(fileSize / BTREE_PAGE_SIZE)
	bad = bad || !(0 < db.page.flushed && db.page.flushed <= maxPages)
	if bad {
		return errors.New("bad meta page")
	}
	return nil
}

// update meta page (root + metadata)
func updateRoot(db *KV) error {
	if _, err := syscall.Pwrite(db.fd, saveMeta(db), 0); err != nil {
		return fmt.Errorf("write meta page: %w", err)
	}
	return nil
}

// persist changes to DB file
func updateFile(db *KV) error {
	// 1. Write new nodes
	if err := writePages(db); err != nil {
		return err
	}

	// 2. fsync to enforce order between 1 and 3
	if err := db.FSync(db.fd); err != nil {
		return err
	}

	// 3. Update the root pointer (and metadata) atomically
	if err := updateRoot(db); err != nil {
		return err
	}

	// 4. fsync to make everything persistent
	if err := db.FSync(db.fd); err != nil {
		return err
	}

	return nil
}

func updateOrRevert(db *KV, meta []byte) error {
	if db.failed { // last update failed
		// the on-disk meta page is in unknown state,
		// ensure it matches the in-memory one
		if _, err := syscall.Pwrite(db.fd, meta, 0); err != nil {
			return fmt.Errorf("rewrite meta page: %w", err)
		}
		if err := db.FSync(db.fd); err != nil {
			return err
		}
		db.failed = false
	}

	// 2-phase update
	err := updateFile(db)
	if err != nil { // revert on error
		// mark to rewrite on-disk meta page on later recovery
		db.failed = true

		// in-memory state (root + page_count) is reverted
		loadMeta(db, meta)

		// discard temporaries
		db.page.temp = db.page.temp[:0]
	}
	return err
}

// open or create a DB file
func (db *KV) Open() (err error) {
	if db.FSync == nil {
		db.FSync = syscall.Fsync
	}

	// B+tree callbacks
	db.tree.get = db.pageRead       // read a page
	db.tree.new = db.pageAppend     // append a page
	db.tree.del = func(u uint64) {} // TODO

	// open or create the DB file
	if db.fd, err = createFileSync(db.Path); err != nil {
		return err
	}

	// get file size
	finfo := syscall.Stat_t{}
	if err = syscall.Fstat(db.fd, &finfo); err != nil {
		goto fail
	}

	// create the initial mmap
	if err = extendMmap(db, int(finfo.Size)); err != nil {
		goto fail
	}

	// read the meta page
	if err = readRoot(db, finfo.Size); err != nil {
		goto fail
	}

	return nil

fail: // error
	db.Close()
	return fmt.Errorf("KV.Open: %w", err)
}

// cleanup
func (db *KV) Close() {
	for _, chunk := range db.mmap.chunks {
		err := syscall.Munmap(chunk)
		assert(err == nil)
	}
	_ = syscall.Close(db.fd)
}

// ===== Interface =====

func (db *KV) Get(key []byte) (val []byte, ok bool) {
	return db.tree.Get(key)
}

func (db *KV) Set(key []byte, val []byte) error {
	meta := saveMeta(db) // save in-memory state before update
	if err := db.tree.Insert(key, val); err != nil {
		return err
	}
	return updateOrRevert(db, meta)
}

func (db *KV) Delete(key []byte) (deleted bool, err error) {
	meta := saveMeta(db) // save in-memory state before update
	if deleted, err = db.tree.Delete(key); !deleted {
		return false, err
	}
	err = updateOrRevert(db, meta)
	return err == nil, err
}
