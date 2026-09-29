package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

const (
	windowSize       = 5
	baseLatency      = 20 * time.Millisecond
	slowExtraLatency = 300 * time.Millisecond
	rampMaxLatency   = 800 * time.Millisecond
)

var validModes = map[string]bool{
	"normal": true,
	"slow":   true,
	"cpu":    true,
	"errors": true,
	"down":   true,
	"ramp":   true,
}

// secondBucket stores work request statistics for one second of the sliding window.
type secondBucket struct {
	second   int64
	requests int64
	errors   int64
	latency  time.Duration
}

type metricsWindow struct {
	mu      sync.Mutex
	buckets [windowSize]secondBucket
}

func (w *metricsWindow) record(at time.Time, latency time.Duration, isError bool) {
	second := at.Unix()
	w.mu.Lock()
	defer w.mu.Unlock()

	bucket := &w.buckets[second%windowSize]
	if bucket.second != second {
		*bucket = secondBucket{second: second}
	}
	bucket.requests++
	bucket.latency += latency
	if isError {
		bucket.errors++
	}
}

func (w *metricsWindow) snapshot(now time.Time) (requests, errors int64, latency time.Duration) {
	minSecond := now.Unix() - windowSize + 1
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, bucket := range w.buckets {
		if bucket.second >= minSecond && bucket.second <= now.Unix() {
			requests += bucket.requests
			errors += bucket.errors
			latency += bucket.latency
		}
	}
	return requests, errors, latency
}

type chaosState struct {
	mu         sync.RWMutex
	transition sync.Mutex
	mode       string
	changed    time.Time
	stopCPU    context.CancelFunc
	cpuGroup   sync.WaitGroup
}

func (c *chaosState) current() (string, time.Duration, float64) {
	c.mu.RLock()
	mode, changed := c.mode, c.changed
	c.mu.RUnlock()

	switch mode {
	case "slow":
		return mode, slowExtraLatency, 0
	case "errors":
		return mode, 0, 0.30
	case "ramp":
		elapsed := time.Since(changed)
		seconds := elapsed.Seconds()
		latency := time.Duration(seconds / 10 * float64(rampMaxLatency))
		if latency > rampMaxLatency {
			latency = rampMaxLatency
		}
		errorProbability := seconds / 10 * 0.60
		if errorProbability > 0.60 {
			errorProbability = 0.60
		}
		return mode, latency, errorProbability
	default:
		return mode, 0, 0
	}
}

func (c *chaosState) setMode(mode string) error {
	if !validModes[mode] {
		return errors.New("mode must be one of normal, slow, cpu, errors, down, ramp")
	}

	c.transition.Lock()
	defer c.transition.Unlock()

	c.mu.Lock()
	oldStop := c.stopCPU
	c.stopCPU = nil
	c.mode = mode
	c.changed = time.Now()
	c.mu.Unlock()

	if oldStop != nil {
		oldStop()
		c.cpuGroup.Wait()
	}

	if mode == "cpu" {
		ctx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		c.stopCPU = cancel
		c.cpuGroup.Add(runtime.NumCPU())
		c.mu.Unlock()
		for i := 0; i < runtime.NumCPU(); i++ {
			go burnCPU(ctx, &c.cpuGroup)
		}
	}
	return nil
}

func burnCPU(ctx context.Context, group *sync.WaitGroup) {
	defer group.Done()
	var value uint64
	for {
		select {
		case <-ctx.Done():
			return
		default:
			value = value*1664525 + 1013904223
			if value == 0 {
				runtime.Gosched()
			}
		}
	}
}

type backend struct {
	serverID string
	active   atomic.Int64
	window   metricsWindow
	chaos    chaosState
}

type workResponse struct {
	ServerID  string `json:"server_id"`
	LatencyMS int64  `json:"latency_ms"`
}

