# xrok

**Xfer Routes Over Khaos** — multiplex tunnels over TLS, proxy through SOCKS5, punch reverse ports, share files. When chaos stands between you and your destination, xrok finds a route. Your infrastructure, your rules.

## Features

### Core Features
- **Secure Tunneling**: Expose local applications through a secure TLS connection
- **Subdomain Routing**: Each tunnel gets a unique subdomain (e.g., `myapp.yourdomain.com`)
- **Unified Binary**: Single `xrok` binary for both server and client modes
- **Auto-Reconnect**: Automatic reconnection with exponential backoff on connection loss

### Advanced Features
- **Multiple Tunnels**: Support multiple tunnels per client connection
- **SOCKS5 Proxy**: Built-in SOCKS5 proxy server for routing traffic through the tunnel
- **UDP Tunneling**: Forward UDP packets through TCP tunnel (e.g., DNS queries)
- **Yamux Multiplexing**: Efficient connection multiplexing for better performance
- **Wildcard TLS**: Support for wildcard certificates (`*.yourdomain.com`)
- **Custom Headers**: Inject custom HTTP headers into all proxied requests
- **HTTP Basic Auth**: Protect individual tunnels with username/password
- **WebSocket Compression**: Automatic permessage-deflate compression for reduced bandwidth
- **Request Logging**: Detailed logging of all incoming requests
- **HTTP CONNECT Proxy**: Connect through corporate HTTP proxies
- **mTLS**: Mutual TLS authentication with client certificates
- **Stdio Tunneling**: SSH ProxyCommand support for transparent SSH access

### QUIC Transport

Use QUIC for better performance on lossy networks:

```bash
# Client with QUIC transport
xrok client --server example.com --tunnel app:localhost:8080 --quic

# Config file
transport: quic
```

QUIC provides:
- Built-in encryption (TLS 1.3)
- Multiplexed streams without head-of-line blocking
- Better performance on high-latency/lossy networks
- Faster connection establishment (0-RTT)

### P2P Direct Connections

Enable direct peer-to-peer connections between clients:

```bash
# Enable P2P mode
xrok client --server example.com --tunnel app:localhost:8080 --p2p

# With custom STUN server
xrok client --server example.com --tunnel app:localhost:8080 --p2p --stun-server stun.example.com:3478

# Config file
p2p:
  enabled: true
  stun_server: stun.l.google.com:19302
  fallback: true  # Fallback to relay if P2P fails
  turn:
    enabled: true
    server: turn.example.com:5349
    username: client-a
    password: "use-a-runtime-secret"
    transport: tls # UDP relay over a UDP/TCP/TLS control connection
    ca_cert: /etc/xrok/turn-ca.pem
    server_name: turn.example.com
    channel_bind: true
```

The equivalent CLI uses `--turn-server`, `--turn-username`,
`--turn-transport`, `--turn-ca-cert`, `--turn-server-name` and
`--turn-channel-bind`. Prefer `XROK_TURN_PASSWORD` over exposing the password
in process arguments.

P2P mode:
- Uses server for signaling only
- Data flows directly between clients
- Reduces latency and server bandwidth
- Exchanges bilateral TURN relay candidates when direct traversal fails
- Falls back to the normal xrok server data path if TURN is unavailable

The client runtime allocates an RFC 5766 candidate before its authenticated ICE
offer/answer, but binds the peer permission only after direct checks fail. Both
peers exchange independently allocated relay candidates and nominate only a
relay-to-relay pair after an authenticated connectivity exchange. It supports
long-term authentication, permissions, refresh,
bounded retransmission, Send/Data indications, optional ChannelBind/ChannelData
and UDP/TCP/TLS control connections. TLS verifies certificates by default.
After nomination, session-bound consent request/response records continuously
verify liveness; application datagrams are protected with AES-256-GCM and a
bounded replay window derived from both authenticated peer sessions and the ICE
negotiation ID.

This is a dependency-free ICE-style runtime, not a complete RFC 8445 stack.
TURN TCP/TLS here means the client-to-coturn control connection; the allocated
peer relay remains UDP. RFC 6062 TCP allocations and TURN-over-TLS peer data are
not claimed.

