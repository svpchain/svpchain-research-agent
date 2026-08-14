package delegated

import (
	"context"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/svpchain/svpdt"
	"google.golang.org/grpc"

	wallettypes "github.com/dydxprotocol/v4-chain/protocol/x/agentwallet/types"
	settlementtypes "github.com/dydxprotocol/v4-chain/protocol/x/settlement/types"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
)

// testOrderID is the settlement order the credential binds to.
func testOrderID() []byte {
	id := make([]byte, settlementtypes.SettlementIDLen)
	id[0] = 0xC0
	id[1] = 0x01
	return id
}

// settlementProof mints a credential granting the record action on
// subaccount 0, bound to boundTo, with a service budget.
func settlementProof(t *testing.T, f *fixture, boundTo []byte) []string {
	t.Helper()
	return f.issue(t, func(p *svpdt.IssueParams) {
		p.Caveats.Actions = svpdt.StringSet{ActionRecordSpend}
		p.Caveats.Settlement = settlementtypes.SettlementIDToCaveat(boundTo)
		p.Caveats.SvcBudget = p.Caveats.Budget
	})
}

// A granted record broadcasts a wrapper whose inner message records this
// agent's spend against the credential's bound order, for the credential's
// principal — the order id defaulted from the caveat, never chosen freely.
func TestExecuteRecordSpendBindsToTheCredential(t *testing.T) {
	f := newFixture(t)

	res, err := f.svc.ExecuteRecordSpend(context.Background(), ExecRecordSpendInput{
		Proof:  settlementProof(t, f, testOrderID()),
		Record: RecordSpendParams{Amount: agentchain.Coin{Denom: "uusdc", Amount: "40"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Principal != testDelegator {
		t.Fatalf("principal = %s", res.Principal)
	}

	var wrapper wallettypes.MsgAgentExecDelegated
	decodeSoleTxMsg(t, f.broadcast.txBytes, "/dydxprotocol.agentwallet.MsgAgentExecDelegated", &wrapper)

	var inner settlementtypes.MsgRecordSpend
	if err := proto.Unmarshal(wrapper.InnerMsg.Value, &inner); err != nil {
		t.Fatal(err)
	}
	if inner.Principal != testDelegator {
		t.Errorf("record principal = %s, want the credential's %s", inner.Principal, testDelegator)
	}
	if inner.AgentId != f.agentID {
		t.Errorf("record agent = %s, want the executing agent %s", inner.AgentId, f.agentID)
	}
	if string(inner.SettlementId) != string(testOrderID()) {
		t.Errorf("settlement id %X, want the caveat's %X", inner.SettlementId, testOrderID())
	}
	if inner.Amount.Denom != "uusdc" || !inner.Amount.Amount.Equal(sdkmath.NewInt(40)) {
		t.Errorf("amount = %s", inner.Amount)
	}
}

func TestExecuteRecordSpendRefusals(t *testing.T) {
	t.Run("explicit id conflicting with the binding", func(t *testing.T) {
		f := newFixture(t)
		other := make([]byte, settlementtypes.SettlementIDLen)
		other[0] = 0xEE
		_, err := f.svc.ExecuteRecordSpend(context.Background(), ExecRecordSpendInput{
			Proof: settlementProof(t, f, testOrderID()),
			Record: RecordSpendParams{
				SettlementID: settlementtypes.SettlementIDToCaveat(other),
				Amount:       agentchain.Coin{Denom: "uusdc", Amount: "1"},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "bound to") {
			t.Errorf("conflicting binding must be refused by name, got %v", err)
		}
	})

	// An unbound credential can never record: the chain requires the binding,
	// so passing the order id in the args does not substitute for it.
	t.Run("credential without a binding", func(t *testing.T) {
		f := newFixture(t)
		unbound := func(p *svpdt.IssueParams) {
			p.Caveats.Actions = svpdt.StringSet{ActionRecordSpend}
			p.Caveats.SvcBudget = p.Caveats.Budget
		}

		for _, tc := range []struct {
			name string
			args RecordSpendParams
		}{
			{"no id passed", RecordSpendParams{
				Amount: agentchain.Coin{Denom: "uusdc", Amount: "1"},
			}},
			{"id passed in args", RecordSpendParams{
				SettlementID: settlementtypes.SettlementIDToCaveat(testOrderID()),
				Amount:       agentchain.Coin{Denom: "uusdc", Amount: "1"},
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := f.svc.ExecuteRecordSpend(context.Background(), ExecRecordSpendInput{
					Proof:  f.issue(t, unbound),
					Record: tc.args,
				})
				if err == nil || !strings.Contains(err.Error(), "no settlement binding") {
					t.Errorf("unbound credential must be refused, got %v", err)
				}
			})
		}
	})

	t.Run("ungranted action", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.ExecuteRecordSpend(context.Background(), ExecRecordSpendInput{
			Proof:  f.issue(t, nil), // fixture default: place + cancel only
			Record: RecordSpendParams{Amount: agentchain.Coin{Denom: "uusdc", Amount: "1"}},
		})
		if err == nil || !strings.Contains(err.Error(), "does not grant action") {
			t.Errorf("ungranted action must be refused by name, got %v", err)
		}
	})
}

// fakeSettlementQ serves one order and its claimables.
type fakeSettlementQ struct {
	order      settlementtypes.SettlementOrder
	claimables []settlementtypes.Claimable
}

func (q fakeSettlementQ) Settlement(_ context.Context, in *settlementtypes.QuerySettlement, _ ...grpc.CallOption) (*settlementtypes.QuerySettlementResponse, error) {
	return &settlementtypes.QuerySettlementResponse{Settlement: q.order}, nil
}

func (q fakeSettlementQ) ClaimablesBySettlement(_ context.Context, in *settlementtypes.QueryClaimablesBySettlement, _ ...grpc.CallOption) (*settlementtypes.QueryClaimablesBySettlementResponse, error) {
	return &settlementtypes.QueryClaimablesBySettlementResponse{Claimables: q.claimables}, nil
}

func stubOrder(f *fixture) fakeSettlementQ {
	coin := func(n int64) sdk.Coin { return sdk.NewCoin("uusdc", sdkmath.NewInt(n)) }
	return fakeSettlementQ{
		order: settlementtypes.SettlementOrder{
			Id:            testOrderID(),
			Opener:        testDelegator,
			Cap:           coin(100),
			FeePaid:       coin(0),
			TotalRecorded: coin(40),
			TotalClaimed:  coin(0),
			Refunded:      coin(0),
			Status:        settlementtypes.SettlementStatus_SETTLEMENT_STATUS_OPEN,
		},
		claimables: []settlementtypes.Claimable{
			{SettlementId: testOrderID(), AgentId: f.agentID, Amount: coin(40)},
		},
	}
}

// A claim signs with the operator's own key — claimer is the operator, the
// agent is this one — and reports what the books showed.
func TestClaimIsOperatorSignedAndGated(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.SettlementQ = stubOrder(f)

	t.Run("a stranger cannot claim", func(t *testing.T) {
		_, err := f.svc.Claim(context.Background(), ClaimInput{
			SettlementID: settlementtypes.SettlementIDToCaveat(testOrderID()),
		})
		if err == nil {
			t.Fatal("an unauthenticated claim must refuse")
		}
	})

	out, err := f.svc.Claim(operatorCtx(), ClaimInput{
		SettlementID: settlementtypes.SettlementIDToCaveat(testOrderID()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Claimable == nil || out.Claimable.Amount != "40" {
		t.Errorf("claimable = %+v", out.Claimable)
	}

	var claim settlementtypes.MsgClaim
	decodeSoleTxMsg(t, f.broadcast.txBytes, "/dydxprotocol.settlement.MsgClaim", &claim)
	if claim.Claimer != f.svc.cfg.Operator || claim.AgentId != f.agentID {
		t.Errorf("claim signer/agent = %s/%s", claim.Claimer, claim.AgentId)
	}
	if string(claim.SettlementId) != string(testOrderID()) {
		t.Errorf("claim order %X", claim.SettlementId)
	}
}

// The read view needs no credential and renders the order plus its claimables.
func TestGetSettlementRendersTheOrder(t *testing.T) {
	f := newFixture(t)

	t.Run("refuses without a querier", func(t *testing.T) {
		_, err := f.svc.GetSettlement(context.Background(), GetSettlementInput{
			SettlementID: settlementtypes.SettlementIDToCaveat(testOrderID()),
		})
		if err == nil || !strings.Contains(err.Error(), "no settlement query endpoint") {
			t.Errorf("want a configuration refusal, got %v", err)
		}
	})

	f.svc.cfg.SettlementQ = stubOrder(f)
	view, err := f.svc.GetSettlement(context.Background(), GetSettlementInput{
		SettlementID: settlementtypes.SettlementIDToCaveat(testOrderID()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Opener != testDelegator || view.Status != "SETTLEMENT_STATUS_OPEN" {
		t.Errorf("view = %+v", view)
	}
	if view.TotalRecorded.Amount != "40" || len(view.Claimables) != 1 || view.Claimables[0].AgentID != f.agentID {
		t.Errorf("books = %+v", view)
	}
}
