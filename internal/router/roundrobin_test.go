package router

import (
	"errors"
	"sync"
	"testing"

	"load-balancer-prototype/internal/backend"
)

func TestRoundRobinNext(t *testing.T) {
	tests := []struct {
		name    string
		alive   []bool
		calls   int
		wantIDs []string
		wantErr error
	}{
		{
			name:    "visits alive backends in order",
			alive:   []bool{true, true, true},
			calls:   6,
			wantIDs: []string{"A", "B", "C", "A", "B", "C"},
		},
		{
			name:    "skips dead backend",
			alive:   []bool{true, false, true},
			calls:   4,
			wantIDs: []string{"A", "C", "A", "C"},
		},
		{
			name:    "returns error when all backends are dead",
			alive:   []bool{false, false, false},
			calls:   1,
			wantErr: ErrNoBackend,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backends := testBackends(t, tt.alive)
			router := &RoundRobin{}

			for i := 0; i < tt.calls; i++ {
				got, err := router.Next(backends)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Next() error = %v, want %v", err, tt.wantErr)
				}
				if tt.wantErr != nil {
					continue
				}
				if got.ID != tt.wantIDs[i] {
					t.Errorf("Next() ID = %q, want %q", got.ID, tt.wantIDs[i])
				}
			}
		})
	}
}

func TestRoundRobinNextConcurrent(t *testing.T) {
	backends := testBackends(t, []bool{true, true, true})
	router := &RoundRobin{}
	const (
		goroutines = 100
		calls      = 3000
	)

	counts := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < calls/goroutines; j++ {
				backend, err := router.Next(backends)
				if err != nil {
					t.Errorf("Next() unexpected error: %v", err)
					return
				}
				mu.Lock()
				counts[backend.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, id := range []string{"A", "B", "C"} {
		if counts[id] != calls/len(backends) {
			t.Errorf("backend %q received %d calls, want %d", id, counts[id], calls/len(backends))
		}
	}
}

func testBackends(t *testing.T, alive []bool) []*backend.Backend {
	t.Helper()

	backends := make([]*backend.Backend, len(alive))
	for i, isAlive := range alive {
		id := string(rune('A' + i))
		backend, err := backend.New(id, "http://localhost:8080")
		if err != nil {
			t.Fatalf("backend.New() error = %v", err)
		}
		backend.Alive.Store(isAlive)
		backends[i] = backend
	}
	return backends
}