type metricsResponse struct {
	ServerID          string  `json:"server_id"`
	CPUPercent        float64 `json:"cpu_percent"`
	MemPercent        float64 `json:"mem_percent"`
	ActiveConnections int64   `json:"active_connections"`
	RequestRate       float64 `json:"request_rate"`
	ErrorRate         float64 `json:"error_rate"`
	AvgLatencyMS      float64 `json:"avg_latency_ms"`
	Mode              string  `json:"mode"`
}

func (b *backend) workHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	b.active.Add(1)
	defer b.active.Add(-1)

	mode, extraLatency, errorProbability := b.chaos.current()
	if mode == "down" {
		b.recordWork(start, time.Since(start), true)
		http.Error(w, `{"error":"backend is down"}`, http.StatusServiceUnavailable)
		return
	}

	latency := baseLatency + time.Duration(rand.Int63n(int64(10*time.Millisecond))) + extraLatency
	time.Sleep(latency)
	actualLatency := time.Since(start)
	if rand.Float64() < errorProbability {
		b.recordWork(start, actualLatency, true)
		http.Error(w, `{"error":"injected failure"}`, http.StatusInternalServerError)
		return
	}

	b.recordWork(start, actualLatency, false)
	writeJSON(w, http.StatusOK, workResponse{
		ServerID:  b.serverID,
		LatencyMS: actualLatency.Milliseconds(),
	})
}

func (b *backend) recordWork(at time.Time, latency time.Duration, isError bool) {
	b.window.record(at, latency, isError)
}

func (b *backend) healthHandler(w http.ResponseWriter, r *http.Request) {
	mode, _, _ := b.chaos.current()
	if mode == "down" {
		http.Error(w, `{"status":"down"}`, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (b *backend) metricsHandler(w http.ResponseWriter, r *http.Request) {
	cpuValues, err := cpu.Percent(0, false)
	if err != nil {
		http.Error(w, "failed to read CPU metrics", http.StatusInternalServerError)
		return
	}
	memory, err := mem.VirtualMemory()
	if err != nil {
		http.Error(w, "failed to read memory metrics", http.StatusInternalServerError)
		return
	}

	requests, errors, totalLatency := b.window.snapshot(time.Now())
	errorRate := 0.0
	averageLatency := 0.0
	if requests > 0 {
		errorRate = float64(errors) / float64(requests)
		averageLatency = totalLatency.Seconds() * 1000 / float64(requests)
	}
	cpuPercent := 0.0
	if len(cpuValues) > 0 {
		cpuPercent = cpuValues[0]
	}
	mode, _, _ := b.chaos.current()
	writeJSON(w, http.StatusOK, metricsResponse{
		ServerID:          b.serverID,
		CPUPercent:        cpuPercent,
		MemPercent:        memory.UsedPercent,
		ActiveConnections: b.active.Load(),
		RequestRate:       float64(requests) / windowSize,
		ErrorRate:         errorRate,
		AvgLatencyMS:      averageLatency,
		Mode:              mode,
	})
}

func (b *backend) chaosHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "request body must be JSON with a mode", http.StatusBadRequest)
		return
	}
	request.Mode = strings.TrimSpace(strings.ToLower(request.Mode))
	if err := b.chaos.setMode(request.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"mode": request.Mode})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("writing JSON response: %v", err)
	}
}

func main() {
	port := envOrDefault("PORT", "9001")
	serverID := envOrDefault("SERVER_ID", "backend-1")
	backendServer := &backend{serverID: serverID}
	backendServer.chaos.mode = "normal"
	backendServer.chaos.changed = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/work", backendServer.workHandler)
	mux.HandleFunc("/health", backendServer.healthHandler)
	mux.HandleFunc("/metrics", backendServer.metricsHandler)
	mux.HandleFunc("/chaos", backendServer.chaosHandler)

	server := &http.Server{Addr: ":" + port, Handler: mux}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("backend %s listening on :%s", serverID, port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	backendServer.chaos.setMode("normal")
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
