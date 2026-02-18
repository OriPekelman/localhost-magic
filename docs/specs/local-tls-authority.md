# Local Dev TLS Authority

> Roadmap reference: Milestone 2

## 0. Design Goals (non-negotiable)

Enable zero-effort management of HTTP processes running locally, including testing TLS interactions: HSTS, iframes, content policy, subdomains, secure-only cookies, mixed-content issues, and services that require TLS.

* **Single static binary**
* **No external runtime dependencies**
* **No manual OpenSSL / mkcert install**
* **Zero-click trust bootstrap (once)**
* **Automatic cert issuance for new domains**
* **Wildcard + SAN support**
* **Fast (<10ms issuance)**
* **Safe-by-default (dev-only, constrained scope)**

---

## 1. High-Level Architecture

```
┌────────────────────────────┐
│ localhost-magic (Go binary) │
│                            │
│ ┌───────────────┐          │
│ │ Root CA       │◀────┐    │
│ │ (self-managed)│     │    │
│ └───────────────┘     │    │
│        │              │    │
│ ┌───────────────┐     │    │
│ │ Intermediate  │─────┘    │
│ │ CA            │          │
│ └───────────────┘          │
│        │                   │
│ ┌───────────────┐          │
│ │ Cert Issuer   │          │
│ └───────────────┘          │
│        │                   │
│ ┌───────────────┐          │
│ │ Trust Manager │───OS────▶│
│ └───────────────┘          │
│                            │
│ Optional: HTTP API / CLI   │
└────────────────────────────┘
```

---

## 2. Cryptography Choices

| Item             | Choice                        | Rationale                  |
|------------------|-------------------------------|----------------------------|
| Root key         | Ed25519                       | Fast, small keys           |
| Intermediate key | Ed25519                       | Fast, small keys           |
| Leaf key         | ECDSA P-256 (browser fastest) | Universal browser support  |
| Signature        | Ed25519                       | No OpenSSL dependency      |
| Hash             | SHA-256                       | Universal trust            |

### Certificate Lifetimes

| Certificate   | Validity  | Renewal                                |
|---------------|-----------|----------------------------------------|
| Root CA       | 10 years  | Explicit rotation command              |
| Intermediate  | 1 year    | Auto-rotated 30 days before expiry     |
| Leaf certs    | 24 hours  | Auto-renewed on next request           |

Short leaf lifetimes are safe because issuance is instant and local. This minimizes the window of exposure if a cert file leaks.

The root CA should have an explicit rotation method. Trusted certs should not linger indefinitely.

---

## 3. Trust Bootstrap (critical path)

### macOS

* Install root CA into **System Keychain** with trust level `Always Trust`
* Implementation: `/usr/bin/security add-trusted-cert`
* Requires **one-time sudo**
* Idempotent: detect existing CA by Subject + Key ID before installing

### Linux

Support tiers:

1. **Debian / Ubuntu**
   * Drop PEM into `/usr/local/share/ca-certificates/`
   * Run `update-ca-certificates`

2. **Fedora / Arch**
   * `/etc/pki/ca-trust/source/anchors/`
   * `update-ca-trust`

3. **Fallback**
   * Local trust store only
   * Print clear instructions for manual installation

Detection:

```go
if _, err := exec.LookPath("update-ca-certificates"); err == nil {
    // Debian/Ubuntu path
} else if _, err := exec.LookPath("update-ca-trust"); err == nil {
    // Fedora/Arch path
} else {
    // Fallback
}
```

### Uninstall

`localhost-magic tls untrust` must cleanly remove the root CA from the OS trust store and print confirmation.

---

## 4. Storage Layout

Everything under one directory:

```
~/.localtls/
├── root_ca.pem
├── root_ca.key           (0600)
├── intermediate.pem
├── intermediate.key      (0600)
├── certs/
│   ├── myapp.localhost.pem
│   ├── myapp.localhost.key
│   └── _wildcard.myapp.localhost.pem
│   └── _wildcard.myapp.localhost.key
├── index.json            (cert inventory: serial, subject, expiry, revoked)
└── config.json
```

Rules:

* Keys: `0600`
* Certs: `0644`
* All writes are atomic (write temp file, then rename)
* No password prompts after bootstrap

---

## 5. Domain Policy (dev-safe)

### Allowed TLDs (hardcoded allowlist)

```
.localhost
.test
.localdev
.internal
.home.arpa
```

### Blocked TLDs

All existing IANA TLDs, fetched from `https://data.iana.org/TLD/tlds-alpha-by-domain.txt` and embedded at build time via `//go:embed`.

Explicitly includes but is not limited to: `.com`, `.dev`, `.io`, `.org`, `.net`

### Wildcard Constraints

* Wildcards allowed **only at depth >= 2**:
  * `*.myapp.localhost` — allowed
  * `*.localhost` — **blocked**
* Only left-most label can be wildcard (RFC 6125)

This prevents accidental misuse where a local CA cert could be confused with a real domain cert.

---

## 6. Certificate Issuance API

### Core Go Interface

