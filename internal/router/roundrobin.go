package router

import (
	"sync/atomic"

	"load-balancer-prototype/internal/backend"
)

type RoundRobin struct {
	counter atomic.Uint64
}

func (r *RoundRobin) Next(backends []*backend.Backend) (*backend.Backend, error) {
	aliveCount := 0
	for _, candidate := range backends {
		if candidate.Alive.Load() {
			aliveCount++
		}
	}
	if aliveCount == 0 {
		return nil, ErrNoBackend
	}

	target := (r.counter.Add(1) - 1) % uint64(aliveCount)
	for _, candidate := range backends {
		if candidate.Alive.Load() {
			if target == 0 {
				return candidate, nil
			}
			target--
		}
	}

	return nil, ErrNoBackend
}

func (r *RoundRobin) Name() string {
	return "round-robin"
}
