// Package wire assembles the agent's dependency graph from its config: chain
// gRPC + CometBFT + EVM clients, the indexer client, the markets caches, the
// self-service auth stores, the policy engine, and the MCP tool handlers the
// A2A tool bridge dispatches into.
//
// The body deliberately mirrors svpchain-mcp's cmd/mcp-server wiring (which is
// package main and cannot be imported): same optional families, same
// all-or-nothing rules, same graceful degradation. Drift between the two is a
// bug in whichever copied last.
package wire

import (
	"context"
	"fmt"
	"os"
	"time"

	"cosmossdk.io/log"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	agenttypes "github.com/dydxprotocol/v4-chain/protocol/x/agent/types"
	wallettypes "github.com/dydxprotocol/v4-chain/protocol/x/agentwallet/types"
	settlementtypes "github.com/dydxprotocol/v4-chain/protocol/x/settlement/types"
	"google.golang.org/grpc"

	"github.com/svpchain/svpchain-mcp/lib/mcp/auth"
	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/chain"
	"github.com/svpchain/svpchain-mcp/lib/mcp/faucet"
	"github.com/svpchain/svpchain-mcp/lib/mcp/indexer"
	"github.com/svpchain/svpchain-mcp/lib/mcp/limits"
	"github.com/svpchain/svpchain-mcp/lib/mcp/markets"
	"github.com/svpchain/svpchain-mcp/lib/mcp/mcpcodec"
	"github.com/svpchain/svpchain-mcp/lib/mcp/policy"
	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/agentrest"
	"github.com/svpchain/svpchain-research-agent/internal/config"
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
	"github.com/svpchain/svpchain-research-agent/internal/operator"
	"github.com/svpchain/svpchain-research-agent/internal/toolbridge"
)

// App is the wired agent: everything the A2A server needs to serve requests,
// plus the background caches it must run.
type App struct {
	Handlers *tools.Handlers      // the MCP tool handlers
	Registry *toolbridge.Registry // A2A operation registry over them
	Tenants  *auth.DynamicTenantStore
	Sessions *auth.SessionBearers
	Indexer  *indexer.Client
	GrpcConn *grpc.ClientConn
	Logger   log.Logger

	// Delegated is the execution service (nil when keyless). Exposed so the
	// server layer can hand it the served agent-card bytes — the capability
	// hash it registers on chain is sha256 of exactly those bytes.
	Delegated *delegated.Service

	// ReadTenants admits verified delegated-read credentials as synthetic
	// tenants; already registered as a policy dynamic source. Non-nil even
	// when Delegated is nil — it just never admits anyone then.
	ReadTenants *delegated.ReadTenantSource
}

// Close releases the app's long-lived connections.
func (a *App) Close() {
	if a.GrpcConn != nil {
		_ = a.GrpcConn.Close()
	}
}

// Run blocks until ctx is cancelled.
//
// This binary starts no background cache refreshers. The intermediary serves no
// operation registry at all — it verifies, narrows and forwards credentials —
// so there is no market or money-market data to keep warm, and nothing here
// that can fail.
func (a *App) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// dynamicTenantAdapter converts auth.TenantRecord into policy.TenantPolicy so
// the policy engine can resolve auto-issued tenants; kept here so auth never
// imports policy (mirrors the mcp-server adapter).
type dynamicTenantAdapter struct{ store *auth.DynamicTenantStore }

func (a dynamicTenantAdapter) LookupTenantPolicy(tenantID string) (policy.TenantPolicy, bool) {
	rec, err := a.store.LookupByTenantID(tenantID)
	if err != nil {
		return policy.TenantPolicy{}, false
	}
	return policy.TenantPolicy{
		TenantID:           rec.TenantID,
		Owner:              rec.Owner,
		AllowedSubaccounts: rec.AllowedSubaccounts,
		KillSwitch:         rec.KillSwitch,
	}, true
}

// multiDynamicSource fans the policy engine's single dynamic-source slot out
// to several tenant populations; the first source that answers wins.
type multiDynamicSource []policy.DynamicSource

func (m multiDynamicSource) LookupTenantPolicy(tenantID string) (policy.TenantPolicy, bool) {
	for _, src := range m {
		if tp, ok := src.LookupTenantPolicy(tenantID); ok {
			return tp, true
		}
	}
	return policy.TenantPolicy{}, false
}

