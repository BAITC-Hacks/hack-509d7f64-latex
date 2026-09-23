package router

import (
	"context"
	"errors"
	"testing"
)

// memoryTestRepo names the in-memory repository for tests that inspect its
// synthetic backend directly.
type memoryTestRepo = memoryRepo

func testRepo(t *testing.T, c *Catalog) Repository {
	t.Helper()
	return NewMemoryRepository(c)
}

func TestRepositoryFailureDoesNotReportSuccess(t *testing.T) {
	c, _ := LoadCatalog()
	repo := &unavailableRepo{}
	e := NewEngine(c, &fakeModel{}, repo)
	_, err := e.Process(context.Background(), input("one", "hi"))
	if !errors.Is(err, ErrDatabase) {
		t.Fatal(err)
	}
}

type unavailableRepo struct{}

func (*unavailableRepo) Ready(context.Context) error                  { return ErrDatabase }
func (*unavailableRepo) Lock(context.Context, string) (Lease, error)  { return nil, ErrDatabase }
func (*unavailableRepo) Get(context.Context, string) (Session, error) { return Session{}, ErrDatabase }
func (*unavailableRepo) GetTurn(context.Context, string, string) (Run, error) {
	return Run{}, ErrDatabase
}
