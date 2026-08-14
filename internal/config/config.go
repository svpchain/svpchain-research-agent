// Package config is the full-agent configuration, loaded from a TOML file.
//
// The schema is adapted from svpchain-mcp's cmd/mcp-server config: the same
// network endpoints, optional EVM/faucet/bridge/lendora families with the same
// all-or-nothing rules, and the same graceful degradation — an unset optional
// family means those operations refuse at call time with a reason, and the
// agent still boots. On top of that the agent adds its A2A identity
// (public_url) and an optional [operator] section holding the delegated
// signer's key reference; without it the execution skill refuses.
package config

import (
	"fmt"
	"path/filepath"
	"time"

	"cosmossdk.io/math"
	"github.com/BurntSushi/toml"
)

// Config is the agent's configuration.
type Config struct {
	DEXChain   DEXChainConfig   `toml:"dex_chain"`
	AgentChain AgentChainConfig `toml:"agent_chain"`
	ListenAddr string           `toml:"listen_addr"`

	// PublicURL is how callers reach this agent, advertised in the Agent Card.
	// Defaults to "http://localhost"+ListenAddr when empty.
	PublicURL string `toml:"public_url"`

	// FaucetBaseURL is the faucet backend's HTTP base URL. Optional: when
	// empty the faucet operations refuse.
	FaucetBaseURL string `toml:"faucet_base_url"`

	// TransferOutCapPath persists per-symbol daily transfer-out caps and the
	// running tally to a JSON file. Optional: when empty the state is
	// in-memory only and resets on restart. Relative paths resolve against
	// the config file's directory.
	TransferOutCapPath string `toml:"transfer_out_cap_path"`

	// BroadcastMode is informational for whoami; the agent always broadcasts
	// the signed tx a caller lands via broadcast_signed_tx.
	BroadcastMode string `toml:"broadcast_mode"`

	Cache    CacheConfig  `toml:"cache"`
	Limits   LimitsConfig `toml:"limits"`
	Fee      FeeConfig    `toml:"fee"`
	Operator Operator     `toml:"operator"`
}

// DEXChainConfig points the agent at the DEX chain (an EVM-compatible
// cosmos-sdk chain): its chain id and the endpoints the chain-facing families
// share — queries and broadcast over gRPC, tx status over CometBFT RPC, reads
// over the Comlink indexer, and the chain's EVM JSON-RPC. All but the EVM
// endpoint are required.
type DEXChainConfig struct {
	ID             string `toml:"id"`
	GrpcAddr       string `toml:"grpc_addr"`
	CometRPCURL    string `toml:"comet_rpc_url"`
	IndexerBaseURL string `toml:"indexer_base_url"`
}

// AgentChainConfig points the agent-identity families — x/agent registry,
// x/agentwallet delegation, and delegated execution — at the chain carrying
// those modules when it is not the DEX chain itself. The agent chain is
// reached over its Cosmos REST API (the gRPC-gateway, typically :1317), not
// gRPC. Optional: unset, those families run against the DEX chain connection
// (the single-chain default). Note delegated orders execute on whichever
// chain verifies the delegation, so a split deployment trades against the
// agent chain's CLOB.
type AgentChainConfig struct {
	ID      string `toml:"id"`
	RestURL string `toml:"rest_url"`
}

// Enabled reports whether a separate agent chain is configured.
func (a AgentChainConfig) Enabled() bool { return a.RestURL != "" }

// Operator configures the agent's own on-chain identity: the eth_secp256k1
// key it signs delegated executions (and its own registration) with, and the
// registration metadata it advertises. Optional — without a key the
// svpchain-execution skill refuses with a reason, exactly like the other
// unconfigured families.
type Operator struct {
	// KeyFile is a file holding the operator private key as hex. The
	// SVPCHAIN_AGENT_OPERATOR_KEY env var takes precedence when set, so
	// deployments can inject the key without touching disk.
	KeyFile string `toml:"key_file"`

	// Capabilities are the capability strings registered on chain.
	Capabilities []string `toml:"capabilities"`

	// Metadata is the free-form metadata registered on chain.
	Metadata string `toml:"metadata"`
}