The local `turn-integration.sh` lab validates bilateral allocation, ChannelData,
deallocation and UDP/TCP/TLS control transports. Its coturn relay address is
locally reachable; reproducing internet symmetric-NAT routing additionally
requires coturn `external-ip` to advertise an address reachable from both test
namespaces or VMs. That deployment boundary is intentionally separate from the
fully local protocol/lifecycle acceptance tests.

#### P2P Server APIs

The server provides APIs for P2P coordination:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/p2p/clients` | GET | List all P2P-enabled clients with their info |
| `/p2p/tunnel-owner?tunnel=name` | GET | Get client ID and P2P info for a tunnel |
| `/p2p/discover?from=id&to=id` | GET | Trigger P2P discovery between two clients |

**Example: List P2P clients**
```bash
curl https://server.com/p2p/clients
# Returns: [{"client_id":"...", "tunnels":["app"], "p2p_enabled":true, "p2p_info":{...}}]
```

**Example: Get tunnel owner**
```bash
curl https://server.com/p2p/tunnel-owner?tunnel=myapp
# Returns: {"tunnel":"myapp", "client_id":"...", "p2p_enabled":true, "p2p_info":{...}}
```

### Server Features
- **API Token Authentication**: Secure client registration with tokens
- **Rate Limiting**: Configurable rate limits per client
- **Prometheus Metrics**: Built-in metrics endpoint for monitoring
- **Graceful Shutdown**: Clean shutdown with active connection draining
- **Request Logging**: Logs all incoming requests with client IP, method, path, and status

## Requirements

- **Go**: Version 1.21 or higher
- **TLS Certificates**: Required for secure connections (Let's Encrypt recommended)
- **Domain Name**: A domain with DNS pointing to your server
- **Wildcard DNS** (optional): For subdomain-based routing (`*.yourdomain.com`)

## Installation

### Building from Source

```bash
git clone https://github.com/imhassla/xrok.git
cd xrok
go build -o xrok .
```

### Cross-compilation for Linux

```bash
GOOS=linux GOARCH=amd64 go build -o xrok-linux .
```

## Server Setup

### 1. TLS Certificates

Generate certificates using Let's Encrypt with DNS challenge for wildcard support:

```bash
# Install certbot with Route53 plugin (for AWS)
sudo apt-get install certbot python3-certbot-dns-route53

# Get wildcard certificate
sudo certbot certonly --dns-route53 \
  -d yourdomain.com \
  -d '*.yourdomain.com' \
  --agree-tos \
  --email admin@yourdomain.com
```

Or use standalone mode for single domain:

```bash
sudo certbot certonly --standalone -d yourdomain.com
```

### 2. DNS Configuration

Set up DNS records:
- `yourdomain.com` → A record → `your-server-ip`
- `*.yourdomain.com` → A record → `your-server-ip` (for subdomain routing)

### 3. Running the Server

```bash
sudo ./xrok server -domain yourdomain.com
```

**Server Options:**

| Flag | Description | Default |
|------|-------------|---------|
| `-domain` | Domain name for tunnel URLs (required) | - |
| `-debug` | Enable debug logging | false |
| `-request-log` | Enable HTTP request logging | false |
| `-client-ca` | CA certificate for mTLS client verification | - |

**Environment Variables:**

| Variable | Description |
|----------|-------------|
| `XROK_API_TOKEN` | API token for client authentication |
| `XROK_RATE_LIMIT` | Rate limit per client (requests/second) |

**Server Ports:**

| Port | Purpose |
|------|---------|
| 443 | Unified HTTPS (registration, proxy, metrics, health) |

## Client Usage

### Basic Usage

Expose a local application:

```bash
./xrok client --server yourdomain.com --tunnel myapp:localhost:8080
```

Your application will be available at: `https://myapp.yourdomain.com`

### Multiple Tunnels

Expose multiple services with one connection:

```bash
./xrok client --server yourdomain.com \
  --tunnel web:localhost:8080 \
  --tunnel api:localhost:3000 \
  --tunnel admin:localhost:9000
```

### SOCKS5 Proxy

Route traffic through the tunnel server:

```bash
./xrok client --server yourdomain.com --socks5 :1080

# Use with curl
curl -x socks5://127.0.0.1:1080 http://example.com
```

### UDP Tunneling

Forward UDP traffic (e.g., DNS):

```bash
./xrok client --server yourdomain.com --udp :5353:8.8.8.8:53

# Test DNS through tunnel
dig @127.0.0.1 -p 5353 google.com
```

