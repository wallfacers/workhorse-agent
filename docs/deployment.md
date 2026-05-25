# workhorse-agent deployment guide

This guide covers the supported deployment shapes:

1. Local single-user (the default).
2. Reverse proxy via nginx (for adding TLS, a hostname, or sharing the box
   with other services).
3. systemd unit for unattended operation.

It also documents the operational knobs that are not in scope for the
protocol reference: Bearer auth, origin allowlists, and SQLite backup.

## Local single-user deploy

This is the supported default. workhorse-agent binds `127.0.0.1` and serves
a single operating-system user.

```sh
# 1. Build the binary.
go build -o workhorse-agent ./cmd/workhorse-agent

# 2. Scaffold the per-user state.
./workhorse-agent init

# 3. Edit config — at minimum, set one provider API key.
$EDITOR ~/.workhorse-agent/config.yaml

# 4. Run.
./workhorse-agent serve
```

File layout under `~/.workhorse-agent/`:

| Path           | Purpose                                                        |
|----------------|----------------------------------------------------------------|
| `config.yaml`  | Static configuration. Requires restart after edits.            |
| `state.db`     | SQLite database (sessions, messages, events, tool_calls, permissions). |
| `mcp.json`     | MCP server registry consumed by the MCP host.                  |
| `skills/`      | One subdirectory per skill, each containing `skill.yaml`. Re-scanned on demand. |
| `agents/`      | One `*.yaml` per sub-agent type. Re-scanned on demand.         |

The default bind is `127.0.0.1:7821`. Do not change `server.host` to
`0.0.0.0` unless you are aware that doing so disables the "missing Origin
header" allowance and exposes the API on the network.

## nginx reverse proxy

A reverse proxy is needed whenever a hostname, TLS termination, or
shared-host topology is involved. SSE imposes three requirements that
nginx does not enable by default; getting any of them wrong silently
breaks the GET stream.

```nginx
server {
    listen 443 ssl http2;
    server_name agent.example.com;

    ssl_certificate     /etc/letsencrypt/live/agent.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/agent.example.com/privkey.pem;

    # Body cap: the agent itself enforces this, but matching it at the
    # proxy avoids buffering a request that is going to be rejected.
    client_max_body_size 2m;

    location / {
        proxy_pass http://127.0.0.1:7821;
        proxy_http_version 1.1;

        # Critical for SSE: do not buffer the response. Without this nginx
        # holds events in its proxy buffer and the GET stream appears to
        # hang.
        proxy_buffering off;

        # Critical: the SSE connection is long-lived. The default 60s
        # read timeout will cut healthy streams mid-flight.
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;

        # Critical: pass the browser Origin through unchanged so the
        # agent's Origin allowlist sees the real value.
        proxy_set_header Host              $host;
        proxy_set_header Origin            $http_origin;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE chunked transfer relies on these.
        proxy_set_header Connection "";
        proxy_cache off;
    }
}
```

The server already sets `X-Accel-Buffering: no` on every SSE response, so
nginx will refuse to buffer that connection even if a global
`proxy_buffering on;` is in effect. The explicit `proxy_buffering off;`
above is belt-and-suspenders.

After enabling the proxy, add the public origin to
`server.allowed_origins` (see below) so the browser-side `Origin` check
passes.

## systemd unit template

For a single-user install, drop the following at
`/etc/systemd/system/workhorse-agent.service`. Replace `OWNER` with the
operating-system user that owns `~/.workhorse-agent/`.

```ini
[Unit]
Description=workhorse-agent local AI agent server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=OWNER
Group=OWNER
ExecStart=/usr/local/bin/workhorse-agent serve
Restart=on-failure
RestartSec=3s

# Hardening. Loosen only if a tool explicitly needs the capability.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/OWNER/.workhorse-agent
PrivateTmp=yes

# SIGTERM triggers the seven-step graceful shutdown.
KillSignal=SIGTERM
TimeoutStopSec=60s

[Install]
WantedBy=multi-user.target
```

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now workhorse-agent
sudo journalctl -u workhorse-agent -f
```

The `TimeoutStopSec` should comfortably exceed
`server.graceful_shutdown_timeout_seconds` (default 30) so systemd does not
escalate to `SIGKILL` while the server is still draining cancelled
sessions.

## Enabling Bearer auth

Auth is off by default. To enable:

1. Generate a long random token (≥ 32 bytes of entropy):

   ```sh
   openssl rand -hex 32
   ```

2. Add the value to `~/.workhorse-agent/config.yaml`:

   ```yaml
   auth:
     enabled: true
     bearer_token: "PASTE_LONG_RANDOM_HEX_HERE"
   ```

   Equivalent environment variables:
   `WORKHORSE_AGENT_AUTH_ENABLED=true` and
   `WORKHORSE_AGENT_AUTH_BEARER_TOKEN=...`. The token value is never
   written to logs, even at debug level.

3. Restart `workhorse-agent serve`.

4. Clients must now send `Authorization: Bearer <token>` on every
   `/v1/*` and `/debug/*` request. `/health` and `/ui` remain exempt so
   monitoring and the embedded UI keep working.

Token comparison uses `crypto/subtle.ConstantTimeCompare`, eliminating
the standard timing side channel.

## Origin allowlist

The server validates the `Origin` header on every
`/v1/sessions/{id}/stream` request using an exact `scheme + hostname + port`
triple match. The defaults are:

- Missing `Origin` (e.g. `curl`, server-to-server clients): allowed only
  when the server is bound to a loopback address.
- `http(s)://127.0.0.1:<any-port>` and `http(s)://localhost:<any-port>`:
  always allowed.
- `null` (sandboxed iframes, `file://` documents): allowed only when
  `server.allow_null_origin: true`.

Add additional origins (for example a development UI or the public
hostname from the nginx example) to the allowlist:

```yaml
server:
  allowed_origins:
    - http://localhost:5173
    - https://agent.example.com
```

Each entry must be a complete origin string. Substring or wildcard
matching is intentionally not supported, so an attacker cannot mount a
homograph attack like `http://127.0.0.1.evil.com`.

## Backup

`state.db` is a regular SQLite file under `~/.workhorse-agent/`. Two
options:

1. **Cold backup.** Stop `workhorse-agent serve`, copy the file:

   ```sh
   sudo systemctl stop workhorse-agent
   cp ~/.workhorse-agent/state.db /backups/state-$(date +%F).db
   sudo systemctl start workhorse-agent
   ```

2. **Online backup.** Use SQLite's online backup API while the server is
   running:

   ```sh
   sqlite3 ~/.workhorse-agent/state.db \
     ".backup '/backups/state-$(date +%F).db'"
   ```

   This is safe to run concurrently with the server because SQLite's
   backup machinery handles WAL frames atomically. Verify the backup with
   `sqlite3 <copy> 'pragma integrity_check;'`.

Either way, the resulting file is a standalone database — restoring it
is `cp` back into place while the server is stopped.
