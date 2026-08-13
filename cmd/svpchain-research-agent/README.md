# svpchain-research-agent

A thin **example** agent that plays the middle role in a multi-hop delegation.

It exists to demonstrate — and keep tested — the one part of the SVP-DT design
no other component exercises: an agent that *receives* a re-delegable
credential, narrows it, and hands the execution leg to another agent, which
executes on the **original user's** account.

```
user ──credential (re-delegable → perps-agent only)──▶ research-agent
                                                        │  attenuates: fewer actions,
                                                        │  smaller budget, one subaccount
                                                        ▼
                                                     perps-agent ──▶ chain
                                                     (position lands on the USER's subaccount)
```

It is not a product. Its "research" is a fixed string; everything real about it
is the credential handling, which is reused from the other agents' packages
rather than reimplemented.

## What it demonstrates

- **Attenuation is enforced, not trusted.** The child is minted with
  `svpdt.Attenuate`, which re-runs monotonicity against the parent and refuses
  to sign anything wider — this agent cannot pass on more than it holds even
  if it tries.
- **The downstream never asks this agent anything.** It receives the whole
  chain and verifies back to the user from the chain registry alone.
- **The principal survives every hop.** The position, margin and liquidation
  risk stay with the user; neither agent can name itself.
- **One hop only.** The child is explicitly non-re-delegable with an empty
  (constrained) target set — the headroom the user granted is spent here.
- **The settlement binding rides along.** The downstream is paid out of the
  same escrow the user opened for this task, and this agent can record its own
  fee against it before forwarding.

## Running it

Needs the same TOML config as the DEX agent — the operator key is this agent's
identity, and the chain endpoints are what it verifies credentials against:

```sh
go build ./cmd/svpchain-research-agent
./svpchain-research-agent -config agent.toml -listen :8081
```

Register it like any other agent (`agent_self_register` on the DEX agent's
surface, or `svpd tx agent register`), then a caller sends:

```jsonc
// message.metadata
{"svp.delegation/v1": {"tokens": ["<base64 credential, re-delegable to the perps-agent>"]}}

// message text
{
  "skill": "svpchain-research",
  "tool": "research_and_execute",
  "args": {
    "topic": "BTC-USD",
    "downstream_did": "did:svp:svp1…",
    "downstream_endpoint": "http://127.0.0.1:8082",
    "envelope": {"skill":"svpchain-execution","tool":"execute_place_order","args":{"order":{…}}},
    "narrow": {"actions":["clob.place_order"], "subaccounts":[7], "budget":{"denom":"uusdc","amount":"250000"}},
    "fee": {"denom":"uusdc","amount":"40000"}
  }
}
```

The credential the caller mints must be `redelegable: true` with the
downstream agent's DID in `redelegate_to` — from the wallet agent, that is
`delegate_task` with those two fields set.

## Where the code is

| | |
|---|---|
| The role | `internal/intermediary/intermediary.go` — verify, attenuate, forward, record |
| A2A surface | `internal/intermediary/server.go` — one skill, one tool, card with the delegation extension |
| Transport | `internal/intermediary/sender.go` — credential in `message.metadata` |
| Tests | `internal/intermediary/*_test.go` — including a chain accepted by the *real* `delegated.Service` verifier |
