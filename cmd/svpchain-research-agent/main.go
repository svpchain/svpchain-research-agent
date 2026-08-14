// Command svpchain-research-agent is a thin example agent that plays the
// middle role in a multi-hop delegation.
//
// It exists to demonstrate — and to keep tested — the one part of the
// delegation design no other component exercises: an agent that receives a
// re-delegable SVP-DT credential, narrows it, and hands the execution leg to
// a downstream agent (typically svpchain-remote-agents), which then executes on
// the *original user's* account. Neither hop can widen what it received, the
// downstream verifies the whole chain without asking this agent anything, and
// the user's emergency stop kills every hop at once.
//
// It is not a product. Its "research" is a fixed string; everything real
// about it is the credential handling, which is reused from the DEX agent's
// packages rather than reimplemented.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/svpchain/svpdt"

	"github.com/svpchain/svpchain-research-agent/internal/config"
	"github.com/svpchain/svpchain-research-agent/internal/intermediary"
	"github.com/svpchain/svpchain-research-agent/internal/operator"
	"github.com/svpchain/svpchain-research-agent/internal/wire"

	sdk "github.com/cosmos/cosmos-sdk/types"
	agenttypes "github.com/dydxprotocol/v4-chain/protocol/x/agent/types"
)

func main() {
	configPath := flag.String("config", "", "TOML config (same shape as the DEX agent's; the operator key and chain endpoints are what this agent uses)")
	listenAddr := flag.String("listen", ":8081", "bind address for the A2A endpoint")
	publicURL := flag.String("public-url", "", "public base URL advertised in the Agent Card (default http://127.0.0.1<listen>)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *configPath, *listenAddr, *publicURL); err != nil {
		fmt.Fprintf(os.Stderr, "svpchain-research-agent: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath, listenAddr, publicURL string) error {
	if configPath == "" {
		return fmt.Errorf("-config is required: this agent needs an operator key to sign the credentials it issues")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if publicURL == "" {
		publicURL = "http://127.0.0.1" + listenAddr
	}

	// The operator key is this agent's identity: its DID derives from the
	// operator address, and the credentials it issues must be signed with
	// that same key or they verify nowhere.
	priv, operatorAddr, err := operator.Load(cfg.Operator)
	if err != nil {
		return err
	}
	if priv == nil {
		return fmt.Errorf("no operator key configured: an intermediary must sign the credentials it passes on")
	}
	signer, err := svpdt.NewPrivateKeySigner(priv.Bytes())
	if err != nil {
		return fmt.Errorf("build credential signer: %w", err)
	}
	agentID := agenttypes.AgentIdFromOperator(sdk.MustAccAddressFromBech32(operatorAddr))

	// Everything credential-shaped is reused from the DEX agent's wiring: the
	// same verifier, resolver, and settlement recorder. The delegation-only
	// profile skips the EVM/Lendora/markets construction this agent never uses,
	// so it needs no routes.json and no EVM RPC.
	app, err := wire.BuildProfile(ctx, cfg, wire.DelegationProfile)
	if err != nil {
		return err
	}
	defer app.Close()
	if app.Delegated == nil {
		return fmt.Errorf("delegated services are not configured; check the [operator] and chain sections")
	}

	svc := intermediary.New(intermediary.Config{
		AgentID:    agentID,
		Signer:     signer,
		Verify:     app.Delegated,
		Downstream: intermediary.A2ASender{},
		Spend:      app.Delegated,
	})

	fmt.Fprintf(os.Stderr, "svpchain-research-agent: %s listening on %s\n", agentID, listenAddr)
	fmt.Fprintf(os.Stderr, "svpchain-research-agent: agent card at %s/.well-known/agent-card.json\n", publicURL)
	return intermediary.Serve(ctx, listenAddr, publicURL, agentID, svc)
}