```go
// Package issuer provides local certificate issuance.

type IssueRequest struct {
    DNSNames []string
    IPs      []net.IP
    ValidFor time.Duration  // 0 = use default (24h)
}

type Certificate struct {
    CertPEM  []byte
    KeyPEM   []byte
    Serial   string
    NotAfter time.Time
    CertPath string  // filesystem path
    KeyPath  string  // filesystem path
}

type Issuer interface {
    Issue(req IssueRequest) (*Certificate, error)
    Get(domain string) (*Certificate, error)     // returns cached if valid
    Ensure(domain string) (*Certificate, error)  // issue if missing/expired
    Revoke(serial string) error
    List() ([]*Certificate, error)
}
```

### SAN Rules

* Always use SAN (never CN-only; CN is set for display purposes only)
* DNS + IP SANs allowed together
* Deduplicate automatically
* When issuing a wildcard, always include both:
  * `DNS:*.myapp.localhost`
  * `DNS:myapp.localhost`

---

## 7. Automation Modes

### Mode A: CLI (default)

```bash
# Issue or return existing cert
localhost-magic tls ensure myapp.localhost

# Output:
# CERT=/Users/you/.localtls/certs/myapp.localhost.pem
# KEY=/Users/you/.localtls/certs/myapp.localhost.key

# Wildcard
localhost-magic tls ensure '*.myapp.localhost'

# List all issued certs
localhost-magic tls list

# Revoke
localhost-magic tls revoke myapp.localhost
```

### Mode B: Embedded in Daemon

When the daemon has TLS enabled, it automatically:

1. Issues a cert on first HTTPS request for a new hostname
2. Caches the cert via `tls.Config.GetCertificate`
3. Renews before expiry

No user action needed after initial `localhost-magic tls init`.

### Mode C: HTTP API (optional)

```
POST /api/tls/issue
{"dns": ["api.myapp.localhost"]}

→ {"cert_path": "...", "key_path": "...", "expires": "..."}
```

Useful for proxies, container sidecars, editor plugins.

---

## 8. Reverse Proxy Config Export

First-class support for generating config snippets:

```bash
localhost-magic tls export nginx myapp.localhost
```

Output:

```nginx
ssl_certificate     /Users/you/.localtls/certs/myapp.localhost.pem;
ssl_certificate_key /Users/you/.localtls/certs/myapp.localhost.key;
```

```bash
localhost-magic tls export caddy myapp.localhost
```

Output:

```
tls /Users/you/.localtls/certs/myapp.localhost.pem /Users/you/.localtls/certs/myapp.localhost.key
```

```bash
localhost-magic tls export traefik myapp.localhost
```

Output:

```yaml
tls:
  certificates:
    - certFile: /Users/you/.localtls/certs/myapp.localhost.pem
      keyFile: /Users/you/.localtls/certs/myapp.localhost.key
```

---

## 9. Concurrency & Performance

* Cert issuance is in-memory (crypto ops only, no disk I/O in hot path)
* Lock strategy: RWMutex on cert index; write lock only during issuance
* Typical issuance: ~1-3ms on M1 Mac
* Safe for hot reload, file watchers, IDE plugins
* Concurrent requests for the same domain coalesce (singleflight)

---

## 10. Security Constraints

* Root key never leaves local machine
* No ACME protocol
* No external network calls (policy TLD list embedded at build time)
* No renewal daemon needed (certs renewed lazily on access)
* Certificates auto-rotated on expiry
* Revocation tracked in `index.json` (local CRL, not network-distributed)

---

## 11. Explicit Non-Goals

* Public trust (this CA is intentionally local-only)
* Internet-facing TLS termination
* Let's Encrypt / ACME compatibility
* Windows support (can be added later as a separate trust backend)

---

## 12. Why Not Embed mkcert Directly

* mkcert is CLI-oriented with no stable Go API
* OS trust logic is not exported cleanly
* Would still need: key management, domain policy, automation, lifecycle
* Reimplementing ~20% of mkcert gives:
  * 100% control over behavior
  * Cleaner UX integration
  * Easier automation
  * Smaller attack surface

---

## 13. Outcome

After setup (`localhost-magic tls init`, one-time sudo):

* `docker compose up` with HTTPS works
* New subdomain appears, cert auto-issued
* Browser shows real lock icon
* OAuth callbacks, secure cookies, HSTS behave exactly like production
* Mixed-content warnings caught in dev, not staging
* No developer ever thinks about certs again

---

## 14. Implementation Packages

| Package | Responsibility | Dependencies |
|---|---|---|
| `internal/tls/ca` | Root + intermediate CA lifecycle | stdlib `crypto/*` only |
| `internal/tls/trust` | OS trust store integration | `internal/tls/ca`, `os/exec` |
| `internal/tls/policy` | Domain validation + TLD blocklist | none |
| `internal/tls/issuer` | Leaf cert issuance + caching | `internal/tls/ca`, `internal/tls/policy` |

All packages expose interfaces. The daemon and CLI depend on interfaces, not implementations.