// FeeConfig sets the gas fee stamped onto non-CLOB txs. Short-term CLOB
// orders are gas-free on svpchain and always ship with an empty fee.
type FeeConfig struct {
	Denom    string `toml:"denom"`
	Amount   string `toml:"amount"`
	GasLimit uint64 `toml:"gas_limit"`
}

// LimitsConfig caps the size of funds movements, in human USDC. Zero
// disables the corresponding check.
type LimitsConfig struct {
	DepositMaxUSDC       uint64 `toml:"deposit_max_usdc"`
	WithdrawMaxUSDC      uint64 `toml:"withdraw_max_usdc"`
	TransferMaxUSDC      uint64 `toml:"transfer_max_usdc"`
	DailyWithdrawCapUSDC uint64 `toml:"daily_withdraw_cap_usdc"`
}

type CacheConfig struct {
	// MarketsRefresh is parsed as a Go duration string ("60s", "2m"…).
	// Zero means the package default in markets.NewCache.
	MarketsRefresh Duration `toml:"markets_refresh"`
}

// Duration parses TOML strings like "60s" into a time.Duration.
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

// Load reads and validates a TOML config file.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("decode TOML %s: %w", path, err)
	}
	c.ApplyDefaults()
	// Relative paths resolve against the config file's own directory, so the
	// "operator.key next to agent.toml" layout the deploy script ships works
	// regardless of where the agent is launched from.
	if c.TransferOutCapPath != "" && !filepath.IsAbs(c.TransferOutCapPath) {
		c.TransferOutCapPath = filepath.Join(filepath.Dir(path), c.TransferOutCapPath)
	}
	if c.Operator.KeyFile != "" && !filepath.IsAbs(c.Operator.KeyFile) {
		c.Operator.KeyFile = filepath.Join(filepath.Dir(path), c.Operator.KeyFile)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults fills the fields a config may omit.
func (c *Config) ApplyDefaults() {
	if c.BroadcastMode == "" {
		c.BroadcastMode = "server"
	}
	if c.PublicURL == "" && c.ListenAddr != "" {
		c.PublicURL = "http://localhost" + c.ListenAddr
	}
	c.Fee.applyDefaults()
}

// Validate enforces the required network-level fields and the optional
// families' invariants.
func (c *Config) Validate() error {
	if c.DEXChain.ID == "" {
		return fmt.Errorf("dex_chain.id is required")
	}
	if c.DEXChain.GrpcAddr == "" {
		return fmt.Errorf("dex_chain.grpc_addr is required")
	}
	if c.DEXChain.CometRPCURL == "" {
		return fmt.Errorf("dex_chain.comet_rpc_url is required")
	}
	if c.DEXChain.IndexerBaseURL == "" {
		return fmt.Errorf("dex_chain.indexer_base_url is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if (c.AgentChain.ID == "") != (c.AgentChain.RestURL == "") {
		return fmt.Errorf("agent_chain.id and agent_chain.rest_url must be set together")
	}
	if err := c.Fee.validate(); err != nil {
		return err
	}
	return nil
}

// Default fee applied to non-CLOB txs when the [fee] section is absent.
// Matches a chain whose minimum-gas-prices is 25000000000asvp at a
// 1,000,000 gas limit (≈0.025 SVP total).
const (
	DefaultFeeDenom    = "asvp"
	DefaultFeeAmount   = "25000000000000000"
	DefaultFeeGasLimit = uint64(1_000_000)
)

func (f *FeeConfig) applyDefaults() {
	if f.Denom == "" {
		f.Denom = DefaultFeeDenom
	}
	if f.Amount == "" {
		f.Amount = DefaultFeeAmount
	}
	if f.GasLimit == 0 {
		f.GasLimit = DefaultFeeGasLimit
	}
}

func (f *FeeConfig) validate() error {
	if f.Denom == "" {
		return fmt.Errorf("fee.denom is required")
	}
	amt, ok := math.NewIntFromString(f.Amount)
	if !ok {
		return fmt.Errorf("fee.amount %q is not a valid integer", f.Amount)
	}
	if amt.IsNegative() {
		return fmt.Errorf("fee.amount %q must be non-negative", f.Amount)
	}
	return nil
}
