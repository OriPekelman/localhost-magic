# localhost-magic Roadmap

This document describes the planned evolution of localhost-magic. Each milestone is designed as an independent module with clear interfaces so that development can happen in parallel across multiple workstreams.

> **Legend**: Each item lists the new Go packages/files it introduces and the existing packages it touches. Items within a milestone have no ordering dependency unless explicitly noted.

---

## Current Architecture

```
cmd/
  cli/main.go              CLI tool
  daemon/main.go            HTTP daemon + reverse proxy + dashboard

internal/
  naming/names.go           Name generation heuristics + blacklist
  portscan/                 OS-specific port scanning (darwin, linux)
  probe/http.go             HTTP detection on a host:port
  storage/store.go          JSON persistence for ServiceRecord
```

Key data type:

```go
type ServiceRecord struct {
    ID, Name, TargetHost, ExePath string
    Port, PID                     int
    Args                          []string
    UserDefined, IsActive, Keep   bool
    LastSeen                      time.Time
}
```

---

## Milestone 1 — Core Hardening

These items strengthen the existing codebase without adding new user-facing features. They unblock every later milestone.

### 1.1 Persistent Blacklist Configuration

**Problem**: Blacklist is currently in-memory only; it resets on daemon restart.

**Scope**: `internal/storage` only.

**Design**:

- Add a `BlacklistEntry` struct to `storage`:

  ```go
  type BlacklistEntry struct {
      Type      string    `json:"type"`      // "pid", "path", "pattern"
      Value     string    `json:"value"`
      CreatedAt time.Time `json:"created_at"`
  }
  ```

- Store in a separate file: `~/.config/localhost-magic/blacklist.json`
- `Store` gains `AddBlacklist(entry)`, `RemoveBlacklist(id)`, `ListBlacklist()`, `IsBlacklisted(exePath, args) bool`
- The existing `naming.IsBlacklisted()` moves into `storage` (or delegates to it) so the daemon and CLI share the same logic.

**Touches**: `internal/storage`, `internal/naming`, `cmd/daemon`, `cmd/cli`

**New files**: none (extends `store.go` or a new `blacklist.go` in the same package)

---

### 1.2 Naming Heuristics as Data

**Problem**: Name generation heuristics are hardcoded. Users cannot customize them, and growing the defaults means growing `names.go` into an unmaintainable file.

**Scope**: `internal/naming`

**Design**:

- Define a `NamingRule` schema:

  ```go
  type NamingRule struct {
      ID          string `json:"id"`
      Description string `json:"description"`
      Priority    int    `json:"priority"`     // lower = higher priority

      // Match conditions (all optional, AND-ed when present)
      ExePattern  string `json:"exe_pattern,omitempty"`  // regex on exe path
      ArgPattern  string `json:"arg_pattern,omitempty"`  // regex on joined args
      CwdPattern  string `json:"cwd_pattern,omitempty"`  // regex on cwd

      // Name extraction
      NameSource  string `json:"name_source"`  // "exe", "cwd", "arg", "parent_dir", "app_bundle", "static"
      NameRegex   string `json:"name_regex,omitempty"`   // capture group 1 = name
      StaticName  string `json:"static_name,omitempty"`  // when name_source = "static"
  }
  ```

- Ship a built-in `rules.json` embedded via `//go:embed`
- User overrides in `~/.config/localhost-magic/naming-rules.json` (merged on top)
- CLI commands:
  - `localhost-magic rules list` — show active rules with priority
  - `localhost-magic rules export > my-rules.json`
  - `localhost-magic rules import my-rules.json`
  - `localhost-magic rules set-base-dir ~/projects` — shortcut for setting the project root used by CWD-based heuristics

**Touches**: `internal/naming`, `cmd/cli`

**New files**: `internal/naming/rules.go`, `internal/naming/rules_builtin.json`

---

### 1.3 Service Groups and Subdomain Auto-Grouping