### File Sharing

Share a local folder via HTTPS:

```bash
# Using built-in file server (auto-picks port)
./xrok client --server yourdomain.com --id myfiles --folder /path/to/share

# Or with explicit tunnel name
./xrok client --server yourdomain.com --tunnel files:localhost:8000 --folder /path/to/share
```

Access at: `https://myfiles.yourdomain.com` - provides directory listing and file downloads.

### Reverse Port Forwarding

Expose a port on the server that forwards to your local machine:

```bash
# Listen on server port 9000, forward to local port 3000
./xrok client --server yourdomain.com -R 9000:localhost:3000

# Multiple reverse tunnels
./xrok client --server yourdomain.com \
  -R 9000:localhost:3000 \
  -R 9001:localhost:3001
```

Users connecting to `yourdomain.com:9000` will be forwarded to your local `localhost:3000`.

### Local HTTP Proxy

Create a local HTTP proxy that routes all traffic through the tunnel:

```bash
./xrok client --server yourdomain.com --proxy

# Then use with curl
export http_proxy=http://127.0.0.1:8080
curl http://example.com  # Goes through tunnel server
```

### Custom Headers via CLI

```bash
./xrok client --server yourdomain.com \
  --tunnel app:localhost:8080 \
  -header "X-Custom-App:MyApp" \
  -header "X-Environment:production" \
  -compress
```

### Versioned standard tunnel transports

Use `--transport auto` to try native QUIC, then HTTP/2, then framed WSS at connection
establishment and reconnection. Each attempt has the configured `--connect-timeout`;
selection logs report the failed stage and selected carrier with credentials redacted.
An explicit TCP upstream proxy excludes QUIC entirely: automatic selection then tries
HTTP/2 and WSS through that proxy. Missing advertised capabilities are skipped; auto
does not silently downgrade to the legacy carrier on old servers. Configure `legacy`
explicitly for those deployments. Native QUIC auto-selection uses a bounded,
versioned, acknowledged registration association. Once a carrier is established,
failure triggers normal re-registration/reconnection, not replay of application
requests or seamless migration of active TCP streams. Explicit `quic`, `http2`, and
`wss` choices remain pinned to the requested carrier.

Select `--transport http2` for a full-duplex HTTP/2 POST carrier, or `--transport wss`
for a real RFC 6455 binary-message carrier. Both carry the existing multiplexed
streams and work through HTTP-aware reverse proxies; WSS negotiates the `xrok.v2`
subprotocol. The corresponding YAML setting is `transport: http2` or `transport: wss`.
The server advertises supported carriers during registration. An explicit unsupported
choice fails rather than silently choosing a different route or disabling TLS.

These endpoints require a registration's session token in `X-Xrok-Session` and
`X-Xrok-Tunnel-Version: 2`. They share the existing control-plane TLS/mTLS policy.
HTTP/HTTPS CONNECT and SOCKS5 upstream proxy settings also apply to these carriers;
target and proxy TLS remain separately verified. The establishment timeout ends after
the handshake; it does not cap a healthy carrier's lifetime. Cancellation closes the
carrier and its streams. The byte adapters do not provide a carrier write-deadline API;
individual yamux stream deadlines remain available.

The default remains `legacy`: the existing raw byte stream following HTTP Upgrade,
retained for older deployments. Historical `tcp`, `tls`, and `websocket` profile values
remain legacy aliases. `--quic` and `--transport quic` select the existing native QUIC
carrier. Use explicit `http2` or `wss` with a new server to gain standard intermediary
compatibility; the legacy carrier does not become framed by renaming it.

### HTTP/2 and gRPC application ingress

The public HTTPS listener negotiates HTTP/2 or HTTP/1.1. A named tunnel can expose
a local plaintext gRPC server without changing the tunnel client's wire protocol:

```bash
xrok client --tunnel rpc:localhost:50051
```

Connect a standard TLS gRPC client to the announced `rpc` authority (including its
port). The public connection uses HTTP/2; the encrypted xrok tunnel carries HTTP/2
to the local plaintext (`h2c`, prior knowledge) gRPC backend. Unary, server/client
streaming and bidirectional calls preserve binary metadata, response trailers,
deadlines and cancellation. Maximum application message sizes remain the gRPC
client/server's settings; xrok copies streaming bodies instead of buffering them.
TLS-enabled local gRPC backends are not configured by this mode.

