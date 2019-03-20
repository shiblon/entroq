// Package subq abstracts the idea of subscribing to a particular queue so that
// changes can be immediately notified.
package subq // import "entrogo.com/entroq/subq"

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/pkg/errors"
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

func (s *sub) Add() {
	defer un(lock(s))
	s.listeners++
}

func (s *sub) Done() {
	defer un(lock(s))
	if s.listeners == 0 {
		log.Fatal("Done before Add")
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

	// Drop if nobody is listening.
	if !qInfo.Reserved() {
		return
	}

	// Do this asynchronously - we don't need nor want to wait around for the
	// condition function to run and the value to be picked up. Note that the lock
	// is not held in this goroutine.
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
			case <-time.After(500 * time.Millisecond):
				if !qInfo.Reserved() {
					return
				}
			}
		}
	}()
}

// makeDefaultCondition creates a condition function that returns false once,
// then true. This will cause the Wait function to wait exactly one time for a
// notification, then exit.
func makeDefaultCondition() func() bool {
	var done bool
	return func() bool {
		val := done
		done = true
		return val
	}
}

// tryCollect attempts to collect an unused queue listener, returning true if
// it was able to do it.
func (s *SubQ) tryCollect(q string) bool {
	defer un(lock(s))
	if !s.qs[q].Reserved() {
		delete(s.qs, q)
		return true
	}
	return false
}

// addListener returns the requested queue wait channel and reservation mechanism,
// creating one if not present. If it creates one, it also starts up a garbage
// collection routine for it.
func (s *SubQ) addListener(q string) *sub {
	defer un(lock(s))

	qs := s.qs

	// If there isn't any sync info for this queue, create it and add this
	// waiter. Otherwise just add the waiter; the sync info is there already.
	if qs[q] != nil {
		qs[q].Add()
		return qs[q]
	}

	qs[q] = &sub{ch: make(chan string)}
	qs[q].Add()

	// Start up a watchdog that deletes when there are no more listeners.
	// Only do this when creating a new queue listener entry.
	go func() {
		for !s.tryCollect(q) {
			time.Sleep(1 * time.Second)
		}
	}()
	return qs[q]
}

// Wait waits on the given queue until something is notified on it or the
// context expires, whichever comes first. This is the basic behavior when
// pollWait is 0 and condition is nil.
//
// If condition is not nil, it is called immediately. If it returns true, then the
// wait is satisfied and the function exits with a nil error. If it returns
// false, then the function begins to wait for either a notification, a context
// cancelation, or the expiration of pollWait.
//
// Note that condition should execute quickly and not block. It should test the
// condition as fast as it can and return. Otherwise "Notify" might busy-wait
// for a while, which is obviously not good.
//
// When pollWait expires or a suitable notification arrives, condition is called
// again, and the above process repeats.
//
// If pollWait is 0, the only way to check condition again is if the channel is
// notified. Otherwise Wait terminates with an error.
//
// This implementation allows you to attempt a polling operation, then wait for
// notification that the next one is likely to succeed, then check again just
// in case you got scooped by another process, repeating until something is
// truly available.
//
// Note that condition is called directly from this function, so if it needs a
// context, it can simply close over the same one passed in here.
func (s *SubQ) Wait(ctx context.Context, q string, pollWait time.Duration, condition func() bool) error {
	if condition == nil {
		condition = makeDefaultCondition()
	}

	wait := func() <-chan time.Time {
		if pollWait > 0 {
			return time.After(pollWait)
		}
		// Zero wait means wait forever.
		return nil
	}

	qi := s.addListener(q)
	defer qi.Done()

	for !condition() {
		select {
		case <-wait():
			log.Printf("SubQ loop release valve")
			// Go around again, even though not signaled.
		case <-qi.Ch():
			log.Printf("SubQ notified")
			// Got a value, go around again to run condition to see if it's still there.
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "subq wait")
		}
	}
	return nil
}
