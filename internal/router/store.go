package router

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCapacity = errors.New("session capacity reached")
var ErrConflict = errors.New("request_id was already used with different input")

type entry struct {
	gate     chan struct{}
	state    Session
	lastUsed time.Time
	refs     int
}

// Store is an in-process key/value store. Per-session gates serialize turns;
// unrelated sessions can call the LLM concurrently. All reads return copies.
type Store struct {
	mu          sync.Mutex
	entries     map[string]*entry
	TTL         time.Duration
	MaxSessions int
}

func NewStore() *Store {
	return &Store{entries: map[string]*entry{}, TTL: time.Hour, MaxSessions: 1000}
}
func (s *Store) lock(ctx context.Context, id string) (*entry, func(), error) {
	s.mu.Lock()
	for k, e := range s.entries {
		if e.refs == 0 && time.Since(e.lastUsed) > s.TTL {
			delete(s.entries, k)
		}
	}
	e := s.entries[id]
	if e == nil {
		if len(s.entries) >= s.MaxSessions {
			s.mu.Unlock()
			return nil, nil, ErrCapacity
		}
		e = &entry{gate: make(chan struct{}, 1), state: Session{ID: id, Language: "ru", Identity: Values{}, Turns: []Turn{}, Stack: []*Frame{}, Queue: []*Frame{}}}
		s.entries[id] = e
	}
	e.refs++
	s.mu.Unlock()
	release := func() { s.mu.Lock(); e.refs--; e.lastUsed = time.Now(); s.mu.Unlock() }
	select {
	case e.gate <- struct{}{}:
		return e, func() { <-e.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	}
}
func (s *Store) Get(ctx context.Context, id string) (Session, bool) {
	s.mu.Lock()
	e := s.entries[id]
	if e == nil {
		s.mu.Unlock()
		return Session{}, false
	}
	e.refs++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); e.refs--; s.mu.Unlock() }()
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
		return clone(e.state), true
	case <-ctx.Done():
		return Session{}, false
	}
}
