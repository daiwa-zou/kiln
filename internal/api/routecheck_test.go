package api

import "testing"

// Constructing the router panics if chi rejects the pattern set (for example
// the corrections wildcard sharing a subtree with the {id} param); building it
// once is the cheapest possible guard and needs no database.
func TestRouterConstructs(t *testing.T) {
	s := &Server{Writes: (*fakeWrites)(nil)}
	_ = s.Router()
}

// fakeWrites satisfies WriteStore by embedding; the router only needs a
// non-nil value to mount the human-loop routes, none of which are invoked.
type fakeWrites struct{ WriteStore }