**Problem**: When multiple services share a common prefix (e.g. `ollama`, `ollama-1`, `ollama-2`), they show as flat siblings. It would be more natural to present them as `api.ollama.localhost`, `web.ollama.localhost`.

**Scope**: `internal/naming`, `internal/storage`, `cmd/daemon`

**Design**:

- Add an optional `Group` field to `ServiceRecord`:

  ```go
  Group string `json:"group,omitempty"` // e.g. "ollama"
  ```

- When the naming engine detects a collision on base name `X`, instead of `X-1.localhost` it can produce `<differentiator>.X.localhost` where `<differentiator>` is derived from the distinguishing argument or port.
- Grouping behavior controlled by a config flag (default: on for new installs, off for upgrades to preserve existing names).
- The reverse proxy already matches on full hostname so no proxy changes needed — just the name generation.
- Dashboard groups services visually under collapsible headers.

**Touches**: `internal/naming`, `internal/storage`, `cmd/daemon` (dashboard HTML)

**New files**: none

---

## Milestone 2 — Local Dev TLS Authority (`internal/tls`)

Full spec in [docs/specs/local-tls-authority.md](docs/specs/local-tls-authority.md).

This is the largest single feature. It is designed as a standalone `internal/tls` package with a clean Go API so the daemon, CLI, and future GUI can all consume it identically.

### 2.1 Root & Intermediate CA Management (`internal/tls/ca`)

**Problem**: No local certificate authority exists.

**Design**:

- Two-tier CA: Root (Ed25519) + Intermediate (Ed25519)
- Root is long-lived but has an explicit rotation command
- Intermediate is shorter-lived, auto-rotated
- Storage under `~/.localtls/` (separate from service config):

  ```
  ~/.localtls/
  ├── root_ca.pem
  ├── root_ca.key          (0600)
  ├── intermediate.pem
  ├── intermediate.key     (0600)
  ├── certs/               (issued leaf certs)
  │   ├── myapp.localhost.pem
  │   └── myapp.localhost.key
  ├── index.json           (cert inventory)
  └── config.json
  ```

- Keys never leave the machine. No ACME. No network calls.
- Atomic writes for all key/cert operations.

**Cryptography choices**:

| Item             | Choice                        |
|------------------|-------------------------------|
| Root key         | Ed25519                       |
| Intermediate key | Ed25519                       |
| Leaf key         | ECDSA P-256 (browser-fastest) |
| Signature        | Ed25519                       |
| Hash             | SHA-256                       |

**New package**: `internal/tls/ca`

**Touches**: nothing (standalone)

---

### 2.2 Trust Bootstrap (`internal/tls/trust`)

**Problem**: The root CA must be trusted by the OS and browsers.

**Design**:

- **macOS**: `/usr/bin/security add-trusted-cert` into System Keychain. Requires one-time sudo. Idempotent (detect existing CA by Subject + Key ID).
- **Linux (Debian/Ubuntu)**: Drop PEM into `/usr/local/share/ca-certificates/`, run `update-ca-certificates`.
- **Linux (Fedora/Arch)**: `/etc/pki/ca-trust/source/anchors/`, `update-ca-trust`.
- **Fallback**: Local trust store only + print clear manual instructions.
- Detection via `exec.LookPath()`.
- Uninstall command to cleanly remove trust.

**New package**: `internal/tls/trust`

**Touches**: nothing (standalone, called by CLI and daemon)

---

### 2.3 Certificate Issuer (`internal/tls/issuer`)

**Problem**: Need fast, on-demand leaf certificate issuance.

**Design**:

- Core interface:

  ```go
  type IssueRequest struct {
      DNSNames []string
      IPs      []net.IP
      ValidFor time.Duration   // default: short, auto-renewed
  }

  type Issuer interface {
      Issue(req IssueRequest) (certPEM, keyPEM []byte, err error)
      Revoke(serial string) error
  }
  ```

