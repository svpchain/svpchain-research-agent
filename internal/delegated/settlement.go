// The settlement surface: how this agent gets paid.
//
// A caller's task rides an SVP-DT credential bound (via its settlement
// caveat) to an escrow order the user opened on chain. After doing the work,
// the agent records its spend against that order — wrapped in
// MsgAgentExecDelegated, so "who may record" is decided by the credential
// chain — and later claims the accrued balance with its own operator key.
package delegated

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"google.golang.org/grpc"

	agentwallettypes "github.com/dydxprotocol/v4-chain/protocol/x/agentwallet/types"
	settlementtypes "github.com/dydxprotocol/v4-chain/protocol/x/settlement/types"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
)

// ActionRecordSpend must stay byte-identical to the chain registry's
// settlement.record_spend action tag.
const ActionRecordSpend = "settlement.record_spend"

// SettlementQuerier is the slice of the x/settlement QueryClient the service
// needs — an interface so tests fake it without a gRPC conn. Nil is legal:
// the read operation refuses, the write operations never consult it.
type SettlementQuerier interface {
	Settlement(ctx context.Context, in *settlementtypes.QuerySettlement, opts ...grpc.CallOption) (*settlementtypes.QuerySettlementResponse, error)
	ClaimablesBySettlement(ctx context.Context, in *settlementtypes.QueryClaimablesBySettlement, opts ...grpc.CallOption) (*settlementtypes.QueryClaimablesBySettlementResponse, error)
}

// RecordSpendParams describes one recorded expenditure.
//
// The settlement id is optional and purely a cross-check: the order charged is
// always the one the credential's settlement caveat names, which the chain
// requires it to carry. Passing a different id is refused rather than honoured.
type RecordSpendParams struct {
	SettlementID string          `json:"settlement_id,omitempty"` // hex; must equal the credential's binding
	Amount       agentchain.Coin `json:"amount"`
}

type ExecRecordSpendInput struct {
	Proof  []string          `json:"proof"`
	Record RecordSpendParams `json:"record"`
}

// ExecuteRecordSpend verifies the proof and records this agent's spend against
// the caller's settlement order, accruing a claimable. The chain re-verifies
// everything — the credential's settlement binding, the service budget, the
// escrow cap — and refuses a record naming any agent but the executing one.
func (s *Service) ExecuteRecordSpend(ctx context.Context, in ExecRecordSpendInput) (ExecResult, error) {
	tokens, verified, err := s.verifyProof(ctx, in.Proof)
	if err != nil {
		return ExecResult{}, err
	}
	// The registry pins settlement messages to subaccount 0 — settlement is
	// bank-level — so the credential must grant it.
	if err := preflight(verified, ActionRecordSpend, 0); err != nil {
		return ExecResult{}, err
	}

	// ★ The credential names the order, and nothing else can. The chain
	// requires the binding — a record under an unbound credential is
	// DT_SETTLEMENT_DENIED — so there is no order for such a credential to
	// charge, whatever the caller passes.
	bound := verified.Effective.Settlement
	if bound == "" {
		return ExecResult{}, fmt.Errorf(
			"credential has no settlement binding: mint the task credential with a settlement caveat naming the escrow order to be charged")
	}
	// Fast-fail on a mismatch the chain would reject anyway, with the caveat
	// named — a caller learns which binding it violated instead of a DT code.
	idHex := in.Record.SettlementID
	if idHex == "" {
		idHex = bound
	}
	if idHex != bound {
		return ExecResult{}, fmt.Errorf(
			"record names settlement %s but the credential is bound to %s", idHex, bound)
	}
	id, err := settlementtypes.ParseSettlementID(idHex)
	if err != nil {
		return ExecResult{}, fmt.Errorf("settlement_id: %w", err)
	}
	amount, err := in.Record.Amount.ToSDK()
	if err != nil {
		return ExecResult{}, fmt.Errorf("amount: %w", err)
	}

	inner := &settlementtypes.MsgRecordSpend{
		Principal:    verified.Principal,
		SettlementId: id,
		AgentId:      s.agentID,
		Amount:       amount,
	}
	if err := inner.ValidateBasic(); err != nil {
		return ExecResult{}, err
	}
	return s.execute(ctx, tokens, verified, inner)
}

type ClaimInput struct {
	SettlementID string `json:"settlement_id"` // hex
}

type ClaimOutput struct {
	ExecResult
	// What the order's books showed before the claim, when the querier is
	// wired; the claim itself pays whatever is actually accrued.
	Claimable *agentchain.Coin `json:"claimable,omitempty"`
}

