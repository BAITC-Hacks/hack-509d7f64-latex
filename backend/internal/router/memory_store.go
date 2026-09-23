package router

import (
	"context"
	"sync"
	"time"
)

// memoryRepo is an in-process Repository for tests and the offline harness.
// Sessions, runs and the synthetic backend live in maps behind one mutex; a
// per-session gate serialises turns while unrelated sessions run concurrently.
// Nothing survives a restart.
type memoryRepo struct {
	mu       sync.Mutex
	sessions map[string]Session
	runs     map[string]map[string]Run
	order    map[string][]string
	gates    map[string]chan struct{}
	backend  *Backend
}

type memoryLease struct {
	repo   *memoryRepo
	id     string
	gate   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

// NewMemoryRepository returns an in-memory Repository with its own copy of the
// synthetic backend seeded from the catalog.
func NewMemoryRepository(c *Catalog) Repository {
	return &memoryRepo{sessions: map[string]Session{}, runs: map[string]map[string]Run{}, order: map[string][]string{}, gates: map[string]chan struct{}{}, backend: NewBackend(c)}
}

func (m *memoryRepo) Ready(context.Context) error { return nil }

func (m *memoryRepo) Lock(ctx context.Context, id string) (Lease, error) {
	m.mu.Lock()
	gate := m.gates[id]
	if gate == nil {
		gate = make(chan struct{}, 1)
		m.gates[id] = gate
		m.sessions[id] = Session{ID: id, Language: "ru", Identity: Values{}, Turns: []Turn{}, Stack: []*Frame{}, Queue: []*Frame{}}
		m.runs[id] = map[string]Run{}
	}
	m.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	own, cancel := context.WithCancel(context.Background())
	return &memoryLease{repo: m, id: id, gate: gate, ctx: own, cancel: cancel}, nil
}

func (m *memoryRepo) Get(ctx context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	s = clone(s)
	s.Turns = []Turn{}
	for _, rid := range m.order[id] {
		r := m.runs[id][rid]
		if r.Phase == "finished" {
			o := clone(r.Output)
			s.Turns = append(s.Turns, Turn{Input: r.Input, Output: &o})
		}
	}
	if len(s.Turns) > 10 {
		s.Turns = s.Turns[len(s.Turns)-10:]
	}
	return s, nil
}

func (m *memoryRepo) GetTurn(ctx context.Context, id, rid string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id][rid]
	if !ok {
		return Run{}, ErrNotFound
	}
	return clone(r), nil
}

func (l *memoryLease) Context() context.Context                  { return l.ctx }
func (l *memoryLease) Release()                                  { l.cancel(); <-l.gate }
func (l *memoryLease) Load(ctx context.Context) (Session, error) { return l.repo.Get(ctx, l.id) }
func (l *memoryLease) GetRun(ctx context.Context, id string) (Run, error) {
	return l.repo.GetTurn(ctx, l.id, id)
}

func (l *memoryLease) Begin(ctx context.Context, in Input) (Run, bool, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	if r, ok := l.repo.runs[l.id][in.RequestID]; ok {
		if !equalJSON(r.Input, in) {
			return Run{}, true, ErrConflict
		}
		return clone(r), true, nil
	}
	for _, r := range l.repo.runs[l.id] {
		if r.Phase != "finished" {
			return Run{}, false, ErrBusy
		}
	}
	r := Run{Input: clone(in), Phase: "received"}
	l.repo.runs[l.id][in.RequestID] = r
	l.repo.order[l.id] = append(l.repo.order[l.id], in.RequestID)
	return clone(r), false, nil
}

// save must be called with repo.mu held.
func (l *memoryLease) save(s *Session, r *Run) error {
	if l.repo.sessions[l.id].Version != s.Version {
		return ErrConflict
	}
	s.Version++
	s.UpdatedAt = time.Now()
	saved := clone(*s)
	saved.Turns = nil
	saved.RoutingContext = nil
	l.repo.sessions[l.id] = saved
	l.repo.runs[l.id][r.Input.RequestID] = clone(*r)
	return nil
}

func (l *memoryLease) Save(ctx context.Context, s *Session, r *Run, event *Event) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	return l.save(s, r)
}

func (l *memoryLease) Tool(ctx context.Context, s *Session, r *Run, op, name string, args Values, apply func(Values)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	result := l.repo.backend.Execute(name, args, l.id+":"+r.Input.RequestID+":"+op)
	apply(result)
	return l.save(s, r)
}