Ordinary HTTP/2 requests can reach existing HTTP/1.1 applications. Existing
HTTP/1.1 upgrades and legacy tunnel carriers retain their paths. Basic Auth and
tunnel capability checks still apply. Native gRPC uses `application/grpc` (or a
`+proto`/`+json` variant); this is not a gRPC-Web translation gateway. An unavailable
tunnel returns gRPC status `UNAVAILABLE` rather than an HTML success response.

### Upstream proxy routes

Use `--http-proxy` to route registration, control, mux, data, and stdio connections
through the same upstream. The historical option name accepts HTTP CONNECT,
HTTPS-to-proxy, and SOCKS5 URLs:

```bash
./xrok client --server yourdomain.com --tunnel app:localhost:8080 \
  --http-proxy http://127.0.0.1:3333 --proxy-session app-session \
  --connect-timeout 15s
```

With ProxyMachine, configure `selectionPolicy: stable` and
`stickyHeader: X-Proxy-Session` on the relay. The opaque `--proxy-session` marker is
sent on each HTTP CONNECT, including registration and reconnects; it is not an origin
application header or an API token. `--proxy-session-header` selects a different relay
header when needed. Marker length is limited to 256 printable ASCII bytes. Prefer a
protected client YAML file for credentials and stable session values.

| Proxy scheme | Proxy hop | Target DNS | Default port |
| --- | --- | --- | --- |
| `http` | Plain HTTP CONNECT | Proxy | 80 |
| `https` | Verified proxy TLS, then CONNECT | Proxy | 443 |
| `socks5` | SOCKS5 with optional username/password | Local resolver | 1080 |
| `socks5h` | SOCKS5 with optional username/password | Proxy | 1080 |

Target TLS remains verified inside the proxy tunnel. `--proxy-ca-cert` adds a CA for
an HTTPS proxy independently of `--ca-cert` for the xrok server; client mTLS identity
is presented to the xrok server, not to the proxy. Proxy userinfo and known session/API
secrets are redacted from route logs. Affinity markers require an HTTP CONNECT relay;
SOCKS5 has no HTTP header channel.

`--connect-timeout` bounds connection establishment, including proxy negotiation and
target TLS. Successful long-lived connections outlive that budget. Cancellation stops
pending handshakes and retry waits. A new server advertises its multiplexed protocol,
so a new client avoids the redundant legacy control connection; older registration
responses retain the previous fallback behavior.

QUIC and P2P use direct network paths and cannot satisfy this TCP proxy routing
requirement. xrok rejects an upstream proxy combined with `--quic` or `--p2p` before
registration instead of silently bypassing the proxy. UDP payloads forwarded through
an established TCP mux are a separate feature and do not imply proxied QUIC support.

The equivalent client YAML fields are `http_proxy`, `proxy_session`,
`proxy_session_header`, `connect_timeout`, and `proxy_ca_cert`. For public HTTPS on
443 behind a shared listener, see the [SNI ingress guide](deploy/ingress.md).

### Combined Example

```bash
./xrok client --server yourdomain.com \
  --tunnel app:localhost:8080 \
  --socks5 :1080 \
  --udp :5353:8.8.8.8:53 \
  -compress \
  --debug
```

**Client Options:**

