package grpcserver

import (
	"sync"
	"time"
)

// StatusTracker holds timestamps updated by the poll loop, read by ControlService.GetStatus.
type StatusTracker struct {
	mu               sync.RWMutex
	lastNetboxPoll   time.Time
	lastMergeWrite   time.Time
	stalenessSeconds float64
}

func (s *StatusTracker) SetNetboxPoll(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastNetboxPoll = t
}

func (s *StatusTracker) SetMergeWrite(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMergeWrite = t
}

func (s *StatusTracker) SetStaleness(d float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalenessSeconds = d
}

// Get returns all status fields atomically.
func (s *StatusTracker) Get() (netboxPoll, mergeWrite time.Time, staleness float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastNetboxPoll, s.lastMergeWrite, s.stalenessSeconds
}
