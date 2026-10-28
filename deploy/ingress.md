# Shared HTTPS ingress for xrok

Use TLS SNI passthrough when xrok shares public port 443 with another HTTPS service.
The ingress routes encrypted connections by hostname; xrok keeps its own wildcard
certificate and verifies client certificates for control endpoints. WebSocket and mux
traffic pass through as TCP without HTTP termination.

The supplied [nginx stream configuration](nginx-stream.conf.example) is a top-level
`stream` block. Replace `xrok.example.com` and its wildcard with the deployed domain.
It sends xrok names to port 8443 and other names to the existing HTTPS service on
loopback port 4443. nginx needs stream and SSL preread support.

## Configuration

Keep the xrok listener on its existing port and advertise the public ingress port:

```yaml
domain: xrok.example.com
proxy_port: 8443
public_port: 443
legacy_listeners: false
tls_cert_path: /etc/xrok/tls/fullchain.pem
tls_key_path: /etc/xrok/tls/privkey.pem
# Retain the existing API token and client_ca_path configuration.
```

`public_port` changes visitor URLs and the unified data port returned during
registration. It does not bind a new port. Zero or an omitted value retains the listen
port in advertised URLs. The CLI equivalent is `--public-port 443`.

Clients use `server_port: 443` for the public ingress. Keep direct 8443 reachable during
validation and rollback. SNI passthrough requires a hostname; a client connecting by IP
without SNI follows nginx's default backend. DNS must cover both the base xrok name and
the tunnel wildcard, and the xrok certificate must cover those names.

## Applying the ingress

1. Save the current nginx configuration, port mappings, and xrok configuration.
2. Move the existing HTTPS listener from public 443 to the loopback backend port 4443.
   Preserve its certificate, host routing, and authentication behavior. For a container,
   adjust the host mapping and backend address to match the actual container network.
3. Add the stream block at nginx's top level, alongside the HTTP block. Validate the
   combined configuration with `nginx -t` before reloading. Do not let both listeners
   compete for the same public address and port.
4. Set xrok `public_port: 443` and keep `proxy_port: 8443`. Reload or restart xrok using
   the normal deployment procedure, then use the public hostname and port in clients.
5. Verify registration, mux traffic, GET, POST integrity, a long-lived WebSocket, client
   mTLS acceptance/rejection, certificate renewal, and unrelated HTTPS hostnames. Repeat
   the same controlled proxy candidates on 443 and 8443 to measure compatibility.

For rollback, restore the saved HTTPS listener/port mapping, remove the stream listener,
and unset `public_port` so xrok advertises 8443 again. Restore client `server_port: 8443`.
Changing the shared vpn-infra ingress is a separate deployment operation; this example
does not modify that service automatically.

## Routing details

The example forwards TCP only. QUIC uses UDP and needs its own explicitly chosen
network path; a TCP SNI listener does not make UDP 443 available. Use direct QUIC on
the reachable xrok UDP port when required. A configured HTTP/HTTPS/SOCKS TCP proxy
cannot carry QUIC, so xrok rejects that combination before registration.

Without PROXY protocol support, xrok sees nginx's backend connection as the peer.
Account for this when placing IP rate limits and inspecting forwarded visitor addresses.
Do not enable nginx's PROXY protocol for the current xrok TLS listener, which expects
TLS bytes immediately. Passthrough leaves xrok's mTLS checks intact; an HTTP termination
design would need a separate trust model for client certificate identity.

The stream example has been syntax checked with nginx and exercised on a local SNI
ingress with private CA verification, xrok control mTLS, HTTPS POST integrity, and
WebSocket echo. Apply the deployment checks above to the actual shared host and DNS.