| Flag | Description | Default |
|------|-------------|---------|
| `--server` | Server address (required) | - |
| `--tunnel` | Tunnel spec `name:target` (repeatable) | - |
| `-R` | Reverse forwarding `remote:local` (repeatable) | - |
| `--port` | Legacy: single tunnel target | - |
| `--folder` | Serve local directory | - |
| `--proxy` | Use local HTTP proxy | false |
| `--socks5` | SOCKS5 proxy address (e.g., `:1080`) | - |
| `--udp` | UDP tunnel `local:remote` (e.g., `:5353:8.8.8.8:53`) | - |
| `--tls` | Use TLS connections | true |
| `--token` | API token for authentication | - |
| `--id` | Custom client ID | - |
| `-config` | YAML configuration file | - |
| `-header` | Add custom header `Name:Value` (repeatable) | - |
| `-compress` | Enable WebSocket compression | false |
| `-http-proxy` | HTTP/HTTPS CONNECT or SOCKS5 upstream URL | - |
| `--connect-timeout` | Connection establishment budget | 30s |
| `--proxy-session` | Opaque HTTP relay affinity marker | - |
| `--proxy-session-header` | HTTP relay affinity header | X-Proxy-Session |
| `--proxy-ca-cert` | CA bundle for HTTPS proxy verification | system roots |
| `-client-cert` | Client certificate for mTLS | - |
| `-client-key` | Client private key for mTLS | - |
| `-stdio` | Run in stdio mode for SSH ProxyCommand | false |
| `-stdio-target` | Target host:port for stdio mode | - |
| `--reconnect` | Enable auto-reconnect | true |
| `--max-retries` | Max reconnect attempts (0=unlimited) | 0 |
| `--retry-interval` | Initial retry interval | 1s |
| `--max-interval` | Maximum retry interval | 5m |
| `--quic` | Use QUIC transport | false |
| `--p2p` | Enable P2P connections | false |
| `--stun-server` | STUN server address | stun.l.google.com:19302 |
| `--p2p-fallback` | Fallback to relay if P2P fails | true |
| `--turn-server` | TURN server `host:port`; enables ICE TURN fallback | - |
| `--turn-username` | TURN long-term username | - |
| `--turn-password` | TURN password; prefer `XROK_TURN_PASSWORD` | env/none |
| `--turn-transport` | TURN control transport: `udp`, `tcp`, or `tls` | udp |
| `--turn-ca-cert` | Additional CA file for TURN TLS | system roots |
| `--turn-server-name` | TURN TLS certificate server name | server host |
| `--turn-insecure` | Disable TURN TLS verification for isolated labs | false |
| `--turn-channel-bind` | Prefer ChannelData with indication fallback | false |
| `--turn-timeout` | TURN transaction timeout | 5s |
| `--turn-refresh-interval` | Allocation/permission refresh interval | derived |
| `--turn-max-datagram` | Maximum TURN payload bytes | 65046 |
| `--turn-retransmits` | Bounded UDP transaction retransmits | 3 |
| `--debug` | Enable debug logging | false |

### Configuration File

Instead of command-line flags, you can use a YAML configuration file:

```bash
./xrok client -config client.yaml
```

**Example `client.yaml`:**

```yaml
# Server connection
server: yourdomain.com
tls: true

# Transport protocol (optional)
transport: quic  # Use QUIC for better performance on lossy networks

# P2P configuration (optional)
p2p:
  enabled: true
  stun_server: stun.l.google.com:19302
  fallback: true  # Fallback to relay if P2P fails
  turn:
    enabled: true
    server: turn.example.com:5349
    username: client-a
    password: use-a-runtime-secret
    transport: tls
    ca_cert: /etc/xrok/turn-ca.pem
    server_name: turn.example.com
    channel_bind: true

# Tunnels to create
tunnels:
  - name: web
    target: localhost:8080
  - name: api
    target: localhost:3000
    basic_auth: admin:secret123  # Protect with HTTP Basic Auth
  - name: admin
    target: localhost:9000
    basic_auth: superuser:password

# Custom headers injected into all proxied requests
headers:
  X-Forwarded-By: xrok
  X-Custom-App: MyApplication
  X-Environment: production

# Enable WebSocket compression (reduces bandwidth)
compress: true

# Enable debug logging
debug: true

# Auto-reconnect settings
reconnect:
  enabled: true
  max_retries: 0       # 0 = unlimited
  retry_interval: 1s
  max_interval: 5m

# Optional authentication
token: your-api-token

# SOCKS5 proxy (optional)
socks5: :1080

# UDP tunneling (optional)
udp:
  - local: :5353
    remote: 8.8.8.8:53

# Reverse port forwarding (optional)
reverse:
  - remote: 9000
    local: localhost:3000
```

**Configuration Options:**

