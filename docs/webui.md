# WebUI

## Read this first

The WebUI is a frontend over the same runtime the CLI uses. That runtime can
**run shell commands, read and write files, and reach the network**. Anyone who
can reach the WebUI can do those things through it.

So the default is local-only, and going beyond that is a deliberate act with
guard rails you have to step over on purpose:

- The default bind is `127.0.0.1:8585`.
- Binding beyond loopback **without authentication is refused at startup**, not
  warned about. You override that refusal with an environment variable set at
  launch, not with a config key that can lie dormant in a copied file.
- Wildcard origins (`*`) are rejected outright.
- `X-Forwarded-*` headers are ignored unless you explicitly declare that a
  trusted proxy sits in front.
- Boop is not, and is not becoming, an internet-facing service (§63). Public
  exposure means putting a real reverse proxy in front of it (§53).

## Status

The `web` package is being completed at the time of writing. The server, the
authenticator, the origin policy, the static asset handler, the TypeScript
frontend and the `boop --web` dispatch path (`cmd/boop/web.go`) all exist, and
the security behaviour described below is implemented in `web/server.go` and
`web/auth.go`.

Parts are still landing — the WebSocket hub and some API handlers were not yet
compiling when this was written. Treat this document as a description of the
intended and largely implemented behaviour, and check the code before depending
on a detail.

## Configuration

```yaml
web:
  enabled: false
  listen: 127.0.0.1
  port: 8585
  auth:
    enabled: false
    token_env: BOOP_WEB_TOKEN
  allowed_origins: []
  trusted_proxy_headers: false
```

| Key | Default | Meaning |
|---|---|---|
| `web.enabled` | `false` | Start the WebUI with the process |
| `web.listen` | `127.0.0.1` | Bind address. Must parse as an IP address |
| `web.port` | `8585` | 1–65535 |
| `web.auth.enabled` | `false` | Require an access token |
| `web.auth.token_env` | *(unset)* | **Name** of the environment variable holding the token. The token itself is never stored in the config |
| `web.allowed_origins` | `[]` | Explicit origin allowlist. Same-origin is always allowed; `*` is rejected |
| `web.trusted_proxy_headers` | `false` | Honour `X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Forwarded-For` |

Command-line overrides:

```bash
boop --web                                  # loopback, configured port
boop --web --port 9000                      # loopback, port 9000
boop --web --listen 0.0.0.0 --port 8585     # every interface — needs auth
boop --web --listen 0.0.0.0 --allow-insecure-bind   # skip the safety refusal
```

`--web` starts the server over the same runtime the CLI uses, with a
`permissions.Broker` as the approver so confirmations reach the browser instead
of blocking on a terminal prompt nobody is watching. Ctrl-C shuts it down.

## The loopback default

`127.0.0.1` means only this machine. Not the LAN, not a container's bridge
network, not a VPN peer.

An **empty** listen address is treated as non-loopback, because `net.Listen`
reads it as "every interface" and the failure mode of guessing wrong is an
exposed server. `localhost` and any loopback IP literal count as loopback.

## Exposing it safely

The order of preference is:

1. **Do not.** Use an SSH tunnel:

   ```bash
   ssh -N -L 8585:127.0.0.1:8585 you@workstation
   ```

   Boop stays on loopback; your browser talks to loopback on your own machine.
   Nothing needs configuring and there is no new attack surface.

2. **A private interface with token authentication**, if you genuinely need
   another device on your LAN to reach it.

3. **Behind a reverse proxy** that terminates TLS and authenticates, if you
   need it outside the LAN. See below.

### Binding to the LAN

Boop refuses to start if `web.listen` is not loopback and `web.auth.enabled` is
false:

```
web: refusing to bind beyond loopback without authentication: web.listen is
"0.0.0.0", which other machines can reach, but web.auth.enabled is false. Set
web.auth.enabled with web.auth.token_env, bind 127.0.0.1 instead, or set
BOOP_WEB_ALLOW_INSECURE=1 to accept the risk deliberately
```

This is fatal rather than a warning, because a rule that still lets the unsafe
configuration start is not a rule.

The correct fix is to turn on authentication:

```yaml
web:
  enabled: true
  listen: 192.168.1.10       # a specific interface, not 0.0.0.0
  port: 8585
  auth:
    enabled: true
    token_env: BOOP_WEB_TOKEN
  allowed_origins:
    - http://192.168.1.10:8585
```

```bash
export BOOP_WEB_TOKEN="$(openssl rand -hex 32)"
boop --web
```

Two things skip the check, and neither is a config key:
`BOOP_WEB_ALLOW_INSECURE=1` in the environment, or `--allow-insecure-bind` on
the command line. That is deliberate — turning off the last safety check should
be a decision made at the moment of launch, not something that travels with a
config file copied between machines. When either is used, the server logs a
loud banner saying so.