- SAN-only (never CN-only). DNS + IP SANs together. Auto-deduplicate.
- Wildcard support: RFC 6125 compliant, left-most label only. Always issue both `*.app.localhost` and `app.localhost`.
- Performance target: <10ms on modern hardware. In-memory with RWMutex on index.

**New package**: `internal/tls/issuer`

**Touches**: `internal/tls/ca` (consumes)

---

### 2.4 Domain Policy (`internal/tls/policy`)

**Problem**: Must prevent the local CA from issuing certs for real domains.

**Design**:

- **Allowlist** (hardcoded): `.localhost`, `.test`, `.localdev`, `.internal`, `.home.arpa`
- **Blocklist**: All IANA TLDs fetched from `https://data.iana.org/TLD/tlds-alpha-by-domain.txt` and embedded at build time via `//go:embed` (with a periodic refresh in CI).
- Wildcard depth constraint: wildcards only allowed at depth >= 2 (`*.myapp.localhost` yes, `*.localhost` no).
- All policy checks happen before any crypto operation.

**New package**: `internal/tls/policy`

**New files**: `internal/tls/policy/tlds.txt` (embedded)

**Touches**: `internal/tls/issuer` (consumes)

---

### 2.5 Daemon HTTPS Listener

**Problem**: The daemon currently only serves HTTP on port 80.

**Design**:

- Add a second listener on port 443 using `tls.NewListener`.
- On first request for a new hostname: call `issuer.Issue()` to get a cert, then cache it.
- Use `tls.Config.GetCertificate` callback for dynamic cert selection.
- HTTP listener on port 80 optionally redirects to HTTPS (configurable).
- `X-Forwarded-Proto: https` header added when proxying.

**Touches**: `cmd/daemon/main.go`

**Consumes**: `internal/tls/issuer`, `internal/tls/ca`, `internal/tls/trust`

---

### 2.6 CLI and Export Commands

- `localhost-magic tls init` — bootstrap CA + trust (one-time sudo)
- `localhost-magic tls ensure <domain>` — issue/return cert paths
- `localhost-magic tls ensure '*.myapp.localhost'` — wildcard
- `localhost-magic tls list` — show issued certs with expiry
- `localhost-magic tls revoke <domain>` — revoke a leaf cert
- `localhost-magic tls rotate` — rotate intermediate CA
- `localhost-magic tls export nginx <domain>` — emit config snippet
- `localhost-magic tls export caddy <domain>`
- `localhost-magic tls export traefik <domain>`
- `localhost-magic tls untrust` — remove root CA from OS trust store

**Touches**: `cmd/cli/main.go`

**Consumes**: all `internal/tls/*` packages

---

## Milestone 3 — Container & VM Integration

### 3.1 Docker Container Auto-Detection (`internal/discovery/docker`)

**Problem**: Docker containers listening on mapped ports are not discovered because the daemon only scans host-level ports.

**Design**:

- New discovery source alongside `portscan`:

  ```go
  type DockerDiscovery struct { /* ... */ }
  func (d *DockerDiscovery) Scan() ([]Service, error)
  ```

- Connect to Docker socket (`/var/run/docker.sock`) using the Docker Engine API (no external dependency — raw HTTP over Unix socket with `net.Dial("unix", ...)`).
- For each running container:
  - Read exposed port mappings
  - Read container labels for name hints (e.g. `localhost-magic.name=myapp`)
  - Read compose project name as group hint
  - Use container name as fallback
- Produce `ServiceRecord` entries with `TargetHost` set to the container IP (bridge network) or `127.0.0.1` (host-mapped port).
- The daemon's discovery loop calls both `portscan.Scan()` and `docker.Scan()` and merges results.
- Docker discovery is optional: if the socket doesn't exist, skip silently.

**New package**: `internal/discovery/docker`

**Touches**: `cmd/daemon` (discovery loop)