| Field | Description | Required |
|-------|-------------|----------|
| `server` | Server address | Yes |
| `tls` | Use TLS connections | No (default: true) |
| `transport` | Transport protocol (tcp/quic) | No (default: tcp) |
| `p2p.enabled` | Enable P2P connections | No (default: false) |
| `p2p.stun_server` | STUN server address | No (default: stun.l.google.com:19302) |
| `p2p.fallback` | Fallback to relay if P2P fails | No (default: true) |
| `p2p.turn.enabled` | Enable authenticated TURN fallback | No (default: false) |
| `p2p.turn.server` | TURN control endpoint `host:port` | When TURN enabled |
| `p2p.turn.username/password` | TURN long-term credentials | When TURN enabled |
| `p2p.turn.transport` | Control transport: udp/tcp/tls | No (default: udp) |
| `p2p.turn.ca_cert/server_name` | TURN TLS trust and identity | No |
| `p2p.turn.channel_bind` | Prefer ChannelData, retaining indication fallback | No |
| `tunnels` | List of tunnels to create | Yes |
| `tunnels[].name` | Subdomain name for tunnel | Yes |
| `tunnels[].target` | Local target address | Yes |
| `tunnels[].basic_auth` | HTTP Basic Auth (`user:pass`) | No |
| `headers` | Custom headers to inject | No |
| `compress` | Enable WebSocket compression | No (default: false) |
| `debug` | Enable debug logging | No (default: false) |
| `token` | API authentication token | No |
| `reconnect.enabled` | Enable auto-reconnect | No (default: true) |
| `reconnect.max_retries` | Max reconnect attempts | No (default: 0) |
| `reconnect.retry_interval` | Initial retry interval | No (default: 1s) |
| `reconnect.max_interval` | Maximum retry interval | No (default: 5m) |
| `socks5` | SOCKS5 proxy address | No |
| `udp` | UDP tunnel configurations | No |
| `reverse` | Reverse port forwarding configs | No |

### HTTP Basic Auth per Tunnel

Protect individual tunnels with HTTP Basic Authentication:

```yaml
tunnels:
  - name: public
    target: localhost:8080
    # No auth - publicly accessible
  - name: admin
    target: localhost:9000
    basic_auth: admin:supersecret
    # Requires authentication
```

When accessing a protected tunnel, clients must provide credentials:

```bash
# Without auth - returns 401 Unauthorized
curl https://admin.yourdomain.com/

# With auth - returns 200 OK
curl -u admin:supersecret https://admin.yourdomain.com/
```

### Custom Headers

Inject custom HTTP headers into all proxied requests:

```yaml
headers:
  X-Forwarded-By: xrok
  X-Real-IP: client
  X-Custom-Header: value
```

These headers are added to every request forwarded to your local application, useful for:
- Identifying traffic coming through the tunnel
- Passing additional context to your application
- Integration with reverse proxies or load balancers

### WebSocket Compression

Enable permessage-deflate compression to reduce bandwidth:

```yaml
compress: true
```

Compression is negotiated between client and server using the WebSocket `permessage-deflate` extension. Useful for:
- Slow network connections
- High-volume traffic
- Text-heavy payloads (JSON, HTML)

### HTTP CONNECT Proxy

Connect through corporate HTTP proxies:

```bash
# Via command line
./xrok client --server yourdomain.com --tunnel app:localhost:8080 \
  -http-proxy http://proxy.corp.com:8080

# With proxy authentication
./xrok client --server yourdomain.com --tunnel app:localhost:8080 \
  -http-proxy http://user:password@proxy.corp.com:8080
```

Or in config file:

```yaml
server: yourdomain.com
http_proxy: http://user:password@proxy.corp.com:8080
tunnels:
  - name: app
    target: localhost:8080
```

### mTLS (Mutual TLS)

Enable mutual TLS authentication with client certificates:

**Client side:**
```bash
./xrok client --server yourdomain.com --tunnel app:localhost:8080 \
  -client-cert /path/to/client.crt \
  -client-key /path/to/client.key
```

**Server side:**
```bash
./xrok server -domain yourdomain.com -client-ca /path/to/ca.crt
```

The server will verify client certificates against the provided CA. Useful for:
- Zero-trust environments
- Additional authentication layer
- Compliance requirements

### Stdio Tunneling (SSH ProxyCommand)

Use xrok as an SSH ProxyCommand for transparent SSH access to internal hosts:

```bash
# Direct usage
ssh -o ProxyCommand='./xrok client --server yourdomain.com \
  --tunnel ssh:internal-host:22 --stdio --stdio-target internal-host:22' \
  user@internal-host

# In ~/.ssh/config
Host internal-*
  ProxyCommand ./xrok client --server yourdomain.com \
    --tunnel ssh:%h:22 --stdio --stdio-target %h:22
  User admin
```

