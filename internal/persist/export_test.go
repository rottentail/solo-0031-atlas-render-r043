package persist

import "errors"

// errInjectedWrite simulates a disk failure while flushing the atomic state
// file (for example ENOSPC or an I/O error during fsync/rename).
var errInjectedWrite = errors.New("injected disk write failure")

// SetWriteFault arms a fault in the on-disk commit writer. While armed, every
// Store.Update commit fails at the write stage: the candidate state has
// already passed the same full production validation as a normal commit, but
// the write returns an error and the live in-memory state is never swapped.
// The returned restore function disarms the fault and reinstates the original
// writer. The boolean result reports whether the fault was exercised.
//
// This file only exists in test binaries (the _test.go suffix excludes it
// from production builds), so the fault surface can never reach production
// code paths; production validation is not weakened or bypassed by it.
func (s *Store) SetWriteFault(armed bool) (restore func(), tripped func() bool) {
	original := s.write
	didTrip := false
	s.write = func(path string, data []byte) error {
		if armed {
			didTrip = true
			return errInjectedWrite
		}
		return original(path, data)
	}
	return func() {
			armed = false
			s.write = original
		}, func() bool {
			return didTrip
		}
}
