package db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// KV store backed by a file
type KV struct {
	Path  string
	Fsync func(int) error // overridable; for testing

	// === internals ===

	fd   int // file descriptor
	tree BTree
	free FreeList
	mmap struct {
		total  int      // mmap size, can be larger than file size
		chunks [][]byte // multiple mmaps, can be non-continuous
	}
	page struct {
		flushed uint64            // DB size in number of pages
		nappend uint64            // number of pages to be appended
		updates map[uint64][]byte // pending updates, including appended pages
	}
	failed bool // did the last update failed?
}

/*
File layout:
- The DB is a single file divided into pages.
- Each page is a B+tree node, except for the 1st page.
- New nodes are appended like a log.

OS background:
- OS page is the minimum unit for mapping between virtual and physical address.
- The virtual address space of a process is not fully backed by physical memory.
  Part of that can be swapped to disk, and when the process tries to access it:
  . the CPU triggers a page fault, which hands control to the OS.
  . the OS reads the swapped data into physical memory,
    remaps the virtual address to it,
	then hands control back to the process.
  . the process resumes with the virtual address mapped to real RAM.
- The CPU also sets a dirty bit when the process modifies a page,
  so the OS can write the page back to disk later;
  fsync() is used to request and wait for the IO.

mmap():
- a way to read/write a file as if it's an in-memory buffer;
  disk IO is implicit and automatic with mmap().
- How it works (see OS background notes):
  . the process gets an address range from mmap().
  . read: when the process touches a page in that range, it may triggers page fault,
    and the OS reads data into page cache and remaps the page to the cache.
  . write: write to the page cache, set the dirty bit,
    and the OS will write the page to disk later (automatic, or when fsync()).
*/

// 'BTree.get', read a page
func (db *KV) pageRead(ptr uint64) []byte {
	assert(ptr < db.page.flushed+db.page.nappend)
	if node, ok := db.page.updates[ptr]; ok {
		return node // pending update
	}
	return db.pageReadFile(ptr)
}

