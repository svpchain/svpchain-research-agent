package wire

import (
	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
	"github.com/svpchain/svpchain-research-agent/internal/toolbridge"
)

// Profile selects which operation families a binary registers.
//
// This binary ships exactly one — DelegationProfile — and it registers nothing
// at all. The indirection stays because BuildProfile is shared machinery that
// takes a Profile, and because an empty Register is a claim worth stating
// explicitly rather than an omission.
//
// It used to carry BuildEVM / BuildLendora / RunMarkets flags, gating dependency
// construction as well as registration, so an agent handed the shared full
// config would not dial endpoints for families it never served. This agent
// serves none of them, so there is nothing left to gate.
type Profile struct {
	Name string

	// Register composes the binary's operation registry. exec is nil on
	// keyless deployments; the execution registrations then advertise
	// informative refusals.
	Register func(r *toolbridge.Registry, h *tools.Handlers, agent *agentchain.Service, exec *delegated.Service)
}

// DelegationProfile is for an agent that serves no operation registry at all —
// the intermediary (research) agent, which exposes its own re-delegation
// endpoint and only needs the delegated service's verifier/recorder plus the
// agent-chain clients. It builds no EVM, no Lendora, and runs no caches, so it
// never reads routes.json or dials the EVM RPC for surfaces it does not serve.
var DelegationProfile = Profile{
	Name: "research",
	// Register nothing: this profile's binary does not dispatch the toolbridge
	// registry; it serves an intermediary A2A endpoint built on app.Delegated.
	Register: func(*toolbridge.Registry, *tools.Handlers, *agentchain.Service, *delegated.Service) {},
}
