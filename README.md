# Adaptive Load Balancer with ML-Based Failure Prediction

A prototype load balancer that doesn't just react to failing servers, it **predicts** them. Backend metrics (CPU, memory, latency, error rate, active connections) are fed to a machine-learning model that estimates the probability of a server failing in the next few seconds. The balancer then shifts traffic away from at-risk servers *before* they fail, and eases them back in gradually once they recover.

> **Status:** work in progress. See the [Roadmap](#roadmap) for what is built so far.

---

## Why this project

Traditional strategies such as Round Robin and Least Connections are blind to server health. A dying backend keeps receiving its full share of traffic until a health check finally fails, and every request sent in that window is an error. This project compares four routing modes side by side:

| Mode | Behavior |
|---|---|
| Round Robin | Rotates through backends, ignores health |
| Least Connections | Picks the backend with the fewest active requests |
| Health-aware | Skips backends that fail health checks |
| **Adaptive (ML)** | Weights backends by predicted failure risk |

---

## Architecture

```
                    ┌──────────────┐
   Clients ───────► │ Reverse Proxy│  (Go)
  (load generator)  └──────┬───────┘
                           │ asks "which backend?"
                    ┌──────▼───────┐
                    │Routing Engine│  RoundRobin | LeastConn | Adaptive (weighted)
                    └──────┬───────┘
                           │ reads weights / status
                    ┌──────▼───────┐        ┌────────────────────┐
                    │ Backend      │◄───────│ Decision Engine    │
                    │ Registry     │ writes │ health + metrics + │
                    │(state+weight)│ status │ ML probability     │
                    └──────┬───────┘        └───▲────────────▲───┘
                           │                    │            │
                    ┌──────▼───────┐            │     ┌──────┴───────┐
                    │Health/Metrics│────────────┘     │ ML Predictor │
                    │ Monitor (Go) │  features ──────►│ (Python,     │
                    └──────┬───────┘                  │  FastAPI)    │
                           │ polls every 1-2 s        └──────────────┘
          ┌────────────────┼────────────────┐
     ┌────▼────┐      ┌────▼────┐      ┌────▼────┐
     │Backend A│      │Backend B│      │Backend C│   Docker containers
     │/work    │      │/health  │      │/metrics │   with /chaos fault injection
     └─────────┘      └─────────┘      └─────────┘

  Recovery Manager (inside LB): Critical -> Testing -> Active (gradual traffic ramp)
```

**Key design decisions**

- The ML model runs on a timer (every 1-2 s per backend), never per request, so routing stays fast.
- The ML predictor is a separate small Python service. Go handles the hot path, Python handles the model.
- Backends can be made to fail on demand via `/chaos`, which is what produces the labeled training data.

---

## Tech stack

| Component | Technology |
|---|---|
| Load balancer and backends | Go (standard library plus gopsutil) |
| ML training and prediction service | Python 3, scikit-learn (Random Forest), FastAPI |
| Containers | Docker, Docker Compose |
| Monitoring dashboard | Prometheus and Grafana (planned) |

---

## Project structure

```
adaptive-lb/
├── cmd/
│   ├── backend/          # fake backend server with fault injection
│   └── lb/               # load balancer / reverse proxy
├── internal/
│   ├── backend/          # Backend struct
│   └── router/           # Router interface, RoundRobin (more coming)
├── ml/                   # training scripts and predictor service (planned)
├── loadgen/              # load generator and data collection (planned)
├── data/                 # datasets (csv)
├── docs/                 # project synopsis and notes
├── docker-compose.yml
└── .github/copilot-instructions.md
```

---

## Prerequisites

- Go 1.22 or newer
- Docker and Docker Compose
- Python 3.10 or newer (from the ML phase onward)
- On Windows, WSL2 is recommended. Keep the repo inside the WSL filesystem (`~/adaptive-lb`), not under `/mnt/c`.

---

## Quick start

```bash
# 1. Clone and enter the repo
git clone https://github.com/<your-username>/adaptive-lb.git
cd adaptive-lb

# 2. Download Go dependencies
go mod tidy

# 3. Start the 3 backend servers (ports 9001-9003)
docker compose up --build -d

# 4. Start the load balancer (port 8080)
go run ./cmd/lb

# 5. Send some traffic
for i in 1 2 3 4 5 6; do curl -s -i localhost:8080/work | grep -i x-backend-id; done
```

You should see requests rotate across `backend-1`, `backend-2` and `backend-3`.

**No Docker yet?** You can run the backends directly instead (without CPU limits):

```bash
PORT=9001 SERVER_ID=backend-1 go run ./cmd/backend
PORT=9002 SERVER_ID=backend-2 go run ./cmd/backend
PORT=9003 SERVER_ID=backend-3 go run ./cmd/backend
```

---

## Backend server API

Each backend simulates a real service and can be made to misbehave on demand.

| Endpoint | Method | Description |
|---|---|---|
| `/work` | GET | Simulated request (~20-30 ms base latency). Returns 500 randomly in `errors` mode and 503 in `down` mode. |
| `/health` | GET | 200 when healthy, 503 in `down` mode. |
| `/metrics` | GET | JSON: `server_id`, `cpu_percent`, `mem_percent`, `active_connections`, `request_rate`, `error_rate`, `avg_latency_ms`, `mode`. Rates and latency are over a 5-second sliding window. |
| `/chaos` | POST | Switch failure mode: `{"mode": "..."}` |

**Chaos modes**

| Mode | Effect |
|---|---|
| `normal` | No faults |
| `slow` | Adds 300 ms latency to `/work` |
| `cpu` | Burns CPU in busy-loop goroutines |
| `errors` | 30% of `/work` requests return 500 |
| `down` | `/health` and `/work` return 503 |
| `ramp` | Latency and error rate increase steadily until the mode is changed (gradual degradation, ideal for training data) |

Example:

```bash
curl -X POST localhost:9001/chaos -d '{"mode":"cpu"}'
curl localhost:9001/metrics        # cpu_percent should climb
curl -X POST localhost:9001/chaos -d '{"mode":"normal"}'
```

Backend configuration: `PORT` (default `9001`) and `SERVER_ID` (default `backend-1`).

---

## Load balancer configuration

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address the proxy listens on |
| `BACKENDS` | `http://localhost:9001,http://localhost:9002,http://localhost:9003` | Comma-separated backend URLs |
| `STRATEGY` | `roundrobin` | Routing strategy |

Every proxied response carries an `X-Backend-ID` header showing which backend served it.

---

## Testing

```bash
# Unit tests (with the race detector)
go test -race ./internal/...

# Demonstrate the weakness of Round Robin: stop a backend, keep sending traffic
docker compose stop backend-2
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w "%{http_code}\n" localhost:8080/work; done
# every third request fails with 502
docker compose start backend-2
```

---

## Roadmap

- [x] **Phase 1:** repo setup, fake backends with fault injection, Docker Compose
- [x] **Phase 2:** reverse proxy with Round Robin
- [ ] **Phase 3:** backend registry, Least Connections, strategy switching
- [ ] **Phase 4:** health checks and metrics monitor
- [ ] **Phase 5:** load generator and labeled dataset collection
- [ ] **Phase 6:** ML training and prediction service
- [ ] **Phase 7:** decision engine and adaptive weighted routing
- [ ] **Phase 8:** recovery manager (Critical -> Testing -> Active)
- [ ] **Phase 9:** dashboard (Prometheus and Grafana)
- [ ] **Phase 10:** benchmarks comparing all four routing modes

---

## Results

_To be filled in during Phase 10:_ error rate, p95 latency and failed-request count for Round Robin vs Least Connections vs Health-aware vs Adaptive (ML), under identical fault-injection scenarios.

---

## License

Add a license of your choice (for example MIT) before publishing.
