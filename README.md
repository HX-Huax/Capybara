# Capybara

English | [简体中文](README.zh-CN.md)

A GPU job queue for shared, single-server labs, written in Go. Submit commands from existing Docker containers and let a host daemon queue jobs, allocate GPUs, and run each command inside its originating container.

Capybara currently provides an M1 foundation. It includes job submission, priority scheduling with FIFO ties, SQLite persistence, GPU allocation by UUID, container identity checks, execution, timeouts, cancellation, and log streaming. See [Current limitations](#current-limitations) before deployment.

## How it works

1. A user submits a command with `capy` inside a Docker container.
2. The host daemon, `capybarad`, identifies the container through Unix socket peer credentials and the submitting process's cgroup.
3. The daemon stores the job in SQLite and waits for enough GPUs to become available.
4. It runs the command using `docker exec`, sets `CUDA_VISIBLE_DEVICES`, and saves the output to a log file.
5. When the job finishes, the daemon records its result and releases its GPU allocation in the same database transaction.

The client and daemon communicate through HTTP/JSON over a Unix socket. The client does not access Docker or the database directly.

## Requirements

- Linux and Go 1.22 or later to build the project.
- Docker CLI and `nvidia-smi` on the host, with a working NVIDIA GPU setup.
- Access to the host's `/proc` filesystem and permission to execute Docker commands for the daemon account.
- `sh`, `setsid`, and a `kill` implementation that supports process group signals inside each job container.
- A Docker cgroup layout that the daemon can identify. User namespace UID mapping is not supported in the current version.

## Build and check

```sh
make build
make test
make vet
```

The build produces two static binaries using `CGO_ENABLED=0`:

| Binary | Purpose |
| --- | --- |
| `bin/capy` | Command-line client, run inside development containers |
| `bin/capybarad` | Job queue daemon, run on the host |

Build output in `bin/` is excluded from Git. If the default Go build cache is not writable, set `GOCACHE` to a writable directory, such as `/tmp/capybara-go-build`.

## Start the daemon

Copy the example configuration and adjust it for your server:

```sh
sudo install -d /etc/capybara
sudo install -m 0644 deploy/config.example.yaml /etc/capybara/config.yaml
sudo ./bin/capybarad --config /etc/capybara/config.yaml
```

The default configuration uses:

| Setting | Default |
| --- | --- |
| Unix socket | `/run/capybara/capybara.sock` |
| Data directory | `/var/lib/capybara` |
| Scheduling interval | `5s` |
| Default job time limit | `72h` |
| Cancellation grace period | `30s` |
| External GPU memory threshold | `500 MiB` |

The data directory has `0700` permissions and contains the SQLite database and job logs. Configuration loading rejects unknown fields; settings for planned features in the architecture draft are not yet supported.

A systemd service template is available at [`deploy/capybarad.service`](deploy/capybarad.service). It expects the daemon binary at `/usr/local/bin/capybarad` and the configuration at `/etc/capybara/config.yaml`.

## Connect a development container

Set a user label and mount the socket and client binary when creating a container. Replace `YOUR_IMAGE` with your development image; it must provide a suitable long-running default command, or you must supply one.

```sh
docker run -d --gpus all \
  --label capybara.user=alice \
  --mount type=bind,src=/run/capybara,dst=/run/capybara,readonly \
  -v "$PWD/bin/capy":/usr/local/bin/capy:ro \
  --name alice-dev YOUR_IMAGE
```

Start the daemon before creating the container so the runtime directory exists. Mount the entire `/run/capybara` directory as shown above. The service uses `RuntimeDirectoryPreserve=yes` to retain this directory across both restarts and separate stop/start operations. The daemon recreates the socket inside the same directory, so containers can reconnect without being restarted. Binding only the socket file leaves containers referencing the old socket after a daemon restart.

To apply the fix to an existing installation, run on the host when no jobs are active:

```sh
sudo install -m 0644 deploy/capybarad.service /etc/systemd/system/capybarad.service
sudo systemctl daemon-reload
sudo systemctl restart capybarad
sudo systemctl show capybarad -p RuntimeDirectoryPreserve
```

The last command should report `RuntimeDirectoryPreserve=yes`. If a container already references a deleted runtime directory, restart that container once after the daemon starts. Existing containers that bind only the socket file need to be recreated with the directory mount. `/run` is temporary storage; preserving the directory across service restarts does not preserve it across host reboots.

The daemon uses the `capybara.user` label to determine job ownership. Alternatively, map the container name to a user in the configuration:

```yaml
users:
  alice-dev: alice
```

Requests from the host, unidentified containers, or containers without a user label or configured mapping are rejected. Users can inspect and cancel only their own jobs.

## Submit and manage jobs

Run these commands inside the development container:

```sh
# Submit a job requesting one GPU for up to 24 hours.
capy submit -g 1 -p 10 -t 24h -- python train.py

# Pass environment variables explicitly.
capy submit -g 2 --env PYTHONPATH --env EXPERIMENT=baseline -- python train.py

# List jobs and inspect a job by ID.
capy ls
capy info 1

# Read or follow job output.
capy logs 1
capy logs -f 1

# Cancel a job and inspect GPU status.
capy cancel 1
capy nodes
```

Submission captures the current working directory and UID. Commands are transmitted as argument arrays. To use shell features such as pipes or redirection, submit a shell command explicitly:

```sh
capy submit -g 1 -- sh -c 'python train.py | tee training.log'
```

Environment variables are passed only through repeated `--env KEY` or `--env KEY=VALUE` options. The daemon sets GPU visibility and the job ID; these values cannot be overridden through `--env`. Job details returned by the API omit environment variables.

Submission, listing, job details, and GPU status currently return JSON. The client also accepts `--socket PATH` to connect to a nondefault socket.

## Job priority

Use `capy submit -p 10 -- COMMAND` to set a priority from **-100 to 100**. The default is **0**, including for existing records created before this feature. Higher values run first; ties use submission time and then job ID. Priority does not interrupt a running job, and a blocked queue head still reserves its place.

An administrator can change the priority of a **pending** job through the Web console or `PATCH /v1/admin/jobs/{id}` with `{"priority": 20}` over the Unix socket. The Unix administrator endpoint requires peer UID 0 and the normal container identity check. Users may set priority when submitting their own jobs; this is a scheduling preference, not a quota or permission boundary.

## Web control and monitoring

The Chinese-language administrator console is embedded in `capybarad`; no frontend build, Node service, or external CDN is required. It provides:

- Global job counts, queue search, user/state filters, and pagination.
- GPU allocation, memory usage, utilization, and the last 60 utilization samples from the current browser session.
- Job details, command arguments, timestamps, exit codes, and output logs.
- Priority editing for pending jobs and cancellation with confirmation.
- Automatic refresh every three seconds, with pause and manual refresh controls.

Job submission remains in the container CLI so the daemon can verify the submitting container. The Web token grants **global administrator access**, including logs and cancellation across all users. It is separate from Unix socket authentication.

### Enable on a server

The Web listener is disabled by default. Add or update the following in `/etc/capybara/config.yaml`:

```yaml
web:
  listen: "127.0.0.1:8080"
  token_env: CAPYBARA_WEB_TOKEN
```

Generate a token file once on the host:

```sh
sudo sh -c 'umask 077; printf "CAPYBARA_WEB_TOKEN=%s\n" "$(openssl rand -hex 32)" > /etc/capybara/web.env'
```

The updated systemd unit reads this optional environment file. Install the new binary and service template, then restart when no jobs are active:

```sh
make build
sudo install -m 0755 bin/capybarad /usr/local/bin/capybarad
sudo install -m 0644 deploy/capybarad.service /etc/systemd/system/capybarad.service
sudo systemctl daemon-reload
sudo systemctl restart capybarad
```

Install the updated `bin/capy` as well to use `-p`; containers that bind the previous client file may need restarting to see the replacement.

From your own computer, open an SSH tunnel to the host account:

```sh
ssh -N -L 8080:127.0.0.1:8080 icca@YOUR_SERVER
```

Visit **http://127.0.0.1:8080** and enter the token value from `/etc/capybara/web.env` (without the variable name). Only loopback IP listeners are accepted; the HTTP port is intended for local access or SSH forwarding. Tokens must contain at least 32 bytes and are retained only in browser memory. Reloading the page requires logging in again. Rotating the token requires updating the file and restarting the daemon.

Logs are refreshed as bounded snapshots of their last 64 KiB. Charts show actual GPU utilization samples and are not persisted. If GPU probing fails, the console reports the failure while keeping job inspection and controls available.

## Current limitations

- **Scheduling:** higher priority first, then submission time, then job ID. If the first pending job needs more GPUs than are available, later jobs wait too. Running jobs are not preempted. Quotas, fair-share scheduling, and backfilling are not implemented.
- **GPU monitoring:** `nvidia-smi` supplies memory usage snapshots. An unallocated GPU with memory usage above the configured threshold is treated as externally occupied. Process ownership checks through NVML are not implemented. GPU probing failures pause new GPU allocations.
- **Isolation:** `CUDA_VISIBLE_DEVICES` provides a soft constraint. Users can still bypass the queue and access GPUs exposed to their containers.
- **Cancellation:** cancellation and timeouts first send TERM to the container process group, then attempt KILL after the grace period. If termination cannot be confirmed, the job stays `running` and retains its GPU allocation for manual inspection.
- **Daemon restarts:** a normal shutdown cancels active jobs. After an abnormal exit, previously running jobs retain their allocations and require manual inspection. Shim-based takeover and automatic recovery are not implemented. Do not delete the database or manually release allocations while old jobs may still be running.
- **Additional features:** GPU administrator endpoints, notifications, log cleanup, queue position reporting, and `--wait` are not implemented. Pending-job priority editing is available through the administrator API and Web console.
- **API time limits:** submission accepts duration strings such as `24h`; job details expose `time_limit_ns` as an integer number of nanoseconds.

## Validation

Automated tests cover SQLite persistence, allocation conflicts and transaction rollback, stale state transitions, cgroup parsing, job ownership checks, environment variable redaction, shell argument preservation, resource retention after unconfirmed cancellation, priority ordering, FIFO ties, Web authentication, bounded log reads, and GPU metric parsing.

Unit tests, race detection, `go vet`, static builds, Unix/Web listener smoke tests, and desktop/mobile Chromium checks have passed in the development environment. Docker and GPU behavior is tested with substitutes; execution on real containers, driver compatibility, and GPU workloads still need validation on a GPU server.

## Design

The [architecture document](Capybara%20架构文档.md) describes the broader design and planned milestones in Chinese. Its implementation notes and this README distinguish the current implementation from future work.

## License

Licensed under the [MIT License](LICENSE).
