package eqmem

import (
	"container/heap"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

type claimItem struct {
	id uuid.UUID
	q  string
	at time.Time

	idx int
}

func newItem(q string, id uuid.UUID, at time.Time) *claimItem {
	return &claimItem{
		id:  id,
		at:  at,
		q:   q,
		idx: -1,
	}
}

type claimHeap struct {
	items []*claimItem
	byID  map[uuid.UUID]*claimItem
}

func newClaimHeap() *claimHeap {
	return new(claimHeap)
}

func (h *claimHeap) Len() int {
	if h == nil {
		return 0
	}
	return len(h.items)
}

func (h *claimHeap) Items() []*claimItem {
	if h == nil {
		return nil
	}
	return h.items
}

func (h *claimHeap) Top() *claimItem {
	return h.items[0]
}

func (h *claimHeap) RandomAvailable(now time.Time) *claimItem {
	return h.randAvail(now, 0)
}

func (h *claimHeap) randAvail(now time.Time, index int) *claimItem {
	if index >= len(h.items) {
		return nil
	}

	// If this one matches, then randomly try to find one below it. If that
	// fails, return this one.
	var item *claimItem
	if candidate := h.items[index]; !now.Before(candidate.at) {
		item = candidate
	}

	// If we found one, then make a random draw less likely to produce a search
	// below this item. Otherwise we always search below this item.
	threshold := 1.0
	if item != nil {
		threshold = 0.33
	}

	next1, next2 := index*2+1, index*2+2
	if rand.Float64() < threshold {
		if candidate := h.randAvail(now, next1); candidate != nil {
			item = candidate
		}
	}

	if rand.Float64() < threshold {
		if candidate := h.randAvail(now, next2); candidate != nil {
			item = candidate
		}
	}

	return item
}

func (h *claimHeap) Less(i, j int) bool {
	return h.items[i].at.Before(h.items[j].at)
}

func (h *claimHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].idx = i
	h.items[j].idx = j
}

func (h *claimHeap) Push(x interface{}) {
	item := x.(*claimItem)
	item.idx = len(h.items)
	h.items = append(h.items, item)
}

func (h *claimHeap) Pop() interface{} {
	n := len(h.items)
	item := h.items[n-1]
	item.idx = -1
	h.items = h.items[:n-1]
	return item
}

func (h *claimHeap) FindItem(id uuid.UUID) (*claimItem, bool) {
	i, ok := h.byID[id]
	return i, ok
}

func (h *claimHeap) PushItem(item *claimItem) {
	heap.Push(h, item)
	h.byID[item.id] = item
}

func (h *claimHeap) RemoveID(id uuid.UUID) bool {
	item, ok := h.FindItem(id)
	if ok {
		h.RemoveItem(item)
	}
	return ok
}

func (h *claimHeap) RemoveItem(item *claimItem) {
	heap.Remove(h, item.idx)
	delete(h.byID, item.id)
}

func (h *claimHeap) UpdateID(id uuid.UUID, at time.Time) bool {
	item, ok := h.FindItem(id)
	if ok {
		h.UpdateItem(item, at)
	}
	return ok
}

func (h *claimHeap) UpdateItem(item *claimItem, at time.Time) {
	item.at = at
	h.FixItem(item)
}

func (h *claimHeap) FixItem(item *claimItem) {
	heap.Fix(h, item.idx)
}
