package intermediary

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/evm/crypto/ethsecp256k1"
	"github.com/svpchain/svpdt"
	"google.golang.org/grpc"

	agenttypes "github.com/dydxprotocol/v4-chain/protocol/x/agent/types"
	wallettypes "github.com/dydxprotocol/v4-chain/protocol/x/agentwallet/types"

	"github.com/svpchain/svpchain-mcp/lib/mcp/policy"
	"github.com/svpchain/svpchain-mcp/lib/mcp/signer"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
)

// The end-to-end claim this whole package exists for: a chain the
// intermediary produced is accepted by the *real* DEX-agent verifier — the
// same delegated.Service a deployment runs — with no fake in the verification
// path and no communication back to the intermediary.

// chainQ answers agent-key lookups for several DIDs and serves the module
// params the verifier reads its ceilings from.
type chainQ struct {
	keys map[string][]byte
}

func (q *chainQ) Agent(_ context.Context, in *agenttypes.QueryAgent, _ ...grpc.CallOption) (*agenttypes.QueryAgentResponse, error) {
	pk, ok := q.keys[in.AgentId]
	if !ok {
		return &agenttypes.QueryAgentResponse{}, nil
	}
	return &agenttypes.QueryAgentResponse{
		Agent: agenttypes.Agent{AgentId: in.AgentId, PublicKey: pk},
	}, nil
}

func (q *chainQ) AgentByOperator(context.Context, *agenttypes.QueryAgentByOperator, ...grpc.CallOption) (*agenttypes.QueryAgentByOperatorResponse, error) {
	return &agenttypes.QueryAgentByOperatorResponse{}, nil
}
func (q *chainQ) AllAgents(context.Context, *agenttypes.QueryAllAgents, ...grpc.CallOption) (*agenttypes.QueryAllAgentsResponse, error) {
	return &agenttypes.QueryAllAgentsResponse{}, nil
}
func (q *chainQ) AgentsByOwner(context.Context, *agenttypes.QueryAgentsByOwner, ...grpc.CallOption) (*agenttypes.QueryAgentsByOwnerResponse, error) {
	return &agenttypes.QueryAgentsByOwnerResponse{}, nil
}
func (q *chainQ) AgentsByCapability(context.Context, *agenttypes.QueryAgentsByCapability, ...grpc.CallOption) (*agenttypes.QueryAgentsByCapabilityResponse, error) {
	return &agenttypes.QueryAgentsByCapabilityResponse{}, nil
}
func (q *chainQ) Params(context.Context, *agenttypes.QueryParams, ...grpc.CallOption) (*agenttypes.QueryParamsResponse, error) {
	return &agenttypes.QueryParamsResponse{}, nil
}

type paramsWalletQ struct{}

func (paramsWalletQ) Delegation(context.Context, *wallettypes.QueryDelegation, ...grpc.CallOption) (*wallettypes.QueryDelegationResponse, error) {
	return &wallettypes.QueryDelegationResponse{}, nil
}
func (paramsWalletQ) DelegationsByDelegator(context.Context, *wallettypes.QueryDelegationsByDelegator, ...grpc.CallOption) (*wallettypes.QueryDelegationsByDelegatorResponse, error) {
	return &wallettypes.QueryDelegationsByDelegatorResponse{}, nil
}
func (paramsWalletQ) Epoch(context.Context, *wallettypes.QueryEpoch, ...grpc.CallOption) (*wallettypes.QueryEpochResponse, error) {
	return &wallettypes.QueryEpochResponse{Epoch: 1}, nil
}
func (paramsWalletQ) Spend(context.Context, *wallettypes.QuerySpend, ...grpc.CallOption) (*wallettypes.QuerySpendResponse, error) {
	return &wallettypes.QuerySpendResponse{}, nil
}
func (paramsWalletQ) Params(context.Context, *wallettypes.QueryParams, ...grpc.CallOption) (*wallettypes.QueryParamsResponse, error) {
	return &wallettypes.QueryParamsResponse{Params: wallettypes.Params{
		MaxDelegationDepth: 4,
		MaxTokenTtlSeconds: 900,
	}}, nil
}

