package mem

import (
	"container/heap"
	"sync"

	"entrogo.com/entroq"
)

type hItem struct {
	idx  int
	task *entroq.Task
}

func newItem(task *entroq.Task) *hItem {
	return &hItem{task: task}
}

type taskHeap struct {
	sync.RWMutex

	name  string
	items []*hItem
}

func newHeap(name string) *taskHeap {
	return &taskHeap{
		name: name,
	}
}

func (h *taskHeap) Len() int {
	if h == nil {
		return 0
	}
	return len(h.items)
}

func (h *taskHeap) Items() []*hItem {
	if h == nil {
		return nil
	}
	return h.items
}

func (h *taskHeap) Less(i, j int) bool {
	return h.items[i].task.At.Before(h.items[j].task.At)
}

func (h *taskHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].idx = i
	h.items[j].idx = j
}

func (h *taskHeap) Push(x interface{}) {
	item := x.(*hItem)
	item.idx = len(h.items)
	h.items = append(h.items, item)
}

func (h *taskHeap) Pop() interface{} {
	n := len(h.items)
	item := h.items[n-1]
	item.idx = -1
	h.items = h.items[:n-1]
	return item
}

func (h *taskHeap) PushItem(item *hItem) {
	heap.Push(h, item)
}

func (h *taskHeap) RemoveItem(item *hItem) {
	heap.Remove(h, item.idx)
}

func (h *taskHeap) FixItem(item *hItem) {
	heap.Fix(h, item.idx)
}