// BuildProfile wires the configuration into a ready-to-run App registering
// only the profile's operation families.
func BuildProfile(ctx context.Context, cfg *config.Config, p Profile) (*App, error) {
	logger := log.NewLogger(os.Stderr).With("module", "remote-agent", "profile", p.Name)

	grpcConn, err := chain.Dial(ctx, cfg.DEXChain.GrpcAddr)
	if err != nil {
		return nil, fmt.Errorf("dial chain gRPC: %w", err)
	}
	encCfg := mcpcodec.GetEncodingConfig()

	chainDeps := tools.ChainDeps{
		Account:         chain.NewAccountClient(grpcConn, encCfg.InterfaceRegistry),
		Broadcast:       chain.NewBroadcastClient(grpcConn),
		ClobQuery:       chain.NewClobQueryClient(grpcConn),
		PerpetualsQuery: chain.NewPerpetualsQueryClient(grpcConn),
		SubaccountQuery: chain.NewSubaccountQueryClient(grpcConn),
		BankQuery:       chain.NewBankQueryClient(grpcConn),
	}
	cometClient, err := chain.NewCometBftClient(cfg.DEXChain.CometRPCURL)
	if err != nil {
		grpcConn.Close()
		return nil, fmt.Errorf("cometbft client: %w", err)
	}
	chainDeps.CometBft = cometClient

	var faucetClient *faucet.Client
	if cfg.FaucetBaseURL != "" {
		faucetClient = faucet.NewClient(cfg.FaucetBaseURL, faucet.Options{})
	}

	idx := indexer.NewClient(cfg.DEXChain.IndexerBaseURL, indexer.Options{})
	mkts := markets.NewCache(chainDeps.ClobQuery, chainDeps.PerpetualsQuery, time.Duration(cfg.Cache.MarketsRefresh), logger)

	limitsCfg := limits.Config{
		DepositMaxUSDC:       cfg.Limits.DepositMaxUSDC,
		WithdrawMaxUSDC:      cfg.Limits.WithdrawMaxUSDC,
		TransferMaxUSDC:      cfg.Limits.TransferMaxUSDC,
		DailyWithdrawCapUSDC: cfg.Limits.DailyWithdrawCapUSDC,
	}
	withdrawLedger := limits.NewMemoryLedger(limitsCfg.DailyWithdrawCapUSDC, nil)
	transferOut, err := limits.LoadMemoryTransferOutStore(cfg.TransferOutCapPath, nil, func(err error) {
		logger.Error("transfer-out cap persistence failed", "error", err)
	})
	if err != nil {
		grpcConn.Close()
		return nil, fmt.Errorf("load transfer-out cap state: %w", err)
	}

	// Self-service auth state: in-memory + TTL-bounded, same defaults as the
	// MCP server (auto-issued tenants get subaccounts 0..9).
	nonceStore := auth.NewNonceStore(auth.DefaultChallengeTTL, nil)
	dynamicTenants := auth.NewDynamicTenantStore(auth.DynamicTenantStoreConfig{
		BearerTTL:                 auth.DefaultBearerTTL,
		DefaultAllowedSubaccounts: []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	}, nil)
	ipLimit := auth.NewIPRateLimiter(auth.DefaultIPChallengeRate, auth.DefaultIPChallengeWindow, nil)
	sessionBearers := auth.NewSessionBearers(auth.DefaultBearerTTL, nil)

	// Two dynamic tenant populations resolve through the engine's single
	// fallback slot: bearer-minted tenants ("auto-…") and proof-derived
	// delegated-read tenants ("svpdt-…"). The prefixes keep the id spaces
	// disjoint, so first-hit-wins never shadows.
	readTenants := delegated.NewReadTenantSource(nil)
	policyEngine := policy.NewEngine(nil)
	policyEngine.SetDynamicSource(multiDynamicSource{
		dynamicTenantAdapter{store: dynamicTenants},
		readTenants,
	})

	deps := tools.Deps{
		Chain:             chainDeps,
		Indexer:           idx,
		Markets:           mkts,
		Builder:           builder.NewAssembler(cfg.DEXChain.ID, cfg.Fee.Denom, cfg.Fee.Amount, cfg.Fee.GasLimit),
		Faucet:            faucetClient,
		Policy:            policyEngine,
		Auditor:           policy.NewStdoutAuditor(),
		Idempotency:       policy.NewIdempotency(0),
		RateLimit:         policy.NewRateLimiter(0, 0),
		Limits:            limitsCfg,
		WithdrawLedger:    withdrawLedger,
		TransferOut:       transferOut,
		NonceStore:        nonceStore,
		DynamicTenants:    dynamicTenants,
		IPChallengeLimit:  ipLimit,
		SessionBearers:    sessionBearers,
		Logger:            logger,
		InterfaceRegistry: encCfg.InterfaceRegistry,
		BroadcastMode:     cfg.BroadcastMode,
	}
	handlers := tools.New(cfg.DEXChain.ID, deps)

	registry := toolbridge.NewEmpty()

	// The agent-identity families — x/agent registry, x/agentwallet
	// delegation, delegated execution — run against the chain that carries
	// those modules. By default that is the DEX chain itself over the shared
	// gRPC conn; a configured [agent_chain] switches everything below
	// (queries, tx builds, the operator's signing and broadcast) to that
	// chain's Cosmos REST API instead.
	agentChainID := cfg.DEXChain.ID
	var (
		agentQ         agentchain.AgentQuerier
		walletQ        agentchain.WalletQuerier
		settlementQ    delegated.SettlementQuerier
		agentAccount   chain.AccountClient   = chainDeps.Account
		agentBroadcast chain.BroadcastClient = chainDeps.Broadcast
		agentAuth      delegated.AuthAccountQuerier
	)
	if cfg.AgentChain.Enabled() {
		rest := agentrest.New(cfg.AgentChain.RestURL, encCfg.Codec, encCfg.InterfaceRegistry)
		agentChainID = cfg.AgentChain.ID
		agentQ = rest
		walletQ = rest.Wallet()
		settlementQ = rest
		agentAccount = rest.AccountClient()
		agentBroadcast = rest
		agentAuth = rest
		logger.Info("agent chain configured", "chain_id", agentChainID, "rest", cfg.AgentChain.RestURL)
	} else {
		agentQ = agenttypes.NewQueryClient(grpcConn)
		walletQ = wallettypes.NewQueryClient(grpcConn)
		settlementQ = settlementtypes.NewQueryClient(grpcConn)
		agentAuth = authtypes.NewQueryClient(grpcConn)
	}
	agentAsm := builder.NewAssembler(agentChainID, cfg.Fee.Denom, cfg.Fee.Amount, cfg.Fee.GasLimit)
	agentSvc := agentchain.New(agentQ, walletQ, agentAsm, agentAccount, policyEngine, agentBroadcast, encCfg.InterfaceRegistry)

	// Delegated execution goes live only when an operator key is configured;
	// keyless deployments register the same operations as informative
	// refusals, keeping the read layer's "advertised but refused" contract.
	operatorPriv, operatorAddr, err := operator.Load(cfg.Operator)
	if err != nil {
		grpcConn.Close()
		return nil, err
	}
	var delegatedSvc *delegated.Service
	if operatorPriv != nil {
		delegatedSvc = delegated.New(delegated.Config{
			Priv:         operatorPriv,
			Operator:     operatorAddr,
			ChainID:      agentChainID,
			Fee:          operator.FeeSpec{Denom: cfg.Fee.Denom, Amount: cfg.Fee.Amount, GasLimit: cfg.Fee.GasLimit},
			AgentQ:       agentQ,
			AuthQ:        delegated.NewAuthKeyClient(agentAuth, encCfg.InterfaceRegistry),
			WalletQ:      walletQ,
			SettlementQ:  settlementQ,
			Markets:      mkts,
			Account:      agentAccount,
			Broadcast:    agentBroadcast,
			Policy:       policyEngine,
			Limits:       limitsCfg,
			Endpoint:     cfg.PublicURL,
			Capabilities: cfg.Operator.Capabilities,
			Metadata:     cfg.Operator.Metadata,
		})
		logger.Info("delegated execution enabled", "operator", operatorAddr)
	}

	// Registration runs last so every service the profile may reference —
	// including a nil delegatedSvc, whose registrations become informative
	// refusals — is in its final state.
	p.Register(registry, handlers, agentSvc, delegatedSvc)

	return &App{
		Handlers:    handlers,
		Registry:    registry,
		Tenants:     dynamicTenants,
		Sessions:    sessionBearers,
		Indexer:     idx,
		GrpcConn:    grpcConn,
		Delegated:   delegatedSvc,
		ReadTenants: readTenants,
		Logger:      logger,
	}, nil
}