---

### 3.2 Compose-Aware Grouping

**Problem**: A `docker compose` stack with `web`, `api`, `worker` containers should naturally appear as `web.myproject.localhost`, `api.myproject.localhost`.

**Design**:

- Read `com.docker.compose.project` label to determine group.
- Read `com.docker.compose.service` label to determine name within group.
- Combine with 1.3 (subdomain grouping) so compose services automatically get subdomain names.
- Full stack name: `<service>.<project>.localhost`

**Touches**: `internal/discovery/docker`, `internal/naming`

**New files**: none (logic in docker discovery package)

---

## Milestone 4 — Peer-to-Peer Proxying

### 4.1 Peer Discovery & Federation (`internal/peer`)

**Problem**: Developers on a shared network want to access each other's services without manual configuration.

**Design**:

- Each localhost-magic instance optionally advertises itself via mDNS/DNS-SD (using `_localhost-magic._tcp.local.`).
- Discovery of peers on the local network.
- Each peer exposes a lightweight JSON API listing its services:

  ```
  GET /api/peer/services
  → [{"name": "myapp.localhost", "port": 3000, "healthy": true}, ...]
  ```

- When peer discovery is enabled, remote services appear as:

  ```
  <service>.<peer-hostname>.localhost
  ```

  e.g. `myapp.alice-macbook.localhost` routes to Alice's machine.

- Peer identity verified via TLS client certificates (issued by the local CA from Milestone 2).
- Security: opt-in only, requires explicit `localhost-magic peer enable`.

**New package**: `internal/peer`

**Touches**: `cmd/daemon` (proxy routing, new API endpoint), `cmd/cli` (peer commands)

---

### 4.2 Selective Sharing

- `localhost-magic peer share myapp.localhost` — expose a specific service to peers
- `localhost-magic peer unshare myapp.localhost` — stop sharing
- `localhost-magic peer list` — show discovered peers and their services
- Default: nothing shared. Must be explicitly opted in per-service.

**Touches**: `cmd/cli`, `internal/peer`, `internal/storage` (add `Shared bool` to ServiceRecord)

---

## Milestone 5 — Observability & Dashboard

### 5.1 Connection & I/O Metrics (`internal/metrics`)

**Problem**: No visibility into proxy traffic.

**Design**:

- Wrap the `httputil.ReverseProxy` transport to capture:
  - Active connection count per service
  - Request count (total, per minute)
  - Bytes in / bytes out
  - Response time (p50, p95, p99)
  - Status code distribution
- Store in a ring buffer (last N minutes), no external dependency.
- Expose via API:

  ```
  GET /api/services/<name>/metrics
  ```

**New package**: `internal/metrics`

**Touches**: `cmd/daemon` (proxy wrapping, API endpoint)

---

### 5.2 Dashboard Activity Indicators

**Problem**: Dashboard shows static health dots; no real-time traffic info.

**Design**:

- Add to each service row:
  - Live connection count badge
  - Bytes transferred (human-readable, e.g. "12.3 KB/s")
  - Sparkline or activity dot that pulses on traffic
- Use SSE (`text/event-stream`) endpoint for real-time updates:

  ```
  GET /api/events
  ```

- Dashboard JS subscribes to SSE and updates DOM.

**Touches**: `cmd/daemon` (dashboard HTML/JS, new SSE endpoint)

**Consumes**: `internal/metrics`

---

### 5.3 Traffic Inspector

**Problem**: Developers want to see request/response details for debugging, similar to browser DevTools Network tab.

**Design**:

- Opt-in per service: `localhost-magic inspect myapp.localhost`
- When enabled, the proxy records:
  - Request method, URL, headers
  - Response status, headers, timing
  - Body preview (first 4KB, text only)
- Ring buffer of last 500 requests per inspected service.
- Dashboard gets a new "Inspector" panel with:
  - Filterable request list
  - Request/response detail view
  - Export as HAR

