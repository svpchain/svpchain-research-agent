package toolbridge

import (
	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"
)

// Skill IDs. One per operation family on the Agent Card; the registry tags
// every operation with its skill so the card and the dispatch table cannot
// drift (a test pins the mapping).
const (
	SkillMarketData    = "svpchain-market-data"
	SkillAccount       = "svpchain-account"
	SkillTrading       = "svpchain-trading"
	SkillFunds         = "svpchain-funds"
	SkillBroadcast     = "svpchain-broadcast"
	SkillAuth          = "svpchain-auth"
	SkillFaucet        = "svpchain-faucet"
	SkillEVM           = "svpchain-evm"
	SkillAgentRegistry = "svpchain-agent-registry"
	SkillDelegation    = "svpchain-delegation"
	SkillExecution     = "svpchain-execution"
)

// NewEmpty returns a registry with nothing registered. A per-category binary
// starts here and composes exactly the families it serves; the full agent
// composes everything via New.
func NewEmpty() *Registry { return newRegistry() }

// RegisterMarketData adds the read-only market intelligence tools.
func (r *Registry) RegisterMarketData(h *tools.Handlers) {
	r.add(SkillMarketData, "list_markets", adapt(h.ListMarkets))
	r.add(SkillMarketData, "get_market", adapt(h.GetMarket))
	r.add(SkillMarketData, "get_orderbook", adapt(h.GetOrderbook))
	r.add(SkillMarketData, "get_oracle_price", adapt(h.GetOraclePrice))
	r.add(SkillMarketData, "get_candles", adapt(h.GetCandles))
	r.add(SkillMarketData, "get_trades", adapt(h.GetTrades))
	r.add(SkillMarketData, "get_sparklines", adapt(h.GetSparklines))
	r.add(SkillMarketData, "get_historical_funding", adapt(h.GetHistoricalFunding))
	r.add(SkillMarketData, "get_height", adapt(h.GetHeight))
	r.add(SkillMarketData, "get_time", adapt(h.GetTime))
}

// RegisterAccount adds the owner-scoped account and position reads.
func (r *Registry) RegisterAccount(h *tools.Handlers) {
	r.add(SkillAccount, "get_subaccount", adapt(h.GetSubaccount))
	r.add(SkillAccount, "get_live_subaccount", adapt(h.GetLiveSubaccount))
	r.add(SkillAccount, "get_balance", adapt(h.GetBalance))
	r.add(SkillAccount, "whoami", adapt(h.Whoami))
	r.add(SkillAccount, "get_orders", adapt(h.GetOrders))
	r.add(SkillAccount, "get_order", adapt(h.GetOrder))
	r.add(SkillAccount, "get_fills", adapt(h.GetFills))
	r.add(SkillAccount, "get_transfers", adapt(h.GetTransfers))
	r.add(SkillAccount, "get_pnl", adapt(h.GetPnl))
	r.add(SkillAccount, "get_historical_pnl", adapt(h.GetHistoricalPnl))
	r.add(SkillAccount, "get_funding_payments", adapt(h.GetFundingPayments))
}

// RegisterTrading adds the build-only order tools (the caller signs and lands
// the payload via broadcast_signed_tx).
func (r *Registry) RegisterTrading(h *tools.Handlers) {
	r.add(SkillTrading, "build_place_limit_order", adapt(h.BuildPlaceLimitOrder))
	r.add(SkillTrading, "build_place_market_order", adapt(h.BuildPlaceMarketOrder))
	r.add(SkillTrading, "build_place_conditional_order", adapt(h.BuildPlaceConditionalOrder))
	r.add(SkillTrading, "build_cancel_order", adapt(h.BuildCancelOrder))
	r.add(SkillTrading, "build_batch_cancel_orders", adapt(h.BuildBatchCancelOrders))
}

// RegisterFunds adds funds movements and transfer-out caps.
func (r *Registry) RegisterFunds(h *tools.Handlers) {
	r.add(SkillFunds, "build_deposit_to_subaccount", adapt(h.BuildDepositToSubaccount))
	r.add(SkillFunds, "build_withdraw_from_subaccount", adapt(h.BuildWithdrawFromSubaccount))
	r.add(SkillFunds, "build_transfer_between_subaccounts", adapt(h.BuildTransferBetweenSubaccounts))
	r.add(SkillFunds, "build_bank_send", adapt(h.BuildBankSend))
	r.add(SkillFunds, "get_transfer_out_cap", adapt(h.GetTransferOutCap))
	r.add(SkillFunds, "set_transfer_out_cap", adapt(h.SetTransferOutCap))
}