This allows SSH access to internal hosts through the xrok tunnel without port forwarding.

## Transport Types

xrok supports two types of tunneling depending on your protocol needs:

### HTTP/WebSocket Tunnels (Web Traffic)

Use `tunnels` for HTTP-based protocols. Traffic flows through WebSocket multiplexing.

**Best for:**
- Web applications (HTTP/HTTPS)
- REST APIs
- WebSocket applications
- Webhooks
- File sharing

```yaml
tunnels:
  - name: webapp
    target: localhost:3000
```

**Access:** `https://webapp.yourdomain.com`

### Reverse Port Forwarding (Raw TCP)

Use `reverse` for raw TCP protocols. Server opens a port that forwards directly to your local service.

**Best for:**
- SSH
- Databases (PostgreSQL, MySQL, Redis)
- Custom TCP protocols
- Game servers
- Any non-HTTP service

```yaml
reverse:
  - remote: "2222"
    local: "localhost:22"
```

**Access:** Direct TCP connection to `yourdomain.com:2222`

### Comparison

| Feature | HTTP Tunnels (`tunnels`) | Raw TCP (`reverse`) |
|---------|-------------------------|---------------------|
| Protocol | HTTP/WebSocket over TLS | Raw TCP |
| Access | Subdomain URL | Server IP + Port |
| TLS | End-to-end HTTPS | Plain TCP (use SSH/TLS in app) |
| Auth | HTTP Basic Auth supported | App-level auth only |
| Best for | Web apps, APIs | SSH, databases, custom TCP |

### Example: Expose SSH via Reverse Forwarding

```yaml
# ssh-tunnel.yaml
server: xrok.example.com
connection_type: tls

reverse:
  - remote: "2222"
    local: "localhost:22"
```

Connect via:
```bash
ssh user@xrok.example.com -p 2222
```

### Example: Expose Database via Reverse Forwarding

```yaml
# db-tunnel.yaml
server: xrok.example.com
connection_type: tls

reverse:
  - remote: "5432"
    local: "localhost:5432"
```

Connect via:
```bash
psql -h xrok.example.com -p 5432 -U myuser mydb
```

### Example: Combined Web + SSH Access

```yaml
# full-access.yaml
server: xrok.example.com
connection_type: tls

# Web application via HTTPS subdomain
tunnels:
  - name: app
    target: localhost:3000

# SSH via direct TCP port
reverse:
  - remote: "2222"
    local: "localhost:22"
```

Access:
- Web: `https://app.xrok.example.com`
- SSH: `ssh user@xrok.example.com -p 2222`

## Architecture

### Standard Mode (Relay)

```
┌─────────────┐         ┌─────────────────────┐         ┌──────────────┐
│   Browser   │ HTTPS   │     xrok server     │  yamux  │  xrok client │
│  or Client  │────────▶│  (yourdomain.com)   │◀───────▶│   (local)    │
└─────────────┘         └─────────────────────┘         └──────┬───────┘
                               │                               │
                               │ SNI-based routing             │ local
                               │ /metrics, /health             │ connections
                               │                               ▼
                               │                        ┌──────────────┐
                               │                        │ Local Apps   │
                               │                        │ :8080, etc.  │
                               │                        └──────────────┘
```

### P2P Mode (Direct Connection)

```
┌──────────────┐                                      ┌──────────────┐
│  xrok client │◀────────── P2P UDP ────────────────▶│  xrok client │
│   (peer A)   │         (hole punched)               │   (peer B)   │
└──────┬───────┘                                      └──────┬───────┘
       │                                                     │
       │ signaling                               signaling   │
       ▼                                                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│                         xrok server                                  │
│  - WebSocket signaling (/p2p/ws)                                    │
│  - STUN coordination                                                 │
│  - Fallback relay if P2P fails                                      │
└─────────────────────────────────────────────────────────────────────┘
```

### Connection Flow

1. Client registers with server via HTTPS on port 443 (`/register_client`)
2. Server assigns subdomain(s) for the client's tunnels
3. Client establishes yamux multiplexed connection (`/mux`)
4. Incoming requests to `subdomain.domain.com` are routed through mux to client
5. Client forwards to local target application

### Special Stream Markers

