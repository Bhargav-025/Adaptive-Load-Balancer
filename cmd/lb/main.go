package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"load-balancer-prototype/internal/backend"
	"load-balancer-prototype/internal/router"
)

const (
	defaultListenAddr = ":8080"
	defaultBackends   = "http://localhost:9001,http://localhost:9002,http://localhost:9003"
	defaultStrategy   = "roundrobin"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func main() {
	listenAddr := envOrDefault("LISTEN_ADDR", defaultListenAddr)
	backends, err := buildBackends(envOrDefault("BACKENDS", defaultBackends))
	if err != nil {
		log.Fatalf("configure backends: %v", err)
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	configureProxies(backends, transport)

	routing, err := newRouter(envOrDefault("STRATEGY", defaultStrategy))
	if err != nil {
		log.Fatalf("configure router: %v", err)
	}

	server := &http.Server{
		Addr:    listenAddr,
		Handler: newHandler(backends, routing),
	}

	go func() {
		log.Printf("load balancer listening on %s using %s", listenAddr, routing.Name())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	waitForShutdown(server)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func buildBackends(rawBackends string) ([]*backend.Backend, error) {
	rawURLs := strings.Split(rawBackends, ",")
	backends := make([]*backend.Backend, 0, len(rawURLs))
	for i, rawURL := range rawURLs {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			return nil, fmt.Errorf("backend %d has an empty URL", i+1)
		}

		item, err := backend.New(fmt.Sprintf("backend-%d", i+1), rawURL)
		if err != nil {
			return nil, fmt.Errorf("backend %d: %w", i+1, err)
		}
		if item.URL.Scheme == "" || item.URL.Host == "" {
			return nil, fmt.Errorf("backend %d: URL must include a scheme and host", i+1)
		}
		backends = append(backends, item)
	}
	return backends, nil
}

func newRouter(strategy string) (router.Router, error) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "roundrobin":
		return &router.RoundRobin{}, nil
	default:
		return nil, fmt.Errorf("unsupported routing strategy %q", strategy)
	}
}

func configureProxies(backends []*backend.Backend, transport *http.Transport) {
	for _, item := range backends {
		item := item
		item.Proxy.Transport = transport
		item.Proxy.ModifyResponse = func(response *http.Response) error {
			response.Header.Set("X-Backend-ID", item.ID)
			return nil
		}
		item.Proxy.ErrorHandler = func(responseWriter http.ResponseWriter, request *http.Request, err error) {
			responseWriter.Header().Set("X-Backend-ID", item.ID)
			log.Printf("proxy error backend=%s method=%s path=%s error=%v", item.ID, request.Method, request.URL.Path, err)
			writeJSONError(responseWriter, http.StatusBadGateway, "bad gateway")
		}
	}
}

func newHandler(backends []*backend.Backend, routing router.Router) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		start := time.Now()
		selectedID := "-"
		writer := &statusWriter{ResponseWriter: responseWriter}

		item, err := routing.Next(backends)
		if err != nil {
			if errors.Is(err, router.ErrNoBackend) {
				writeJSONError(writer, http.StatusServiceUnavailable, "no available backend")
			} else {
				writeJSONError(writer, http.StatusInternalServerError, "routing error")
			}
			logRequest(request, selectedID, writer.status, start)
			return
		}

		selectedID = item.ID
		item.ActiveConns.Add(1)
		defer item.ActiveConns.Add(-1)

		item.Proxy.ServeHTTP(writer, request)
		logRequest(request, selectedID, writer.status, start)
	})
}

func logRequest(request *http.Request, backendID string, status int, start time.Time) {
	if status == 0 {
		status = http.StatusOK
	}
	log.Printf("method=%s path=%s backend=%s status=%d duration=%s",
		request.Method, request.URL.Path, backendID, status, time.Since(start))
}

func writeJSONError(responseWriter http.ResponseWriter, status int, message string) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(status)
	_ = json.NewEncoder(responseWriter).Encode(map[string]string{"error": message})
}

func waitForShutdown(server *http.Server) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	signal.Stop(signals)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
}