// RegisterBroadcast adds the Cosmos broadcast + status rail.
func (r *Registry) RegisterBroadcast(h *tools.Handlers) {
	r.add(SkillBroadcast, "broadcast_signed_tx", adapt(h.BroadcastSignedTx))
	r.add(SkillBroadcast, "get_tx_status", adapt(h.GetTxStatus))
}

// RegisterAuth adds self-service auth. Every binary that serves owner-scoped
// tools needs this family: it mints the bearer the other families gate on.
func (r *Registry) RegisterAuth(h *tools.Handlers) {
	r.add(SkillAuth, "auth_challenge", adapt(h.AuthChallenge))
	r.add(SkillAuth, "auth_verify", adapt(h.AuthVerify))
}

// RegisterFaucet adds the testnet faucet.
func (r *Registry) RegisterFaucet(h *tools.Handlers) {
	r.add(SkillFaucet, "list_faucet_tokens", adapt(h.ListFaucetTokens))
	r.add(SkillFaucet, "faucet_claim", adapt(h.FaucetClaim))
}

// RegisterEVMBroadcast adds only the EVM landing rail — broadcast a raw tx and
// track its status. The lending binary registers this without the rest of the
// EVM family: Lendora builds return EVM payloads that land here.
func (r *Registry) RegisterEVMBroadcast(h *tools.Handlers) {
	r.add(SkillEVM, "broadcast_evm_tx", adapt(h.BroadcastEVMTx))
	r.add(SkillEVM, "evm_tx_status", adapt(h.EVMTxStatus))
}

// RegisterEVMDeFi adds the EVM DeFi surface: swap, bridge, ERC-20/721.
func (r *Registry) RegisterEVMDeFi(h *tools.Handlers) {
	r.add(SkillEVM, "quote_swap", adapt(h.QuoteSwap))
	r.add(SkillEVM, "build_token_approval", adapt(h.BuildTokenApproval))
	r.add(SkillEVM, "build_swap", adapt(h.BuildSwap))
	r.add(SkillEVM, "build_bridge_deposit", adapt(h.BuildBridgeDeposit))
	r.add(SkillEVM, "build_bridge_deposit_inbound", adapt(h.BuildBridgeDepositInbound))
	r.add(SkillEVM, "build_erc20_transfer", adapt(h.BuildERC20Transfer))
	r.add(SkillEVM, "build_erc20_approve", adapt(h.BuildERC20Approve))
	r.add(SkillEVM, "build_erc20_transfer_from", adapt(h.BuildERC20TransferFrom))
	r.add(SkillEVM, "build_erc721_transfer_from", adapt(h.BuildERC721TransferFrom))
	r.add(SkillEVM, "build_erc721_safe_transfer_from", adapt(h.BuildERC721SafeTransferFrom))
	r.add(SkillEVM, "build_erc721_approve", adapt(h.BuildERC721Approve))
	r.add(SkillEVM, "build_erc721_set_approval_for_all", adapt(h.BuildERC721SetApprovalForAll))
}

// RegisterEVM adds the whole EVM family.
func (r *Registry) RegisterEVM(h *tools.Handlers) {
	r.RegisterEVMBroadcast(h)
	r.RegisterEVMDeFi(h)
}

// New builds the full-surface operation registry over the MCP tool handlers —
// every bridged family. The optional services (agent-registry / delegation
// queries, delegated execution) are registered by their own Register*
// functions so milestones land independently; nil-safe wiring lives there.
func New(h *tools.Handlers) *Registry {
	r := newRegistry()
	r.RegisterMarketData(h)
	r.RegisterAccount(h)
	r.RegisterTrading(h)
	r.RegisterFunds(h)
	r.RegisterBroadcast(h)
	r.RegisterAuth(h)
	r.RegisterFaucet(h)
	r.RegisterEVM(h)
	return r
}