| Marker | Purpose |
|--------|---------|
| `0xFF` | SOCKS5/Dynamic TCP connection |
| `0xFE` | UDP packet forwarding |
| Other | Regular tunnel traffic with name prefix |

## Monitoring

### Prometheus Metrics

Available at `https://yourdomain.com/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `xrok_active_clients` | Gauge | Number of currently connected clients |
| `xrok_active_connections` | Gauge | Number of active proxy connections |
| `xrok_total_connections` | Counter | Total number of proxy connections handled |
| `xrok_bytes_received_total` | Counter | Total bytes received from clients |
| `xrok_bytes_sent_total` | Counter | Total bytes sent to clients |
| `xrok_connection_duration_seconds` | Histogram | Duration of proxy connections |
| `xrok_connection_errors_total` | Counter | Total connection errors by type |
| `xrok_registration_attempts_total` | Counter | Total client registration attempts |
| `xrok_registration_success_total` | Counter | Successful client registrations |
| `xrok_websocket_upgrades_total` | Counter | Total WebSocket upgrade attempts |
| `xrok_ws_ping_latency_seconds` | Histogram | WebSocket ping-pong latency |

### Health & Status Endpoints

Available on main HTTPS endpoint:

| Endpoint | Description |
|----------|-------------|
| `/health` | Returns `{"status":"healthy"}` |
| `/ready` | Returns `{"status":"ready","clients":N}` |
| `/stats` | Text format server stats |
| `/metrics` | Prometheus metrics |

### Health Check

```bash
curl https://yourdomain.com/health
```

## Security

### API Token Authentication

Set token on server:
```bash
export XROK_API_TOKEN="your-secret-token"
./xrok server -domain yourdomain.com
```

Use token on client:
```bash
./xrok client --server yourdomain.com --token "your-secret-token" --tunnel app:localhost:8080
```

### Rate Limiting

Configure rate limit on server:
```bash
export XROK_RATE_LIMIT=100  # requests per second per client
```

## Troubleshooting

### Connection Issues

- Verify server is running: `curl https://yourdomain.com/health`
- Check firewall allows port 443
- Verify DNS resolves correctly: `dig yourdomain.com`

### Certificate Issues

- Ensure certificates are readable: `sudo ls -la /etc/letsencrypt/live/yourdomain.com/`
- Verify certificate validity: `openssl x509 -in fullchain.pem -text -noout`

### Client Reconnection

- Client auto-reconnects with exponential backoff
- Check logs for reconnection attempts
- Use `--debug` for detailed connection info

### Subdomain Conflicts (409 Error)

- Subdomain already registered by another client
- Wait for previous client to disconnect or use different name
- Restart server to clear state

## Example Configurations

Ready-to-use configuration examples are available in the `examples/` directory:

| File | Description |
|------|-------------|
| `ssh-tunnel.yaml` | Expose local SSH through xrok |
| `ssh-multiple-hosts.yaml` | Multiple SSH hosts on different ports |
| `database-tunnel.yaml` | Expose PostgreSQL, MySQL, Redis |
| `web-app.yaml` | Simple web application tunnel |
| `web-with-auth.yaml` | Web tunnel with HTTP Basic Auth |
| `full-featured.yaml` | Combined web + SSH + SOCKS5 |
| `quic-tunnel.yaml` | QUIC transport for better performance |

## Development

### Project Structure

```
xrok/
├── main.go              # Entry point, subcommand dispatch
├── cmd/
│   ├── client.go        # Client implementation
│   ├── server.go        # Server implementation
│   ├── socks5.go        # SOCKS5 proxy implementation
│   ├── status.go        # Connection status display
│   ├── config/
│   │   └── config.go    # YAML configuration parser
│   ├── logger/
│   │   └── logger.go    # Colored logging utilities
│   ├── p2p/
│   │   ├── p2p.go       # P2P manager, STUN discovery, hole punching
│   │   ├── signaling.go # P2P signaling protocol
│   │   └── tunnel.go    # P2P tunnel transport layer
│   └── transport/
│       └── quic.go      # QUIC transport implementation
├── examples/            # Ready-to-use YAML configurations
│   ├── ssh-tunnel.yaml
│   ├── database-tunnel.yaml
│   └── ...
└── README.md
```

### Building

```bash
go build -o xrok .
go vet ./...
```

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Submit a pull request

## License

MIT License - see LICENSE file for details.
