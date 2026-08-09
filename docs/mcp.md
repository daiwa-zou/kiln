# Serving the wiki to agents

kiln exposes a bench over the Model Context Protocol two ways. They run the same
seven tools against the same HTTP API; what differs is who can reach them.

| | `kiln mcp` | `kiln serve` |
| --- | --- | --- |
| Transport | stdio | Streamable HTTP at `/mcp` |
| Who starts it | the agent, as a subprocess | already running |
| Reaches | an agent on this machine | anything that can reach the URL |
| Credential | `KILN_TOKEN` in the agent's config | `Authorization: Bearer` per request |

The subprocess is right for an agent on the same machine as your config file. A
remote client — a Claude connector, a teammate's agent — cannot spawn a
subprocess on your host, which is what the HTTP endpoint is for.

## The endpoint

`kiln serve` mounts it at `/mcp`. There is nothing to enable.

```
kiln dev listening on 127.0.0.1:8080 (https)
  MCP endpoint at https://kiln.example.com/mcp
```

Tools: `list_benches`, `search_wiki`, `read_page`, `wiki_overview`, `list_pages`,
`page_backlinks`, `wiki_gaps`. Every one is a read.

There is no default bench. The subprocess takes `--workspace` because whoever
starts it knows which bench they mean; a URL is shared across benches, so every
tool takes a `bench` argument and `list_benches` enumerates what the caller's
token can reach.

## HTTPS

Remote MCP clients require `https`. Either terminate TLS in front and set the
public origin:

```toml
public_url = "https://kiln.example.com"
```

or serve it directly:

```toml
tls_cert = "/etc/kiln/tls.crt"
tls_key  = "/etc/kiln/tls.key"
```

`public_url` is not cosmetic. It is what the discovery documents advertise, and
behind a terminating proxy it is the only place the real scheme is known — the
request itself arrives as plain HTTP on an internal host. Validation rejects a
non-`https` `public_url` that is not a loopback address, because a client that
reads `http://` there will refuse the connector.

## Connecting each agent

Setup differs by agent, and not cosmetically. The dividing line is **who dials
the URL**: an agent running on your machine reaches whatever you can reach, but
a connector added in the Claude apps is fetched by Anthropic's servers, so it
only reaches the public internet.

| Agent | How | Reaches a private address? |
| --- | --- | --- |
| Claude Code | HTTP endpoint | yes — dials it itself |
| Claude Desktop | stdio subprocess | yes — runs locally |
| Claude.ai (web, mobile) | custom connector | no — needs public https |
| Other MCP clients | either | depends on the client |

### Claude Code

Runs on your machine and dials the endpoint itself, so any address you can
reach works — including a loopback one over plain http.

```bash
claude mcp add --transport http kiln https://kiln.example.com/mcp \
  --header "Authorization: Bearer kiln_..."
```

Drop `--header` if `auth.mode = "none"`. Check it with `claude mcp list`; the
entry should read `✔ Connected`.

### Claude Desktop

The desktop app's connector list is fetched by Anthropic's servers, so it
cannot reach a private address. Run kiln as a local subprocess instead — no
reachable URL, no TLS. In `claude_desktop_config.json`
(`~/Library/Application Support/Claude/` on macOS, `%APPDATA%\Claude\` on
Windows):

```json
{
  "mcpServers": {
    "kiln": {
      "command": "kiln",
      "args": ["mcp", "--url", "http://127.0.0.1:8080", "--workspace", "my-bench"],
      "env": {"KILN_TOKEN": "kiln_..."}
    }
  }
}
```

Restart the app afterwards. `kiln` must be on your `PATH`, or give an absolute
path.

### Claude.ai (web and mobile)

*Settings → Connectors → Add custom connector*, with the endpoint URL. It has to
be reachable from the public internet over https — see [HTTPS](#https) above.

- `auth.mode = "none"` — connects with no credential once it is reachable. An
  authless MCP server is a supported connector type. Only defensible for an
  instance nobody else can reach.
- `auth.mode = "token"` — the key goes in the dialog's **Request headers**
  section as `Authorization` with the value `Bearer kiln_...`, including the
  word `Bearer`. That field is in beta; without it the alternative is OAuth,
  which kiln does not implement (see below).

### Other MCP clients

Anything that speaks Streamable HTTP takes the endpoint plus a bearer header.
Clients that want a command rather than a URL — or that run somewhere the
address does not resolve — take the `kiln mcp` subprocess form above.

Mint a key with `kiln admin token create`. It carries the read scope and sees
exactly the benches its owner does.

## What is not built yet

Claude's other connector auth types are OAuth flows: dynamic client
registration, a Client ID Metadata Document, or client credentials Anthropic
holds for you. All three require kiln to be an OAuth authorization server —
`/authorize`, `/token`, `/register`, PKCE, refresh-token rotation, a consent
screen. It is not one.

What kiln does today is the resource-server half, which is what a client needs
to *discover* that flow when it exists:

- An unauthenticated request gets `401` with
  `WWW-Authenticate: Bearer resource_metadata="…"`. The status matters: Claude
  does not honour that header on a `200`, and a tool-level error instead of a
  `401` leaves it with nothing to follow.
- `/.well-known/oauth-protected-resource/mcp` serves the RFC 9728 document, and
  the bare `/.well-known/oauth-protected-resource` answers the fallback probe.
  `resource` matches the endpoint URL exactly, as the spec requires.

The document omits `authorization_servers` until an operator sets
`mcp_authorization_server`. Naming an issuer that cannot mint tokens for this
resource would send every client through a handshake that ends in a refusal;
absent, the document still says what the resource is and that it takes a bearer
token in the header.

## Why the endpoint reads its own API

The tools call kiln's HTTP API rather than the store directly. That is what
makes an agent see exactly what its token sees: every visibility rule lives in
the handlers, so reading through them cannot skip one.

Mounted on that same server, those calls do not leave the process — they are
dispatched straight into the router. No socket, no port to discover, no second
pass through TLS, and no internal hop anyone else could reach. The caller's
token rides along on each one, so two agents pointed at the same URL see
different wikis if their tokens differ.