Even a legal bind produces warnings. `boop --verbose` and the server log will
tell you when the WebUI is reachable from other machines, when there is no
authentication, and when origins are not being validated.

## Token authentication

The token is named, not stored. `web.auth.token_env` gives the name of an
environment variable; the value is read once at startup and kept only as a
SHA-256 digest, so a heap dump or a stray `%v` of the server cannot surrender
it. Comparison is over digests, which is constant time in both the value and
the length — comparing raw strings would leak the token length through timing.

Generate something with real entropy:

```bash
export BOOP_WEB_TOKEN="$(openssl rand -hex 32)"
```

If `auth.enabled` is true and the named variable is empty or unset, the server
**refuses to start**. Starting with authentication that cannot possibly succeed
is worse than not starting, because the operator believes they are protected.

### Sending the token

HTTP requests use the standard header:

```bash
curl -H "Authorization: Bearer $BOOP_WEB_TOKEN" http://127.0.0.1:8585/api/status
```

A browser cannot set an `Authorization` header on a WebSocket handshake, so the
event stream accepts the token two other ways, in this order of preference:

1. **A WebSocket subprotocol** — `boop.token.<token>`, alongside the plain
   `boop.v1` subprotocol. Preferred, because subprotocol headers are not
   written to a proxy access log by default.
2. **A query parameter** — `?access_token=<token>`. Supported for clients and
   proxies that cannot manipulate subprotocols, but query strings are logged by
   reverse proxies, kept in browser history and leaked in `Referer` headers.
   Use it only when you must.

Failures are deliberately coarse: the server says "a valid access token is
required" whether the token was absent or wrong. Which one it was is
information a caller can use.

## Origin validation

Every API request goes through an origin check before authentication.

- **Same origin is always allowed.** "Same origin" means the origin the request
  was addressed to — scheme and host from the request, or from
  `X-Forwarded-Proto`/`X-Forwarded-Host` when `trusted_proxy_headers` is on.
- **Listed origins are allowed.** Each entry must be a full
  `scheme://host[:port]` with no path. Default ports are normalised away, so
  `http://LocalHost:80` and `http://localhost` compare equal.
- **`*` is rejected at startup**, not silently downgraded. §23 calls unsafe
  wildcard CORS out by name, and quietly reinterpreting it would leave the
  operator believing something untrue.
- **`Origin: null` is always refused.** That is what a browser sends from a
  sandboxed iframe, a `data:` URL or a `file://` page. It can never be
  same-origin and can never be listed.
- **A missing `Origin` header is allowed** for plain HTTP requests — that is
  `curl`, a health check, a script. The WebSocket upgrade path runs its own
  stricter check, because a browser always labels an upgrade.

CORS response headers are only emitted when an `Origin` was present and
allowed: `Access-Control-Allow-Origin` echoes the normalised origin, methods
are `GET, POST, PUT, OPTIONS`, headers are `Authorization, Content-Type`, and
`Vary: Origin` is always set.

You need `allowed_origins` when a page served from somewhere else talks to
Boop — a Vite dev server on `http://localhost:5173`, for example, or the proxy
hostname users actually type.

## Reverse proxy setup (§53)

```
public client
   │  HTTPS
   ▼
reverse proxy / auth gateway / VPN
   │  trusted private connection
   ▼
boop  127.0.0.1:8585
```

The proxy owns TLS, public authentication, rate limiting and internet-facing
hardening. Boop remains a local service that happens to have something in front
of it. Keep Boop bound to loopback and let the proxy be the only thing that can
reach it — that way the guard rails and the proxy are not two competing
opinions about who is allowed in.

Requirements:

- **WebSocket upgrade must be proxied.** The event stream is a WebSocket; if
  `Upgrade` and `Connection` do not survive the hop, the UI will load and then
  sit there.
- **Set `web.trusted_proxy_headers: true`** so Boop computes its own origin
  from `X-Forwarded-Proto` and `X-Forwarded-Host` instead of the internal
  address. Without it, a page served as `https://boop.example.com` will fail
  the origin check against `http://127.0.0.1:8585`.
- **Only set it when a proxy really is in front.** These are client-supplied
  headers. Trusting them by default would let anyone assert their own origin is
  yours, and write anything they like into your access log.
- **Add the public origin** to `web.allowed_origins`.
- **Do not idle out the WebSocket.** Set a generous read timeout; a long agent
  run has quiet stretches.

### Nginx

