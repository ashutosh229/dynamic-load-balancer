# Lab 6 — Dynamic Load Balancer for Persistent Group Chat

A single static Go binary, zero external dependencies, that sits in front of
your 3 backend instances and routes `/message` and `/feed` traffic based on
**live measured load**, not a fixed rotation.

---

## 1. Architecture

```
                         clients / load generator
                                   │
                                   ▼
                    ┌───────────────────────────┐
                    │   Load Balancer (Go)       │   <- 4th allotted system
                    │   listens on :8080         │
                    │                             │
                    │  ┌───────────────────────┐ │
                    │  │ HealthChecker (goroutine) │  polls /health every 2s
                    │  └───────────────────────┘ │
                    │  ┌───────────────────────┐ │
                    │  │ Balancer (score+threshold)│  picks backend per request
                    │  └───────────────────────┘ │
                    │  ┌───────────────────────┐ │
                    │  │ /message /feed /health /stats │
                    │  └───────────────────────┘ │
                    └──────────────┬──────────────┘
                       ┌────────────┼────────────┐
                       ▼            ▼             ▼
                 ┌───────────┐┌───────────┐┌───────────┐
                 │ Backend 1 ││ Backend 2 ││ Backend 3 │   <- 3 allotted systems
                 │  (your    ││  (your    ││  (your    │
                 │   app)    ││   app)    ││   app)    │
                 └─────┬─────┘└─────┬─────┘└─────┬─────┘
                       └────────────┼────────────┘
                                    ▼
                         ┌─────────────────────┐
                         │  Shared, persistent   │
                         │  datastore (Postgres  │
                         │  or SQLite over a      │
                         │  shared volume)        │
                         │  message_id UNIQUE     │
                         └─────────────────────┘
```

Key property: the LB is **stateless** — it holds no chat data itself. All
three backends read/write the same datastore, so it never matters which
backend serves a given request. That's what makes load-based (not
session-based) routing safe here.

---

## 2. Why this is not round robin

`balancer.go` implements **Weighted Least-Load with Threshold Cutover**:

1. Only backends currently marked `healthy` are eligible.
2. Each eligible backend gets a live score:
   `score = activeRequests + ewmaLatencyMs / 100`
   - `activeRequests` is incremented/decremented atomically the instant a
     request starts/finishes routing through that backend — this reacts in
     microseconds, not on a health-check tick.
   - `ewmaLatencyMs` is an exponentially-weighted moving average of that
     backend's real response times (`α = 0.3` by default), so a backend that
     is *slow* even with few connections still gets penalized.
3. A backend is **overloaded** if `activeRequests > max_active_requests` OR
   `ewmaLatencyMs > max_latency_ms` (the two tunable thresholds). Overloaded
   backends are skipped in favor of any non-overloaded healthy backend.
4. If **every** healthy backend is overloaded, the LB doesn't fail the
   request — it degrades gracefully and sends it to the least-bad backend.
5. The chosen backend list is fully ordered by score, so if the top pick's
   request fails mid-flight (timeout, connection refused, 5xx), the LB
   automatically retries against the next-best backend (`max_retries`,
   default 2) before giving up. This is safe specifically *because* your
   backend de-duplicates by message ID (see §5).

This means traffic share shifts continuously and automatically toward
whichever backend is fastest/least busy right now, and a backend that starts
timing out under load sheds traffic within one request cycle — not after a
fixed number of round-robin turns.

## 3. Health detection (separate from load scoring)

`health.go` runs an independent goroutine that polls each backend's
`/health` path every `health_check_interval_ms` (default 2s) with its own
timeout (default 1.5s). Hysteresis avoids flapping:

- `unhealthy_threshold` (default 3) consecutive failures → marked DOWN,
  removed from rotation entirely (not just deprioritized).
- `healthy_threshold` (default 2) consecutive successes → marked back UP.

If your backend has no dedicated `/health` route, point
`health_check_path` at `/feed` in `config.json` — any 2xx–4xx response is
treated as "process is alive"; 5xx or connection failure counts as down.

## 4. Required routes (exposed by the LB, exactly as specified)

