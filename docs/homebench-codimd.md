---
title: Homebench — How it works
tags: homebench, eos, benchmark, filesystem
---

# Homebench

Distributed shared-filesystem stress harness: one **controller** (Web UI + orchestration) drives many **clients** that create, delete, read, and write on a common mount — then reports IOPS, bandwidth, and latency.

IO goes to the filesystem, **not** through the controller.

---

## Repository

| | |
|---|---|
| GitHub | https://github.com/apeters1971/home-bench |
| Module | `github.com/apeters/homebench` |
| Language | Go |

```bash
git clone https://github.com/apeters1971/home-bench.git
cd home-bench
```

---

## Build

Requires a recent Go toolchain (see `go.mod`).

```bash
make                 # builds both binaries into bin/
# or:
go build -o bin/homebench-controller ./cmd/controller
go build -o bin/homebench-client ./cmd/client
```

| Binary | Role |
|--------|------|
| `bin/homebench-controller` | Config, phase orchestration, metrics aggregation, Web UI |
| `bin/homebench-client` | Registers with controller, runs IO phases, reports metrics |

---

## Starting the controller

Run the controller on a host that clients can reach (batch nodes must open a TCP connection to the controller listen port).

```bash
./bin/homebench-controller -addr :8080
# optional: -config /path/to/homebench-config.json
```

- Default listen address: `:8080`
- Web UI: `http://<controller-host>:8080`
- Config is persisted as JSON (default `homebench-config.json`)

Open the UI in a browser to set prefixes, rates, optional software/git/untar URLs, select participants, and Start/Stop.

---

## Starting clients (batch jobs)

Clients are typically submitted as **long-running batch jobs** (e.g. SLURM `srun` / job arrays). Each client process:

1. Connects to the controller over **WebSocket** (`/ws/client`)
2. Stays registered until the job ends or is killed
3. Reconnects automatically if the controller restarts

**Network requirement:** every client host must be able to reach the controller’s HTTP/WebSocket port (default **8080**). There is no reverse connection from controller → client for work; if the batch network cannot open that port, clients never appear in the UI.

```bash
# Interactive / debug
./bin/homebench-client -controller http://CONTROLLER:8080

# Optional: override the name used for the private tree under the shared FS
./bin/homebench-client -controller http://CONTROLLER:8080 -hostname node042

# SLURM-style sketch — long-running jobs
srun ./bin/homebench-client -controller http://controller.example:8080
```

Flags:

| Flag | Meaning |
|------|---------|
| `-controller` | Controller base URL (default `http://127.0.0.1:8080`) |
| `-hostname` | Override hostname used for path layout and prefix hashing (default: OS hostname) |

---

## Architecture

```
 Browser ──WebSocket /ws/ui──► Controller (:8080)
                                  ▲
 Clients ──WebSocket /ws/client───┘
                                  │
                          Shared filesystem
                     (prefixes / test / hosts)
```

| Piece | Responsibility |
|-------|----------------|
| **Controller** | Holds config, drives phase order, aggregates metrics, serves UI |
| **Web UI** | Live snapshot: clients, charts, latency histograms, results, export |
| **Clients** | Receive phase commands, run IO on the shared FS, push metrics |
| **Prefix** | `prefixes[hash(hostname) % N]` — data plane is the mount, not the controller |

---

## Shared filesystem layout

```
<prefix>/<test>/software/          shared assets (package, git bundle, archive)
<prefix>/<test>/<hostname>/        private tree per client
  ├─ git/ · untar/                 timed software ops (then cleaned)
  └─ shards / files                create · delete · R/W
```

- Controller prepares shared `software/` once (unpack package / create `repo.bundle` / download untar archive).
- Clients time clone / untar into their own `<hostname>/` directories.
- Hostname can be overridden with `-hostname` — that string names the private tree. Hostnames must be unique across clients.

---

## Run phases

Phases appear as buttons above **Start** / **Stop**. Click to enable/disable for the next run (struck-through = skipped). Linked groups:

- **Software Unpack / Cold / Warm** toggle together
- **Create / Delete** toggle together

| Phase | What it measures |
|-------|------------------|
| Software unpack → cold / warm | Startup command from shared package (optional) |
| Git clone | Wall time: clone from shared `repo.bundle` (optional) |
| Untar | Wall time: `tar xvf` shared archive (optional) |
| Create | Create IOPS — 4 KiB files |
| Delete | Delete IOPS (paths kept for bandwidth phases) |
| Write / Read BW | Bandwidth — 64 MiB files |
| Read+Write | Overlapped R+W at configured rates |
| RIOPS | 5× phase step: 1 GiB sparse file, then sequential random 4 KiB direct writes, then reads (IOPS on the timeline). Optional global write/read IOPS ceilings are split per client; empty/0 = unlimited. |
| Final delete | Paced delete, then force wipe of host trees |

Orange/optional software phases only appear when the corresponding URLs/commands are configured in the UI.

---

## Load model (ramps and rates)

- You set **global** create/delete rates and write/read bandwidth in the UI.
- Each **selected** participant gets `global ÷ N`.
- Every IO phase ramps **10% → 100%** in 10% steps (default **30 s**/step), except **RIOPS** (5× step duration: writes then reads).
- Delete adds an extra 100% sweep; final delete ends with a forced host-tree wipe.
- Losing a run participant fails the run. Selection is locked while running.
- Header **Startup** = time from first client connect to last client connect; **Elapsed** = run wall time.

---

## Metrics and results

| View | Content |
|------|---------|
| IOPS / bandwidth charts | Observed vs grey expected lines; phase overlays; ~60 min history · RIOPS shows write then read IOPS |
| Latency histograms | Create, delete, 64 MiB R/W, software, git, untar (+ failures) |
| Results | Phase efficiency vs target, peaks, RIOPS write/read averages, p95/p99; JSON / printable report |

With large fleets, clients report less often (scaled metrics interval); the controller spreads each multi-second sample across the report window so charts stay near the true aggregate rate.

---

## Typical workflow

1. Build binaries; start the **controller** on a reachable host.
2. Submit many long-running **client** batch jobs pointing at `http://CONTROLLER:8080`.
3. Wait until clients show as registered (check **Startup** span and client count).
4. Configure prefixes (shared mount paths), rates, optional software/git/untar.
5. Select participants (all / first N / manual).
6. Optionally disable phases via the phase buttons.
7. **Start** — watch charts; **Stop** cancels and cleans client trees.
8. Export JSON or printable report when done.
