package db

import (
	"encoding/binary"
)

// free-list's node format (consume a page itself)
// | next | pointers | unused |
// | 8B   | n * 8B	 | ...	  |
// . next: pointer to next node
// . pointers: pointers to free pages
type LNode []byte

const FREE_LIST_HEADER = 8
const FREE_LIST_CAP = (BTREE_PAGE_SIZE - FREE_LIST_HEADER) / 8

// ===== getters & setters =====

func (node LNode) getNext() uint64 {
	return binary.LittleEndian.Uint64(node[0:8])
}

func (node LNode) setNext(next uint64) {
	binary.LittleEndian.PutUint64(node[0:8], next)
}

func (node LNode) getPtr(idx int) uint64 {
	offset := FREE_LIST_HEADER + idx*8
	return binary.LittleEndian.Uint64(node[offset:])
}

func (node LNode) setPtr(idx int, ptr uint64) {
	assert(idx < FREE_LIST_CAP)
	offset := FREE_LIST_HEADER + idx*8
	binary.LittleEndian.PutUint64(node[offset:], ptr)
}

type FreeList struct {
	// ===== callbacks for managing on-disk pages =====

	get func(uint64) []byte // read a page
	new func([]byte) uint64 // append a new page
	set func(uint64) []byte // returns a writable buffer to capture in-place update

	// ===== persisted data in meta page =====

	headPage uint64 // pointer to list head node
	headSeq  uint64 // monotonic sequence number to index into list head
	tailPage uint64
	tailSeq  uint64

	// ===== in-memory states =====

	maxSeq uint64 // saved 'tailSeq' to prevent consuming newly added items
}

// wrapped-around index from sequence number
//   - 2 sequence numbers are monotonically increasing
//   - to prevent list head from overrunning list tail,
//     just compare the sequence numbers
func seq2idx(seq uint64) int {
	return int(seq % FREE_LIST_CAP)
}

// ensure that:
//   - free-list doesn't use meta page (page 0).
//   - 'headSeq' doesn't overrun 'tailSeq'
//     (headSeq == tailSeq -> empty list).
func (fl *FreeList) check() {
	assert(fl.headPage != 0 && fl.tailPage != 0)
	assert(fl.headSeq != fl.tailSeq || fl.headPage == fl.tailPage)
}

// get 1 item from head; return 0 on failure
func (fl *FreeList) PopHead() uint64 {
	ptr, head := flPop(fl)
	if head != 0 { // recycle empty head node
		fl.PushTail(head)
	}
	return ptr
}

// remove 1 item from head; remove head if it becomes empty;
// returns:
// - pointer to a free page (if found)
// - removed head (if removed)
func flPop(fl *FreeList) (ptr uint64, head uint64) {
	fl.check()
	if fl.headSeq == fl.maxSeq {
		return 0, 0 // cannot advance
	}

	node := LNode(fl.get(fl.headPage))
	ptr = node.getPtr(seq2idx(fl.headSeq))
	fl.headSeq++

	// remove head if it becomes empty
	if seq2idx(fl.headSeq) == 0 { // wrapped-around
		head, fl.headPage = fl.headPage, node.getNext()
		assert(fl.headPage != 0)
	}
	return
}

// add 1 item to tail
func (fl *FreeList) PushTail(ptr uint64) {
	fl.check()

	// add to tail node
	LNode(fl.set(fl.tailPage)).setPtr(seq2idx(fl.tailSeq), ptr)
	fl.tailSeq++

	// if tail node is full, add a new empty tail node
	// (ensure there's at list 1 node in free-list,
	//  in case current tail node is removed as head node)
	if seq2idx(fl.tailSeq) == 0 { // wrapped-around
		// try to reuse from head
		next, head := flPop(fl) // may remove the head node

		if next == 0 {
			// no available items -> allocate new node
			next = fl.new(make([]byte, BTREE_PAGE_SIZE))
		}

		// link to new tail node
		LNode(fl.set(fl.tailPage)).setNext(next)
		fl.tailPage = next

		// also add the head node if it's removed
		if head != 0 {
			LNode(fl.set(fl.tailPage)).setPtr(0, head)
			fl.tailSeq++
		}
	}
}

// - at the beginning of an update, save the current tailSeq to maxSeq
// - during the update, don't let headSeq overrun maxSeq
// - after that, maxSeq is advanced to tailSeq
//
// make the newly added items available for consumption
func (fl *FreeList) SetMaxSeq() {
	fl.maxSeq = fl.tailSeq
}