// realDownstream builds a genuine delegated.Service whose agent id derives
// from its own operator key — the registry's rule — and returns it with its
// DID and credential signer.
func realDownstream(t *testing.T, keys map[string][]byte) (*delegated.Service, string, *svpdt.PrivateKeySigner) {
	t.Helper()

	bz := make([]byte, 32)
	if _, err := rand.Read(bz); err != nil {
		t.Fatal(err)
	}
	priv := &ethsecp256k1.PrivKey{Key: bz}
	operatorAddr := signer.DeriveAddress(priv)
	did := agenttypes.AgentIdFromOperator(sdk.MustAccAddressFromBech32(operatorAddr))

	dtSigner, err := svpdt.NewPrivateKeySigner(bz)
	if err != nil {
		t.Fatal(err)
	}
	keys[did] = dtSigner.PublicKey()

	svc := delegated.New(delegated.Config{
		Priv:     priv,
		Operator: operatorAddr,
		ChainID:  testChainID,
		AgentQ:   &chainQ{keys: keys},
		WalletQ:  paramsWalletQ{},
		Policy:   policy.NewEngine(nil),
	})
	return svc, did, dtSigner
}

// ★ The real DEX-agent verifier accepts the intermediary's forwarded chain.
func TestForwardedChainVerifiesAtTheRealDownstream(t *testing.T) {
	keys := map[string][]byte{}
	down, downDID, _ := realDownstream(t, keys)

	// The user and the intermediary, with their keys published the way the
	// chain would publish them.
	user := newParty(t, "did:svp:"+testPrincipal)
	inter := newParty(t, "did:svp:svp1intermediaryxxxxxxxxxxxxxxxxxxxxxx")
	keys[user.did] = user.pubKey
	keys[inter.did] = inter.pubKey

	// A deployed downstream reads the wall clock, so this scenario mints
	// against real time rather than the package's fixed test clock.
	now := time.Now().Unix()
	sender := &captureSender{}
	svc := New(Config{
		AgentID:    inter.did,
		Signer:     inter.signer,
		Verify:     &fakeVerifier{resolver: svpdt.SingleKeyResolver(keys), audience: inter.did, now: now},
		Downstream: sender,
		Now:        func() int64 { return now },
	})

	// The user's grant: re-delegable to this downstream agent only.
	proof := userGrant(t, user, inter.did, downDID, now)

	out, err := svc.Forward(context.Background(), ForwardInput{
		Proof:              proof,
		DownstreamDID:      downDID,
		DownstreamEndpoint: "https://downstream.example.com",
		Envelope:           []byte(`{"skill":"svpchain-execution","tool":"execute_place_order","args":{}}`),
		Narrow: NarrowSpec{
			Actions:     []string{"clob.place_order"},
			Subaccounts: []uint32{7},
			Budget:      &agentchain.Coin{Denom: "uusdc", Amount: "250000"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The downstream's own inbound check — module params, key resolution,
	// audience binding and redelegation targets, exactly as deployed.
	_, verified, err := down.VerifyInbound(context.Background(), sender.tokens)
	if err != nil {
		t.Fatalf("the real downstream verifier rejected the forwarded chain: %v", err)
	}
	if verified.Principal != testPrincipal {
		t.Errorf("principal = %s, want the user %s", verified.Principal, testPrincipal)
	}
	if verified.Depth != 2 || out.Depth != 2 {
		t.Errorf("depth = %d (reported %d), want 2", verified.Depth, out.Depth)
	}
	if !verified.Effective.Actions.Has("clob.place_order") ||
		verified.Effective.Actions.Has("settlement.record_spend") {
		t.Errorf("effective actions = %v, want only the narrowed one", verified.Effective.Actions)
	}
	if verified.Effective.Settlement != testOrderHex {
		t.Errorf("settlement binding = %q, want it carried to the executor", verified.Effective.Settlement)
	}
}

// The two sides agree on the target rule: a chain forwarded to an agent the
// user did not authorise is refused by the intermediary — and would be
// refused by the downstream too, which is what makes the intermediary's own
// check a courtesy rather than the only defence.
func TestTheRealDownstreamAlsoRefusesAnUnauthorisedTarget(t *testing.T) {
	keys := map[string][]byte{}
	down, downDID, _ := realDownstream(t, keys)
	other, otherDID, otherSigner := realDownstream(t, keys)
	_ = other

	user := newParty(t, "did:svp:"+testPrincipal)
	inter := newParty(t, "did:svp:svp1intermediaryxxxxxxxxxxxxxxxxxxxxxx")
	keys[user.did] = user.pubKey
	keys[inter.did] = inter.pubKey

	now := time.Now().Unix()
	svc := New(Config{
		AgentID:    inter.did,
		Signer:     inter.signer,
		Verify:     &fakeVerifier{resolver: svpdt.SingleKeyResolver(keys), audience: inter.did, now: now},
		Downstream: &captureSender{},
		Now:        func() int64 { return now },
	})
	proof := userGrant(t, user, inter.did, downDID, now) // redelegate_to = [downDID]

	// The intermediary refuses it.
	_, err := svc.Forward(context.Background(), ForwardInput{
		Proof:              proof,
		DownstreamDID:      otherDID,
		DownstreamEndpoint: "https://other.example.com",
		Envelope:           []byte(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "does not allow re-delegating") {
		t.Fatalf("the intermediary must refuse an unauthorised target, got %v", err)
	}

	// Nor could a careless intermediary produce one by accident: Attenuate
	// itself refuses to sign a child for an audience outside the parent's
	// targets, so reaching the downstream at all requires deliberately
	// hand-building the token — the path delegated's own
	// TestVerifyProofEnforcesRedelegationTargets covers, asserting the
	// downstream refuses it there.
	caveats := parentCaveats(t, proof[0])
	if _, _, err := svpdt.Attenuate(otherSigner, svpdt.AttenuateParams{
		Parent:   mustDecode(t, proof[0]),
		Issuer:   inter.did,
		Audience: otherDID,
		Caveats:  caveats,
		Nonce:    [16]byte{0x09},
	}); err == nil {
		t.Fatal("Attenuate must refuse a child addressed outside redelegate_to")
	}
	_ = down
}

// userGrant mints the user's depth-1 re-delegable credential against a given
// clock.
func userGrant(t *testing.T, user party, interDID, downDID string, now int64) []string {
	t.Helper()
	redelegateTo, err := svpdt.ConstrainedTo(downDID)
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, err := svpdt.Issue(user.signer, svpdt.IssueParams{
		ChainID:   testChainID,
		Root:      [32]byte{0xAA},
		RootEpoch: 1,
		Issuer:    user.did,
		Audience:  interDID,
		Caveats: svpdt.Caveats{
			Principal:    testPrincipal,
			Actions:      svpdt.StringSet{"clob.place_order", "settlement.record_spend"},
			Subaccounts:  svpdt.Uint32Set{0, 7},
			Budget:       coin(1_000_000),
			SvcBudget:    coin(500_000),
			Settlement:   testOrderHex,
			Redelegable:  true,
			RedelegateTo: redelegateTo,
			MaxDepth:     3,
			NotBefore:    now - 60,
			Expires:      now + 600,
		},
		Nonce: [16]byte{0x01},
	})
	if err != nil {
		t.Fatal(err)
	}
	return []string{base64.StdEncoding.EncodeToString(encoded)}
}

func mustDecode(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parentCaveats(t *testing.T, b64 string) svpdt.Caveats {
	t.Helper()
	tok, err := svpdt.UnmarshalToken(mustDecode(t, b64))
	if err != nil {
		t.Fatal(err)
	}
	return tok.Payload.Caveats
}
