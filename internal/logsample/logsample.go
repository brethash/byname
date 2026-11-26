// Package logsample keeps a repetitive log line from flooding the journal:
// one occurrence per interval is logged in full, carrying a count of those
// held back since, and the rest go to debug.
package logsample

import (
	"sync"
	"time"
)

// Sampler decides which occurrences of one kind of event to log in full. It
// is safe for concurrent use.
type Sampler struct {
	every time.Duration

	mu   sync.Mutex
	next time.Time
	held int
}

// New returns a sampler that lets one occurrence through every interval.
func New(every time.Duration) *Sampler { return &Sampler{every: every} }

// Take reports whether the occurrence at now should be logged in full and,
// if so, how many occurrences were held back since the last one that was.
func (s *Sampler) Take(now time.Time) (ok bool, held int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.next) {
		s.held++
		return false, 0
	}
	held, s.held = s.held, 0
	s.next = now.Add(s.every)
	return true, held
}
