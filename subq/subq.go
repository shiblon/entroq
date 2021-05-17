// Package subq abstracts the idea of subscribing to a particular queue so that
// changes can be immediately notified.
package subq // import "entrogo.com/entroq/subq"

import (
	"context"
	"fmt"
	"log"
	"reflect"
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

// Wait waits on the given queues until something is notified on one or the
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
func (s *SubQ) Wait(ctx context.Context, qs []string, pollWait time.Duration, condition func() bool) error {
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

	var listeners []*sub
	for _, q := range qs {
		qi := s.addListener(q)
		defer qi.Done()
		listeners = append(listeners, qi)
	}

	for !condition() {
		// Faster version of select statement when there's exactly one listener.
		if len(listeners) == 1 {
			select {
			case <-wait():
				// Go around again, even though not signaled.
			case <-listeners[0].Ch():
				// Got a value, go around again to run condition to see if it's still there.
			case <-ctx.Done():
				return fmt.Errorf("subq wait: %w", ctx.Err())
			}
			continue
		}

		// More than one listener: create select cases dynamically. Note that
		// we can't just create one goroutine per channel and funnel them into
		// a single place because that would potentially starve other waiters
		// (we would have consumed a notification not meant for us if we pick
		// one and cancel the others). Thus, we use reflection here to generate
		// a dynamic select.
		//
		// The cases are created each time through the loop because we have to
		// call functions to get at the channels safely each time (and in some
		// cases we have to get brand new channels).
		cases := []reflect.SelectCase{
			// This must be first.
			{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(ctx.Done()),
			},
			{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(wait()),
			},
		}
		for _, listener := range listeners {
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(listener.Ch()),
			})
		}
		// Dyanmic select. If it's position 0, we know our context was canceled.
		// Otherwise, we go around again because it's either a time-based
		// release valve, or we got a notification and need to check the
		// condition again.
		chosen, _, _ := reflect.Select(cases)
		if chosen == 0 {
			// Context was canceled. Get it out and wrap its error.
			return fmt.Errorf("subq wait multi: %w", ctx.Err())
		}
		// Otherwise, we just go around again. Either the time case
		// fired, in which case it's a release valve, or a notification
		// occurred, in which case we know trying again is likely to work.
	}
	return nil
}
