# Homebench

Distributed filesystem stress harness with a central controller (Web UI) and batch-job clients.

## Components

| Binary | Role |
|--------|------|
| `homebench-controller` | Config, orchestration, metrics aggregation, Web UI |
| `homebench-client` | Registers with controller, runs IO phases, reports metrics |

## Quick start

```bash
go build -o bin/homebench-controller ./cmd/controller
go build -o bin/homebench-client ./cmd/client

# Terminal 1
./bin/homebench-controller -addr :8080

# Terminal 2..N (or batch jobs)
./bin/homebench-client -controller http://CONTROLLER:8080
```

Open `http://CONTROLLER:8080`.

## How it works

1. Clients register over WebSocket. The controller assigns a directory prefix by hashing the hostname: `prefixes[hash(hostname) % len(prefixes)]`.
2. Global rates (create/delete ops/s, read/write bytes/s) are downscaled per client: `global / num_clients`.
3. **Start** runs this sequence:

| Phase | Intensity | Step length | Notes |
|-------|-----------|-------------|-------|
| Create | 10→100% | 60s | 4 KiB files under `prefix/testname/hostname/shard1/shard2/fileindex` |
| Delete | 10→100% (+ extra 100%) | 60s | Removes created files; path list kept for bandwidth phases |
| Write BW | 10→100% | 30s | Rewrites paths as 64 MiB files |
| Read BW | 10→100% | 30s | Reads those files |
| Read+Write | 10→100% | 30s | Concurrent read and write at their configured rates |
| Final Delete | 10→100% (+ extra 100%) | 60s | Removes remaining files |

4. **Stop** cancels the run and tells every client to delete `prefix/testname/hostname/`.
5. The UI shows a 30-minute history of aggregate IOPS and bandwidth, the active phase, and elapsed time.

## Configuration (Web UI)

- Global test name
- Directory prefixes (one per line)
- Global file creation rate (files/s)
- Global file deletion rate (files/s)
- Global write bandwidth (MiB/s)
- Global read bandwidth (MiB/s)

## Batch job example

```bash
# SLURM-style sketch
srun ./homebench-client -controller http://controller.example:8080
```

Clients reconnect automatically if the controller restarts.
