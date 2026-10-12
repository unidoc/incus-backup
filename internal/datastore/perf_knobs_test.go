package datastore

import "errors"

// setBenchParallel sets the request parallelism of a store for a benchmark.
func setBenchParallel(s *Store, par int) { s.SetRequestParallelism(par) }

// setBenchPresence switches the presence set on and loads it.
func setBenchPresence(s *Store) error {
	s.SetPresenceCache(true)
	return s.RebuildPresence()
}

// listShallow forwards the sharded listing of the wrapped backend (so it is measured).
func (c *countingBackend) listShallow(prefix string) ([]listedObject, []string, error) {
	sl, ok := c.Backend.(shallowLister)
	if !ok {
		return nil, nil, errors.New("not supported")
	}
	c.lists.Add(1)
	return sl.listShallow(prefix)
}
