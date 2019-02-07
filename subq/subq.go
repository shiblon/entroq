// Package subq abstracts the idea of subscribing to a particular queue so that
// changes can be immediately notified.
package subq

import (
	"context"
	"sync"
)

// SubQ is a queue subscription service. It is not public, though; it is based
// on competing consumer principles, like EntroQ itself.
type SubQ struct {
	sync.Mutex

	qs map[string]*sub
}

type sub struct {
	sync.Mutex
	sync.WaitGroup
	ch chan string
}

func (s *sub) Ch() chan string {
	if s == nil {
		return nil
	}
	return s.ch
}

func lock(l sync.Locker) func() {
	l.Lock()
	return l.Unlock
}

func un(f func()) {
	f()
}

// New creates a new queue competing-consumer subscription service.
func New() *SubQ {
	return &SubQ{
		qs: make(map[string]*sub),
	}
}

// Notify sends notifications to at most one waiting goroutines that something
// is ready on the given queue. If nobody is listening, it immediately drops
// the event.
func (s *SubQ) Notify(q string) {
	defer un(lock(s))

	select {
	case s.qs[q].Ch() <- q:
	default:
	}
}

// Wait waits on the given queue until something is notified on it or the
// context expires, whichever comes first. You should always call this
// function assuming it might block forever *even if the queue is notified*.
func (s *SubQ) Wait(ctx context.Context, q string) error {
	var qInfo *sub
	func() {
		defer un(lock(s))

		qs := s.qs

		// If there isn't any sync info for this queue, create it and add this
		// waiter. Otherwise just add the waiter; the sync info is there already.
		qInfo = qs[q]
		if qInfo != nil {
			qInfo.Add(1)
			return
		}

		qInfo = &sub{ch: make(chan string)}
		qInfo.Add(1)
		qs[q] = qInfo

		// In the backgound, wait until there are no more subscribers and
		// delete the entry for this queue.
		g := qs[q]
		go func() {
			g.Wait()

			defer un(lock(s))
			delete(qs, q)
		}()
	}()
	defer qInfo.Done()

	// Since we have already added our intent to listen on this channel,
	// it won't get deleted from the queue map before the select executes
	// below.

	select {
	case <-qInfo.Ch():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
