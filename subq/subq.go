// Package subq abstracts the idea of subscribing to a particular queue so that
// changes can be immediately notified.
package subq

import (
	"context"
	"log"
	"sync"
	"time"
)

// SubQ is a queue subscription service. It is not public, though; it is based
// on competing consumer principles, like EntroQ itself.
type SubQ struct {
	sync.Mutex

	qs map[string]*sub
}

type sub struct {
	sync.Mutex

	listeners int
	ch        chan string
}

func (s *sub) Ch() chan string {
	if s == nil {
		return nil
	}
	defer un(lock(s))
	return s.ch
}

func (s *sub) Reserve() {
	defer un(lock(s))
	s.listeners++
}

func (s *sub) Release() {
	defer un(lock(s))
	if s.listeners == 0 {
		log.Fatal("Release before Reserve")
	}
	s.listeners--
}

func (s *sub) Reserved() bool {
	if s == nil {
		return false
	}
	defer un(lock(s))
	return s.listeners != 0
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
// the event. This function does not block, but notifies in a new goroutine.
func (s *SubQ) Notify(q string) {
	defer un(lock(s))

	qInfo := s.qs[q]

	// Do this asynchronously - we don't need nor want to wait around for the
	// cond function to run and the value to be picked up.
	go func() {
		// Because the Wait function lets go of the mutex just before selecting on
		// its channel, there is a case where we can fail to notify even though
		// it is reserved by a listener. Thus, we keep trying to send if we
		// hit the default case while the channel is reserved.
		//
		// The entire point of keeping track of reservations is to allow exactly
		// this kind of query, we just have to loop here to avoid that situation.
		for {
			select {
			case qInfo.Ch() <- q:
				return
			default:
				if !qInfo.Reserved() {
					return
				}
			}
		}
	}()
}

// Wait waits on the given queue until something is notified on it or the
// context expires, whichever comes first. This is the basic behavior when
// pollWait is 0 and cond is nil.
//
// If cond is not nil, it is called immediately. If it returns true, then the
// wait is satisfied and the function exits with a nil error. If it returns
// false, then the function begins to wait for either a notification, a context
// cancelation, or the expiration of pollWait.
//
// Note that cond should execute quickly and not block. It should test the
// condition as fast as it can and return. Otherwise "Notify" might busy-wait
// for a while, which is obviously not good.
//
// When pollWait expires or a suitable notification arrives, cond is called
// again, and the above process repeats.
//
// If pollWait is 0, the only way to check cond again is if the channel is
// notified. Otherwise Wait terminates with an error.
//
// This implementation allows you to attempt a polling operation, then wait for
// notification that the next one is likely to succeed, then check again just
// in case you got scooped by another process, repeating until something is
// truly available.
//
// Note that cond is called directly from this function, so if it needs a
// context, it can simply close over the same one passed in here.
func (s *SubQ) Wait(ctx context.Context, q string, pollWait time.Duration, cond func() bool) error {
	if cond == nil {
		// A nil cond function means, basically, "just wait once and tell me what happened".
		// The first time it's called, it returns false, then true ever after,
		// giving use the wait behavior we want below. Wrapped in an
		// immediately-called closure to avoid scope leakage.
		cond = func() func() bool {
			var done bool
			return func() bool {
				defer func() {
					done = true
				}()
				return done
			}
		}()
	}

	var qInfo *sub
	func() {
		defer un(lock(s))

		qs := s.qs

		// If there isn't any sync info for this queue, create it and add this
		// waiter. Otherwise just add the waiter; the sync info is there already.
		qInfo = qs[q]
		if qInfo != nil {
			qInfo.Reserve()
			return
		}

		qInfo = &sub{ch: make(chan string)}
		qInfo.Reserve()
		qs[q] = qInfo

		// Start up a watchdog that deletes when there are no more listeners.
		// Only do this when creating a new pInfo entry.
		go func() {
			for {
				s.Lock()
				if !qs[q].Reserved() {
					delete(qs, q)
				}
				if qs[q] == nil {
					s.Unlock()
					return
				}
				s.Unlock()
				time.Sleep(10 * time.Second)
			}
		}()
	}()
	defer qInfo.Release()

	for {
		if cond() {
			return nil
		}

		// If not waiting, after is nil, so never satisfied below.
		var after <-chan time.Time
		if pollWait > 0 {
			after = time.After(pollWait)
		}

		select {
		case <-after:
			// Go around again, even though not signaled.
		case <-qInfo.Ch():
			// Got a value, go around again to run cond to see if it's still there.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
