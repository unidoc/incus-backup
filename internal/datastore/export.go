package datastore

import "time"

// SetClockForTest replaces the store's clock (tests of GC grace and verify age).
func (s *Store) SetClockForTest(f func() time.Time) { s.now = f }
