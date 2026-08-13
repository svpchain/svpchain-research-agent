# svpchain-research-agent

The intermediary [A2A](https://a2aproject.github.io/A2A/) agent for SVP-Chain:
a remote, server-side agent that accepts a task under a **re-delegable** SVP-DT
credential, narrows it, and hands the execution leg to a downstream agent —
which executes on the **original user's** account.

It exists to demonstrate, and keep tested, the one part of the delegation design
no other component exercises: a middle hop that cannot widen what it received.
See [`cmd/svpchain-research-agent/README.md`](cmd/svpchain-research-agent/README.md)
for the credential flow in detail.

| | |
|---|---|
| Port | 8081 |
| Advertised at | `<public-url>/research` |
| Image | `ghcr.io/svpchain/svpchain-research-agent` |
| Downstream | any agent the credential's `redelegate_to` allows — typically the perps agent |

The credential handling is reused from
[`svpchain-agent-core`](https://github.com/svpchain/svpchain-agent-core) rather
than reimplemented: the same verifier, resolver, and settlement recorder the
other agents use. `internal/intermediary` is this repo's own — the
re-delegation role is this agent's product, not shared library surface.

## Running

```sh
go run ./cmd/svpchain-research-agent -config agent.toml -listen :8081
```

Unlike the other agents it takes its listen address and advertised URL as
flags rather than from the config, so the deploy passes every argument.

## The operator key is mandatory

This agent **signs the credentials it forwards** — its DID derives from its
operator key, and a credential it mints verifies nowhere without it. It
therefore cannot run keyless, and the deploy script skips it entirely when no
key is supplied.

The key must be distinct from every other agent's, for the same reason as the
rest: an agent's on-chain id derives from its key, and `agent_self_register`
publishes a hash of this binary's own card.

```sh
./scripts/deploy.sh --host www@host.example.com \
  --operator-key-file ./research.key \
  --public-url https://agents.svpchain.org
```

Inspect without touching anything: `--print-config`, `--print-compose`,
`--print-nginx`, `--dry-run`. Tear down with `--uninstall`.

## Behind the reverse proxy

The agents share one host, each on its own path: this one answers at
`<base>/research` and listens on `127.0.0.1:8081`. Print its location block:

```sh
./scripts/deploy.sh --public-url https://agents.svpchain.org --print-nginx
```

Nothing installs it. The server block it belongs in owns TLS and the base
host, both shared with agents this repo must not know about — so paste it,
then `nginx -t && systemctl reload nginx`.

The route is not cosmetic. `public_url` is advertised inside the Agent Card,
and a verifier fetches that URL to recompute the capability hash; if nginx
does not route `/research` to this port the agent advertises a URL that 404s
and reads as unverified, with every process healthy and nothing in the logs.
`TestDeployScriptNginxRouteMatchesConfig` pins the two together.

This agent has a second reason to care: it re-delegates to a downstream agent
by that agent's public URL, so a broken route breaks the hop, not just the
lookup.

## Development

`GOWORK=off` is set in every Makefile target; see the note in the sibling agent
repos. The build needs the chain's protocol module at `../svpagent/protocol`,
and `deps_test.go` asserts this repo's replace directives still match core's.