```nginx
server {
    listen 443 ssl http2;
    server_name boop.example.com;

    ssl_certificate     /etc/ssl/certs/boop.crt;
    ssl_certificate_key /etc/ssl/private/boop.key;

    # Public authentication belongs here, not in Boop.
    auth_basic           "boop";
    auth_basic_user_file /etc/nginx/boop.htpasswd;

    location / {
        proxy_pass http://127.0.0.1:8585;

        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;

        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering    off;
    }
}
```

```yaml
web:
  enabled: true
  listen: 127.0.0.1
  port: 8585
  trusted_proxy_headers: true
  allowed_origins:
    - https://boop.example.com
```

### Caddy

```caddyfile
boop.example.com {
    basicauth {
        you $2a$14$…
    }
    reverse_proxy 127.0.0.1:8585
}
```

Caddy handles TLS, WebSocket upgrades and `X-Forwarded-*` without extra
configuration. You still need `trusted_proxy_headers: true` and the public
origin in `allowed_origins` on the Boop side.

### Traefik

Route to the Boop service, attach a TLS resolver and an auth middleware.
Traefik forwards `X-Forwarded-*` and upgrades WebSockets by default; the same
two Boop-side settings apply.

## The API surface

Served under `/api/`, each endpoint wrapped in panic recovery, origin
enforcement, CORS and token authentication, in that order. Method dispatch
happens inside each handler so that a wrong method returns the same JSON error
envelope as everything else rather than a plain-text 405.

| Endpoint | Purpose |
|---|---|
| `/api/status` | Version, uptime, session state, provider health, current model, agent counts. No secrets |
| `/api/config` | `GET` the effective configuration; `PUT` to update, persist **and apply it live** — most settings take effect on the next turn, and `restart_required` / `restart_fields` name the few (web bind, logger, outbound web access, provider definitions) that still need a restart |
| `/api/models` | Models available from configured providers |
| `/api/providers` | Configured providers and their health |
| `/api/agents` | Agent fleet state |
| `/api/sessions` | Session list |
| `/api/session` | Create or select a session |
| `/api/stats` | Token and cost totals |
| `/api/tools` | Registered tools |
| `/api/message` | Submit a user turn |
| `/api/approval` | Resolve one pending approval |

Anything else under `/api/` returns a 404 in the API's own JSON vocabulary
rather than falling through to the single-page-app fallback. Everything outside
`/api/` is served from the embedded static bundle.

Runtime events arrive over a WebSocket carrying the same
`internal/app` event stream the TUI subscribes to, plus approval-queue changes
from `permissions.Broker`. The endpoint path constant is still being finalised;
the frontend probes `/api/events`, `/api/ws` and `/ws` in that order.

## Approvals in the browser

Approvals are the reason the event stream is a WebSocket rather than
server-sent events: they are bidirectional.

`permissions.Broker` is the shared queue. The core blocks in `Request`; every
attached frontend sees the same `Pending` list and any of them can `Resolve`.
Resolution is keyed by ID and broadcast, so approving in the terminal makes the
request disappear from the browser.

Two rules the WebUI must not weaken (§50):

- A browser approval must show **everything** the terminal shows: the summary,
  the detail, the risk level, and the production warning. An approval dialog
  that shows less is a security regression.
- "Always for session" is refused for production-affecting and critical-risk
  actions. The broker downgrades such a request to a single approval and
  reports the scope it actually applied, so the UI can tell the user.

## The static bundle

The frontend is TypeScript built with esbuild into `web/static/dist` and
embedded with `go:embed`. Build it with:

```bash
make web-build
```

A clean checkout has no bundle. The embed pattern names the parent directory
and a committed placeholder so the server builds whether or not anyone has run
npm, and an empty or failed build degrades to the placeholder rather than
serving 404s that look like a broken server.

## Troubleshooting

**Server refuses to start with `refusing to bind beyond loopback`.** Working as
designed. Turn on `web.auth.enabled` with a `token_env`, or bind `127.0.0.1`,
or set `BOOP_WEB_ALLOW_INSECURE=1` if you have genuinely decided to accept it.

**Server refuses to start with `the environment variable … is empty or
unset`.** `auth.enabled` is true but the token variable is not exported in the
shell that launched Boop.

**403 `origin … is not allowed`.** Add that exact origin to
`web.allowed_origins`, scheme and port included. Behind a proxy, also set
`trusted_proxy_headers: true`.

**401 with a token you are sure is right.** Check the header form: `Authorization:
Bearer <token>`, not `Token` and not bare. For the WebSocket, use the
`boop.token.<token>` subprotocol.

**The page loads but nothing updates.** The WebSocket did not connect. Almost
always a proxy that is not forwarding `Upgrade`/`Connection`, or a proxy read
timeout closing an idle stream.

**`0.0.0.0` in a URL.** That is a bind address, not an address to visit.
`Server.URL()` rewrites it to `127.0.0.1` for the banner; visit the machine's
actual address.
