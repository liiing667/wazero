package wasm

import "sync"

// MemoryQuota tracks the total number of linear memory pages shared by every
// live module instantiated in a Store. A limit of zero means the quota is
// disabled, which keeps the behavior identical to runtimes without a limit.
//
// Memory never shrinks in WebAssembly, so the counter only increases on
// instantiation and memory.grow, and decreases when the owning module closes.
// All operations are guarded by a mutex so concurrent instantiations and
// grow calls observe a single consistent view of the remaining quota.
type MemoryQuota struct {
	limitPages uint32

	// mux guards usedPages. Using a mutex (instead of atomics) makes the
	// check-and-reserve sequence in TryReserve atomic with respect to
	// concurrent reservations and releases.
	mux       sync.Mutex
	usedPages uint32
}

// NewMemoryQuota creates a MemoryQuota capped at limitPages. A limit of zero
// disables enforcement.
func NewMemoryQuota(limitPages uint32) *MemoryQuota {
	return &MemoryQuota{limitPages: limitPages}
}

// LimitPages returns the configured limit, or zero when the quota is
// disabled.
func (q *MemoryQuota) LimitPages() uint32 {
	if q == nil {
		return 0
	}
	return q.limitPages
}

// UsedPages returns the number of pages currently reserved by live modules.
func (q *MemoryQuota) UsedPages() uint32 {
	if q == nil {
		return 0
	}
	q.mux.Lock()
	defer q.mux.Unlock()
	return q.usedPages
}

// TryReserve atomically reserves pages pages against the quota. It returns
// true when the reservation fits, leaving the quota increased by pages. It
// returns false and leaves the quota untouched when the reservation would
// exceed the limit. A disabled quota always succeeds.
func (q *MemoryQuota) TryReserve(pages uint32) bool {
	if q == nil || q.limitPages == 0 || pages == 0 {
		return true
	}
	q.mux.Lock()
	defer q.mux.Unlock()
	if uint64(q.usedPages)+uint64(pages) > uint64(q.limitPages) {
		return false
	}
	q.usedPages += pages
	return true
}

// Release returns previously reserved pages to the quota. It is invoked when
// a module closes its owned memory.
func (q *MemoryQuota) Release(pages uint32) {
	if q == nil || pages == 0 {
		return
	}
	q.mux.Lock()
	defer q.mux.Unlock()
	if pages >= q.usedPages {
		q.usedPages = 0
	} else {
		q.usedPages -= pages
	}
}
