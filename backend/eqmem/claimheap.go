package mem

import (
	"container/heap"
	"time"
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
	items []*hItem
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

func (h *claimHeap) Items() []*hItem {
	if h == nil {
		return nil
	}
	return h.items
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

func (h *claimHeap) PushItem(item *claimItem) {
	heap.Push(h, item)
}

func (h *claimHeap) RemoveItem(item *claimItem) {
	heap.Remove(h, item.idx)
}

func (h *claimHeap) FixItem(item *claimItem) {
	heap.Fix(h, item.idx)
}