| Route      | Method | Behavior                                                        |
|------------|--------|-------------------------------------------------------------------|
| `/message` | POST   | Forwarded to the best-scoring healthy backend, with retry.         |
| `/feed`    | GET    | Forwarded to the best-scoring healthy backend, with retry.         |
| `/health`  | GET    | LB's own liveness: 200 if ≥1 backend healthy, else 503.            |
| `/stats`   | GET    | JSON dump of per-backend health/load, for your own tuning/demo.    |

`/stats` is bearer-token protected if you set `admin_token` in
`config.json`; leave it blank to disable auth (fine for a lab demo).

---

## 5. Persistence and duplicate prevention (backend team's contract)

The LB has no opinion on your backend's internal stack, but for the
assignment's correctness requirement to hold, the backend team needs:

1. **One shared datastore reachable from all 3 backend instances** — not
   three separate local SQLite files. Two workable options on 3 systems:
   - Run Postgres/MySQL on one of the three systems and have all three
     backend processes (including the one colocated with the DB) connect to
     it over the network.
   - Or use SQLite in WAL mode on a network filesystem shared by all three
     — workable for a lab, but a real DB server is more robust under
     concurrent writes from 3 processes.
2. **A unique, client-generated message ID** on every message (e.g. a UUID
   the client generates once per message, so retries of the *same* logical
   message reuse the *same* ID). Schema:
   ```sql
   CREATE TABLE messages (
     message_id  TEXT PRIMARY KEY,      -- or UUID type in Postgres
     client_name TEXT NOT NULL,
     msg         TEXT NOT NULL,
     created_at  TIMESTAMP NOT NULL DEFAULT now()
   );
   ```
3. **Idempotent insert** so a retried/duplicated POST is a no-op instead of
   a duplicate row:
   ```sql
   INSERT INTO messages (message_id, client_name, msg)
   VALUES ($1, $2, $3)
   ON CONFLICT (message_id) DO NOTHING;
   ```
   (SQLite: `INSERT OR IGNORE INTO messages ...`)

This matters here specifically because the LB's own retry logic (§2, point
5) will occasionally replay a POST against a second backend if the first
one times out right at the edge of success — without the `UNIQUE` +
`ON CONFLICT DO NOTHING` pattern, that replay becomes a visible duplicate
message in `/feed`.

---

## 6. Deployment on your allotted systems

You have 4 systems behind the same IP (`10.1.75.79`), each identified only
by a distinct port range. Per your setup: if a system's **SSH port** is
`22XX`, an app you run on that system on port `3000/4000/5000/...` is
reachable globally at `3XX_/4XX_/5XX_` — concretely:

```
global_port = (app_port // 1000) * 1000 + (ssh_port mod 1000)
```

Example: SSH port `2205`, app bound to `4000` → globally reachable at
`4205`.

**Plan:** run the 3 backends on systems A, B, C (already handled by your
teammates) and run this load balancer on your 4th system.

### Step-by-step (repeat the backend part per teammate/system)

1. **Note each system's SSH port** — call them `ssh_A`, `ssh_B`, `ssh_C` for
   backends and `ssh_LB` for the load balancer's system.
2. **Backends**: have each teammate run their backend bound to an app port,
   e.g. `4000`, on their own system, and point it at the shared DB (§5).
   Its globally reachable URL is then
   `http://10.1.75.79:<4000-thousands + ssh_X mod 1000>`.
3. **Load balancer**: on your 4th system, run the LB bound to app port
   `3000` (any app port your systems allow) inside `config.json`'s
   `listen_addr` — e.g. `":3000"`. It becomes globally reachable at
   `http://10.1.75.79:<3000-thousands + ssh_LB mod 1000>`.
4. Fill in `config.json`'s `backends[].url` with the **globally reachable**
   backend URLs computed in step 2 (the LB itself talks to backends over
   this same public mapping unless you have private networking between the
   4 containers — if you do, prefer the private addresses/ports for lower
   latency).
