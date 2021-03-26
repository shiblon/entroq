package eqmem

import "time"

// modInfo contains just enough information to determine whether a task
// ID+Version exists for modification, to find its queue if needed, and to
// determine whether modification is allowed by the requester.
type modInfo struct {
	ID      uuid.UUID
	Version int32

	Queue string

	At       time.Time
	Claimant uuid.UUID

	// heapIndex is used to change things in the heap, when needed.
	heapIndex int
}

type modMap map[uuid.UUID]*modInfo

func makeModMap() modMap {
	return make(modMap)
}

func (m modMap) idQueue(id uuid.UUID) (string, bool) {
	if inf, ok := m[id]; ok {
		return inf.Queue, true
	}
	return "", false
}

func (m modMap) info(id uuid.UUID) (*modInfo, bool) {
	return m[id]
}

func (m modMap) infoIfAllowed(id uuid.UUID, version int32, claimant uuid.UUID, now time.Time) (*modInfo, bool) {
	inf, ok := m[id]
	if !ok {
		return nil, false
	}

	if inf.Version != version {
		return nil, false
	}

	if now.Before(inf.At) && inf.Claimant != claimant {
		return nil, false
	}

	return inf, true
}