**New package**: `internal/inspector`

**Touches**: `cmd/daemon` (proxy wrapping, dashboard, API), `cmd/cli`

---

## Milestone 6 — System Integration

### 6.1 systemd / launchd Auto-Start

**Problem**: Users must manually start the daemon with sudo.

**Design**:

- `localhost-magic install` command:
  - **macOS**: Generate and load a `com.localhost-magic.daemon.plist` into `/Library/LaunchDaemons/`
  - **Linux**: Generate and enable a `localhost-magic.service` systemd unit
- `localhost-magic uninstall` — reverse the above
- `localhost-magic status` — check if daemon is running (via PID file or systemd/launchctl query)
- The daemon writes a PID file at `/var/run/localhost-magic.pid`

**New files**: `internal/system/launchd.go`, `internal/system/systemd.go`

**Touches**: `cmd/cli`

---

### 6.2 System Notifications (`internal/notify`)

**Problem**: New services appear silently; users only notice via dashboard.

**Design**:

- Emit desktop notifications on:
  - New service discovered (with clickable link)
  - Service went offline
  - TLS certificate about to expire (Milestone 2)
  - Peer connected/disconnected (Milestone 4)
- **macOS**: `osascript` or UserNotifications framework via cgo-free approach
- **Linux**: `notify-send` (freedesktop)
- Notification preferences in config (enable/disable per event type)

**New package**: `internal/notify`

**Touches**: `cmd/daemon` (emit notifications from discovery loop)

---

### 6.3 Optional GUI Wrapper

**Problem**: Some users prefer a tray/menubar app over CLI.

**Design**:

- Thin native wrapper that:
  - Shows a menubar/tray icon (macOS: NSStatusItem, Linux: AppIndicator)
  - Lists services in a dropdown menu
  - Opens dashboard in default browser on click
  - Shows notification badge for new services
- Communicates exclusively via the existing REST API (port 80)
- Separate binary: `localhost-magic-gui`
- No additional dependencies on the daemon side.

**New directory**: `cmd/gui/`

**Touches**: nothing in core (consumes REST API only)

---

## Milestone 7 — Advanced Routing

### 7.1 Custom TLD Support

**Problem**: `.localhost` is convenient but some tools or environments need other TLDs.

**Design**:

- Config option:

  ```json
  { "tlds": [".localhost", ".test", ".localdev"] }
  ```

- For non-`.localhost` TLDs, provide a lightweight DNS resolver:
  - Listen on `127.0.0.1:53` (configurable)
  - Respond to A/AAAA queries for configured TLDs with `127.0.0.1`
  - Forward everything else upstream
- `localhost-magic dns enable` — configure system DNS to use local resolver
- `localhost-magic dns disable` — revert
- On macOS: create `/etc/resolver/<tld>` files (no system DNS change needed)
- On Linux: configure via `systemd-resolved` split DNS or `/etc/resolver`

**New package**: `internal/dns`

**Touches**: `cmd/daemon`, `cmd/cli`, `internal/tls/policy` (must allow configured TLDs)

---

## Dependency Graph