func (db *KV) pageReadFile(ptr uint64) []byte {
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

// 'BTree.new', allocate a free page
func (db *KV) pageAlloc(node []byte) uint64 {
	if ptr := db.free.PopHead(); ptr != 0 { // try free-list
		db.page.updates[ptr] = node
		return ptr
	}
	return db.pageAppend(node) // append
}

// 'FreeList.new', append a new page
func (db *KV) pageAppend(node []byte) uint64 {
	assert(len(node) == BTREE_PAGE_SIZE)
	ptr := db.page.flushed + db.page.nappend
	db.page.nappend++
	assert(db.page.updates[ptr] == nil)
	db.page.updates[ptr] = node
	return ptr
}

// 'FreeList.set', returns a writable copy to capture in-place updates
func (db *KV) pageWrite(ptr uint64) []byte {
	assert(ptr < db.page.flushed+db.page.nappend)
	if node, ok := db.page.updates[ptr]; ok {
		return node // pending update
	}

	node := make([]byte, BTREE_PAGE_SIZE)

	if !(db.page.flushed == 2 && ptr == 1) {
		// initialized from file
		copy(node, db.pageReadFile(ptr))
	}
	// else (db.page.flushed == 2 && ptr == 1):
	// - Happen the 1st time the B+tree root is split.
	//   The original root is deallocated and appended to free-list's tail
	//   But page 1 (current tail page) has not been written to file after creating an empty DB
	//   (see KV.Open() -> readRoot() -> fileSize == 0 case)
	// - Both FreeList pages and B+tree pages are written to disk when commit.
	//   (see KV.Commit() -> ... -> writePages() -> write db.page.updates)

	db.page.updates[ptr] = node
	return node
}

// write (append) new pages after B+tree updates
func writePages(db *KV) error {
	// extend mmap if needed
	size := (int(db.page.flushed + db.page.nappend)) * BTREE_PAGE_SIZE
	if err := extendMmap(db, size); err != nil {
		return err
	}

	// write data pages to the file
	for ptr, node := range db.page.updates {
		offset := int64(ptr * BTREE_PAGE_SIZE)
		if _, err := unix.Pwrite(db.fd, node, offset); err != nil {
			return err
		}
	}

	// discard in-memory data
	db.page.flushed += db.page.nappend
	db.page.nappend = 0
	db.page.updates = map[uint64][]byte{}
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

/*
Meta page (page 0):
| sig | root_ptr | page_used | head_page | head_seq | tail_page | tail_seq | (unused) |
| 16B |    8B    |     8B    | 8B		 | 8B		| 8B		| 8B	   | ...	  |
. sig (signature): magic bytes to identify file type
. root_ptr: pointer to the latest root node
. page_used: number of pages
. head/tail page/seq: see FreeList struct
*/

const DB_SIG = "RelationalDB0123" // 16-byte string to identify file type

// load DB metadata
func loadMeta(db *KV, data []byte) {
	db.tree.root = binary.LittleEndian.Uint64(data[16:24])
	db.page.flushed = binary.LittleEndian.Uint64(data[24:32])
	db.free.headPage = binary.LittleEndian.Uint64(data[32:40])
	db.free.headSeq = binary.LittleEndian.Uint64(data[40:48])
	db.free.tailPage = binary.LittleEndian.Uint64(data[48:56])
	db.free.tailSeq = binary.LittleEndian.Uint64(data[56:64])
}

// save DB metadata
func saveMeta(db *KV) []byte {
	var data [64]byte
	copy(data[:16], []byte(DB_SIG))
	binary.LittleEndian.PutUint64(data[16:24], db.tree.root)
	binary.LittleEndian.PutUint64(data[24:32], db.page.flushed)
	binary.LittleEndian.PutUint64(data[32:40], db.free.headPage)
	binary.LittleEndian.PutUint64(data[40:48], db.free.headSeq)
	binary.LittleEndian.PutUint64(data[48:56], db.free.tailPage)
	binary.LittleEndian.PutUint64(data[56:64], db.free.tailSeq)
	return data[:]
}

// read meta page (root + metadata)
func readRoot(db *KV, fileSize int64) error {
	if fileSize == 0 { // empty file
		// reserve 2 pages: meta page and a free-list node
		db.page.flushed = 2
		// add initial node to free-list (ensure at least 1 node)
		db.free.headPage = 1
		db.free.tailPage = 1
		return nil // meta page will be written in the 1st update
	}

	if fileSize%BTREE_PAGE_SIZE != 0 {
		return errors.New("file is not multiple of pages")
	}

	// read
	data := db.mmap.chunks[0]
	loadMeta(db, data)

	// initialize free-list
	db.free.SetMaxSeq()

	// verify
	bad := !bytes.Equal([]byte(DB_SIG), data[:16])

	maxPages := uint64(fileSize / BTREE_PAGE_SIZE)
	bad = bad || !(0 < db.page.flushed && db.page.flushed <= maxPages)
	bad = bad || !(0 < db.tree.root && db.tree.root < db.page.flushed)
	bad = bad || !(0 < db.free.headPage && db.free.headPage < db.page.flushed)
	bad = bad || !(0 < db.free.tailPage && db.free.tailPage < db.page.flushed)

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
	// 1. write new nodes
	if err := writePages(db); err != nil {
		return err
	}

	// 2. fsync to enforce order between 1 and 3
	if err := db.Fsync(db.fd); err != nil {
		return err
	}

	// 3. update the root pointer (and metadata) atomically
	if err := updateRoot(db); err != nil {
		return err
	}

	// 4. fsync to make everything persistent
	if err := db.Fsync(db.fd); err != nil {
		return err
	}

	// prepare the free-list for next update
	db.free.SetMaxSeq()

	return nil
}

func updateOrRevert(db *KV, meta []byte) error {
	if db.failed { // last update failed
		// the on-disk meta page is in unknown state,
		// ensure it matches the in-memory one
		if _, err := syscall.Pwrite(db.fd, meta, 0); err != nil {
			return fmt.Errorf("rewrite meta page: %w", err)
		}
		if err := db.Fsync(db.fd); err != nil {
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
		db.page.nappend = 0
		db.page.updates = map[uint64][]byte{}
	}
	return err
}

// open or create a DB file
func (db *KV) Open() (err error) {
	if db.Fsync == nil {
		db.Fsync = syscall.Fsync
	}

	db.page.updates = map[uint64][]byte{}

	// B+tree callbacks
	db.tree.get = db.pageRead      // read a page
	db.tree.new = db.pageAlloc     // reuse from free-list or append
	db.tree.del = db.free.PushTail // freed pages go to free-list

	// free-list callbacks
	db.free.get = db.pageRead   // read a page
	db.free.new = db.pageAppend // append a page
	db.free.set = db.pageWrite  // in-place updates

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