// Claim withdraws this agent's accrued claimable on one settlement order,
// signed with the operator's own key — earnings flow to the operator account,
// minus the chain's marketplace commission. Operator-gated: a claim moves
// this deployment's money, not a caller's.
func (s *Service) Claim(ctx context.Context, in ClaimInput) (ClaimOutput, error) {
	if err := s.requireOperator(ctx, "a claim withdraws this agent's earnings and"); err != nil {
		return ClaimOutput{}, err
	}
	id, err := settlementtypes.ParseSettlementID(in.SettlementID)
	if err != nil {
		return ClaimOutput{}, fmt.Errorf("settlement_id: %w", err)
	}

	out := ClaimOutput{}
	if s.cfg.SettlementQ != nil {
		if resp, err := s.cfg.SettlementQ.ClaimablesBySettlement(
			ctx, &settlementtypes.QueryClaimablesBySettlement{SettlementId: id},
		); err == nil {
			for _, c := range resp.Claimables {
				if c.AgentId == s.agentID {
					out.Claimable = &agentchain.Coin{Denom: c.Amount.Denom, Amount: c.Amount.Amount.String()}
				}
			}
		}
	}

	msg := &settlementtypes.MsgClaim{
		Claimer:      s.cfg.Operator,
		SettlementId: id,
		AgentId:      s.agentID,
	}
	if err := msg.ValidateBasic(); err != nil {
		return ClaimOutput{}, err
	}
	res, err := s.signAndBroadcastOwn(ctx, msg)
	if err != nil {
		return ClaimOutput{}, err
	}
	out.ExecResult = res
	return out, nil
}

type GetSettlementInput struct {
	SettlementID string `json:"settlement_id"` // hex
}

// SettlementView is the JSON-friendly rendering of one order and its
// outstanding claimables.
type SettlementView struct {
	SettlementID     string          `json:"settlement_id"`
	Opener           string          `json:"opener"`
	Status           string          `json:"status"`
	Cap              agentchain.Coin `json:"cap"`
	TotalRecorded    agentchain.Coin `json:"total_recorded"`
	TotalClaimed     agentchain.Coin `json:"total_claimed"`
	Refunded         agentchain.Coin `json:"refunded"`
	RootDelegationID string          `json:"root_delegation_id,omitempty"`
	Memo             string          `json:"memo,omitempty"`
	Claimables       []ClaimableView `json:"claimables"`
}

type ClaimableView struct {
	AgentID string          `json:"agent_id"`
	Amount  agentchain.Coin `json:"amount"`
}

// GetSettlement reads one order and its outstanding claimables. No credential
// required: orders are public chain state, and a payee checking what it is
// owed should not need to spend a credential use to look.
func (s *Service) GetSettlement(ctx context.Context, in GetSettlementInput) (SettlementView, error) {
	if s.cfg.SettlementQ == nil {
		return SettlementView{}, fmt.Errorf(
			"this deployment has no settlement query endpoint configured")
	}
	id, err := settlementtypes.ParseSettlementID(in.SettlementID)
	if err != nil {
		return SettlementView{}, fmt.Errorf("settlement_id: %w", err)
	}
	resp, err := s.cfg.SettlementQ.Settlement(ctx, &settlementtypes.QuerySettlement{Id: id})
	if err != nil {
		return SettlementView{}, err
	}
	order := resp.Settlement

	view := SettlementView{
		SettlementID:  settlementtypes.SettlementIDToCaveat(order.Id),
		Opener:        order.Opener,
		Status:        order.Status.String(),
		Cap:           coinView(order.Cap),
		TotalRecorded: coinView(order.TotalRecorded),
		TotalClaimed:  coinView(order.TotalClaimed),
		Refunded:      coinView(order.Refunded),
		Memo:          order.Memo,
		Claimables:    []ClaimableView{},
	}
	if len(order.RootDelegationId) > 0 {
		view.RootDelegationID = agentwallettypes.HexRootID(order.RootDelegationId)
	}
	claims, err := s.cfg.SettlementQ.ClaimablesBySettlement(
		ctx, &settlementtypes.QueryClaimablesBySettlement{SettlementId: id})
	if err != nil {
		return SettlementView{}, err
	}
	for _, c := range claims.Claimables {
		view.Claimables = append(view.Claimables, ClaimableView{
			AgentID: c.AgentId,
			Amount:  coinView(c.Amount),
		})
	}
	return view, nil
}

func coinView(c sdk.Coin) agentchain.Coin {
	return agentchain.Coin{Denom: c.Denom, Amount: c.Amount.String()}
}
