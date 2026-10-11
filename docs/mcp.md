# Serving the wiki to agents

kiln exposes a bench over the Model Context Protocol two ways. They run the same
tools against the same HTTP API: seven that read, plus three that file into the
review queue for a key allowed to (see [Filing](#filing-questions-and-corrections)).
What differs is who can reach them.

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

**Every bench the key can read is served, and no others.** `list_benches`
enumerates them, and every other tool takes a `bench` argument. A token sees
exactly what its owner sees: naming a bench the token cannot read answers
identically to naming one that does not exist.

Neither form pins a bench by default. `kiln mcp --workspace <id>` narrows the
subprocess to one, which is worth doing only when the agent should not see the
rest — it makes the other benches unreachable rather than merely un-defaulted.

**Name a bench by its id.** `list_benches` prints each bench's id first, with
its `org/slug` beside it, and the id is what to pass back as `bench` (or to
`--workspace`). Slugs are unique only within an org, so a user in two orgs that
each own a `docs` bench could otherwise be answered from the wrong one. `org/slug`
is unambiguous too. A bare slug is still accepted, but only while exactly one
bench the key can read has it; otherwise the tool reports the candidates and
asks for the id.

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
      "args": ["mcp", "--url", "http://127.0.0.1:8080"],
      "env": {"KILN_TOKEN": "kiln_..."}
    }
  }
}
```

Restart the app afterwards. `kiln` must be on your `PATH`, or give an absolute
path. Add `"--workspace", "<bench id>"` to the args only if you want the agent
confined to one bench; without it, it can reach every bench the key allows and
`list_benches` shows them.

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

Mint a key under **Agent access → Keys**, or with `kiln admin token create`.
It carries the read scope and sees exactly the benches its owner does.

## Filing questions and corrections

Ten tools in all. The seven reads are always there. Three more file into a
bench's review queue, and nothing any of them sends changes a page:

| Tool | Files |
| --- | --- |
| `request_gap{bench, slug}` | a request for a page `wiki_gaps` lists (the Gaps view's "Ask for it") |
| `file_question{bench, page, kind, title, detail}` | a question about a page: `uncertain` or `contradiction` |
| `propose_correction{bench, page, text, rationale}` | a proposed correction, which only a person can pin |

**Scopes.** A key minted with *Can file questions and corrections* carries
`read` and `propose:<bench id>`, and files into that one bench. `propose` is
accepted on exactly two routes, `POST …/workspaces/{ws}/reviews` and
`POST …/workspaces/{ws}/gaps/{slug}/request`, and refused on every other
mutation. It cannot pin a correction, resolve a review, delete a page or mint
a key. A `read` key sees only the seven reads, and a filing tool called by name
is refused. A `write` token sees the filing tools too, but nothing new opens up
for it: a write token can already pin corrections directly over REST, so treat
write tokens as human credentials and give agents propose keys.

**Promotion is human.** A proposed correction waits under Reviews, showing its
text verbatim and the key that filed it. A member with write access pins it
with `POST …/reviews/{id}/promote {text}` (the inbox's *Pin as a correction*,
after ticking *I have read this text*). The text they confirm, which they may
edit, is what gets pinned, in the same transaction that approves the review.
Prompts label a promoted correction "proposed by an agent, confirmed by a
workspace member", fenced like every other correction.

**Flood control.** Each key has its own filing bucket (ten at once, then one
every six seconds), and every filing also spends from its owner's write bucket.
A bench holds at most 20 open items per key and 200 key-filed items in total,
gap requests included;
past that the filing answers `429` until people work through the queue. A
person holds at most 20 live keys.

**Getting a key that can file.** Mint it under **Agent access → Keys** with the
bench you want it to file into open, ticking *Can file questions and
corrections*. That needs token authentication (the default `auth.mode`) and a
role on the bench that can write; `kiln admin token create` does not mint
propose keys.

**stdio.** `kiln mcp` cannot see its key's scopes, so it offers the filing tools
only with `--propose`:

```json
{
  "mcpServers": {
    "kiln": {
      "command": "kiln",
      "args": ["mcp", "--propose", "--url", "https://your-kiln", "--workspace", "<bench id>"],
      "env": {"KILN_TOKEN": "kiln_..."}
    }
  }
}
```

Its tool descriptions cannot name the bench, so pass the key's bench as
`--workspace` (or as `bench` on each call); kiln refuses any other.

**Authentication disabled.** With `auth.mode = "none"` every caller is trusted,
so the endpoint shows the filing tools to everyone and they file into any
bench. That mode is refused on a non-loopback bind, and the API's loopback
guard is what stands between a web page and these routes.

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