```
Milestone 1 (Core Hardening)
  1.1 Persistent Blacklist     ─── no deps
  1.2 Naming Heuristics        ─── no deps
  1.3 Subdomain Grouping       ─── no deps (benefits from 1.2)

Milestone 2 (TLS)
  2.1 CA Management            ─── no deps
  2.2 Trust Bootstrap          ─── depends on 2.1
  2.3 Certificate Issuer       ─── depends on 2.1
  2.4 Domain Policy            ─── no deps (consumed by 2.3)
  2.5 Daemon HTTPS Listener    ─── depends on 2.2, 2.3, 2.4
  2.6 CLI & Export             ─── depends on 2.1, 2.2, 2.3

Milestone 3 (Containers)
  3.1 Docker Auto-Detection    ─── no deps
  3.2 Compose Grouping         ─── benefits from 1.3, depends on 3.1

Milestone 4 (Peer-to-Peer)
  4.1 Peer Discovery           ─── benefits from Milestone 2 (mTLS)
  4.2 Selective Sharing        ─── depends on 4.1

Milestone 5 (Observability)
  5.1 Connection Metrics       ─── no deps
  5.2 Dashboard Indicators     ─── depends on 5.1
  5.3 Traffic Inspector        ─── depends on 5.1

Milestone 6 (System Integration)
  6.1 Auto-Start               ─── no deps
  6.2 Notifications            ─── no deps
  6.3 GUI Wrapper              ─── no deps (consumes REST API)

Milestone 7 (Advanced Routing)
  7.1 Custom TLD Support       ─── benefits from Milestone 2 (TLS for custom TLDs)
```

---

## Package Layout (target state)

```
cmd/
  cli/main.go
  daemon/main.go
  gui/main.go                          (Milestone 6.3)

internal/
  naming/
    names.go                           (existing)
    rules.go                           (Milestone 1.2)
    rules_builtin.json                 (Milestone 1.2)
  portscan/
    types.go, scan_darwin.go, scan_linux.go   (existing)
  probe/
    http.go                            (existing)
  storage/
    store.go                           (existing)
    blacklist.go                       (Milestone 1.1)
  tls/
    ca/ca.go                           (Milestone 2.1)
    trust/trust.go                     (Milestone 2.2)
    trust/trust_darwin.go
    trust/trust_linux.go
    issuer/issuer.go                   (Milestone 2.3)
    policy/policy.go                   (Milestone 2.4)
    policy/tlds.txt
  discovery/
    docker/docker.go                   (Milestone 3.1)
  peer/
    peer.go                            (Milestone 4.1)
    mdns.go
  metrics/
    metrics.go                         (Milestone 5.1)
  inspector/
    inspector.go                       (Milestone 5.3)
  notify/
    notify.go                          (Milestone 6.2)
    notify_darwin.go
    notify_linux.go
  dns/
    resolver.go                        (Milestone 7.1)
  system/
    launchd.go                         (Milestone 6.1)
    systemd.go                         (Milestone 6.1)
```

---

## Parallelism Guide

The following workstreams can proceed **simultaneously** with no coordination:

| Workstream | Items | Interface boundary |
|---|---|---|
| **A: Core/Storage** | 1.1, 1.2 | `internal/storage`, `internal/naming` |
| **B: TLS CA + Policy** | 2.1, 2.4 | `internal/tls/ca`, `internal/tls/policy` |
| **C: TLS Trust** | 2.2 (after 2.1) | `internal/tls/trust` |
| **D: TLS Issuer** | 2.3 (after 2.1) | `internal/tls/issuer` |
| **E: Docker** | 3.1 | `internal/discovery/docker` |
| **F: Metrics** | 5.1 | `internal/metrics` |
| **G: System** | 6.1, 6.2 | `internal/system`, `internal/notify` |

Once A completes, 1.3 can start. Once B+C+D complete, 2.5 and 2.6 can start. These are the only serialization points.

---

## Design Principles

1. **Single binary**: All features compile into one binary. Optional features are enabled via config, not separate processes.
2. **No external runtime dependencies**: Pure Go. No shelling out except for OS trust bootstrap (unavoidable).
3. **Clean package boundaries**: Each `internal/` package has a Go interface. The daemon and CLI consume interfaces, never concrete types from other packages.
4. **Backward compatible storage**: New fields use `omitempty`. Old config files work without migration.
5. **Opt-in complexity**: Advanced features (TLS, peer, inspector) are off by default. The zero-config experience remains unchanged.
6. **Dev-safe by default**: The TLS CA cannot issue certs for real domains. Peer sharing requires explicit opt-in. No data leaves the machine unless the user asks.
