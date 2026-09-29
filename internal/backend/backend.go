package backend

import (
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	ID          string
	URL         *url.URL
	Alive       atomic.Bool
	ActiveConns atomic.Int64
	Proxy       *httputil.ReverseProxy
}

func New(id, rawURL string) (*Backend, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	backend := &Backend{
		ID:    id,
		URL:   parsedURL,
		Proxy: httputil.NewSingleHostReverseProxy(parsedURL),
	}
	backend.Alive.Store(true)

	return backend, nil
}
