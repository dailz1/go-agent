package mcpbridge

import "sync"

// bridgeRejections holds the names excluded by the tool filter for the
// lifetime of one Bridge. It is shared between the projection paths, the
// Bridge summary, and the forwarding wrappers (whose Execute consults it so
// calls admitted before a rejection fail softly). Safe for concurrent use.
type bridgeRejections struct {
	mu      sync.RWMutex
	m       map[string]struct{}
	skipped int64
	total   int64
}

func newBridgeRejections() *bridgeRejections {
	return &bridgeRejections{m: make(map[string]struct{})}
}

// retained reports whether the name was rejected at any earlier listing.
func (r *bridgeRejections) retained(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.m[name]
	return ok
}

// add records a newly rejected name and counts it against the current
// listing.
func (r *bridgeRejections) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = struct{}{}
	r.skipped++
}

// beginListing resets the per-listing skipped counter and records the
// listing size for the filter summary.
func (r *bridgeRejections) beginListing(total int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total = total
	r.skipped = 0
}

// summary reports the filter outcome of the most recent listing.
func (r *bridgeRejections) summary() (skipped int64, total int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skipped, r.total
}