5. Copy the binary + config to the LB system and run it:
   ```bash
   scp -P <ssh_LB> lb config.json lb.service user@10.1.75.79:/opt/lb/
   ssh -p <ssh_LB> user@10.1.75.79
   sudo mv /opt/lb/lb.service /etc/systemd/system/lb.service
   sudo useradd -r -s /usr/sbin/nologin lbrunner || true
   sudo chown -R lbrunner:lbrunner /opt/lb
   sudo systemctl daemon-reload
   sudo systemctl enable --now lb
   sudo systemctl status lb
   ```
6. **Verify**:
   ```bash
   curl http://10.1.75.79:<lb_global_port>/health
   curl -X POST http://10.1.75.79:<lb_global_port>/message \
        -d '{"client-name":"alice","msg":"hello"}'
   curl http://10.1.75.79:<lb_global_port>/feed
   curl http://10.1.75.79:<lb_global_port>/stats
   ```
7. **Submit** `http://10.1.75.79:<lb_global_port>` as your Load Balancer
   URL.

### Container-native alternative

If your allotted systems accept Docker-in-Docker or `docker compose`, use
the included `Dockerfile` instead of systemd:
```bash
docker build -t chat-lb .
docker run -d --restart unless-stopped -p 8000:8080 \
  -v $(pwd)/config.json:/etc/lb/config.json:ro --name chat-lb chat-lb
```
(map your chosen host app port to the container's `8080`, then apply the
same global-port formula to the host port you picked).

---

## 7. Choosing the threshold (`max_active_requests`, `max_latency_ms`)

There's no universally "correct" number — it depends on each backend
system's actual capacity, which you should measure, not guess:

1. **Isolate one backend.** Point a load generator (or `hey`/`wrk`/`ab`)
   directly at a single backend instance (bypass the LB) and ramp
   concurrency: 1, 5, 10, 20, 40, 80 concurrent clients hitting `/message`.
2. **Plot latency vs. concurrency.** Watch `/stats` (or your backend's own
   logs) for where p50/p95 latency starts climbing steeply rather than
   staying flat — that's the *knee point* where the backend transitions
   from "handling load fine" to "queueing/degrading."
3. **Set `max_active_requests` a bit below the knee**, e.g. if latency stays
   flat up to ~25 concurrent in-flight requests and then spikes, set
   `max_active_requests` around 15–18 — high enough to use real capacity,
   low enough to shift traffic away before users feel the degradation.
4. **Set `max_latency_ms` to roughly 2–3× the backend's normal p50 latency**
   under light load. This catches degradation caused by things concurrency
   alone won't show (e.g. a slow DB query pattern, CPU contention with
   another process on that shared system).
5. **Re-check with all 3 backends live** behind the LB, since your 3
   systems may not be identical in horsepower — it's fine (expected, even)
   for each backend to reach its own knee at a different concurrency; the
   thresholds here are global to the LB's config but the scoring
   naturally compensates because latency, not just a raw count, feeds the
   score.
6. **Because the leaderboard re-runs the load generator**, start
   conservative (favor correctness/availability over squeezing out the last
   bit of raw throughput), watch `/stats` during a dry run, then tighten
   the thresholds once you've seen real numbers rather than tuning blind.

The defaults shipped in `config.json` (`max_active_requests: 15`,
`max_latency_ms: 250`) are a reasonable starting point for small
lab-container-class hardware — treat them as a first guess to refine with
step 1–5 above, not a final answer.

---

## 8. Files in this deliverable

```
lb-project/
├── main.go               entrypoint, graceful shutdown
├── config.go              config.json loader + defaults
├── backend.go             Backend struct: health state + atomic live metrics
├── balancer.go            the dynamic scoring/threshold selection algorithm
├── health.go              background health-check goroutine
├── handlers.go            /message /feed /health /stats + retry-on-failure
├── util.go                small helpers
├── integration_test.go    tests: dynamic routing shift + unhealthy exclusion
├── config.json            example runtime config (fill in your backend URLs)
├── Dockerfile             optional containerized deployment
├── lb.service             systemd unit for the recommended bare-metal deploy
└── README.md              this file
```

## 9. Local testing

```bash
go test ./... -v      # runs the included integration tests against fake backends
go build -o lb .       # produces the deployable binary
./lb -config config.json
```
