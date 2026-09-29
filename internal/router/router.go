package router

import (
	"errors"

	"load-balancer-prototype/internal/backend"
)

type Router interface {
	Next(backends []*backend.Backend) (*backend.Backend, error)
	Name() string
}

var ErrNoBackend = errors.New("no available backend")
