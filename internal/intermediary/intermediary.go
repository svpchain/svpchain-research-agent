// Package intermediary is a thin example agent that plays the middle role in a
// multi-hop delegation: it accepts a task under a re-delegable SVP-DT
// credential, does its own (illustrative) work, then narrows that credential
// and hands the execution leg to a downstream agent over A2A.
//
// It exists to exercise the one part of the delegation design that no other
// component does. The user delegates to this agent; this agent re-delegates to
// the DEX agent; the DEX agent executes on the *user's* account. Neither hop
// can widen what it received, the downstream agent verifies the whole chain
// without ever asking this one, and the user's emergency stop kills every hop
// at once.
//
// Deliberately thin. It reuses the DEX agent's packages for everything that is
// not the intermediary role itself — credential verification, resolution,
// settlement recording — so that what remains here is only: verify inbound,
// attenuate, forward, record.
package intermediary

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/svpchain/svpdt"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
)

// Config wires a Service.
type Config struct {
	// AgentID is this agent's DID — the audience inbound credentials must
	// name, and the issuer of the children it mints.
	AgentID string

	// Signer signs the child credentials. It must hold the key behind the
	// operator address AgentID derives from: the chain resolves an agent's
	// credential key from its registration, so a child signed with anything
	// else verifies nowhere.
	Signer svpdt.Signer

	// Verify is the inbound half, reused wholesale from the DEX agent: chain
	// signatures, linkage, monotonicity, expiry, depth, audience, and the
	// redelegate_to targets — with the ceilings read from the chain's own
	// agentwallet params. An interface so a test can drive the role without a
	// chain, and so this package depends on the behaviour rather than the
	// deployment.
	Verify Verifier

	// Downstream sends the forwarded task. An interface so a test drives it
	// without a network, and so the transport can change without touching the
	// delegation logic.
	Downstream Sender

	// Spend records this agent's own fee against the task's settlement escrow.
	// Nil disables charging: an intermediary may work for free, and a demo
	// deployment usually does.
	Spend Recorder

	// Now is the clock, injectable for tests.
	Now func() int64
}

// Verifier checks an inbound credential chain, returning the exact token
// bytes (a child commits to its parent's bytes, so a re-encoding will not do)
// and what the chain grants. delegated.Service satisfies it.
type Verifier interface {
	VerifyInbound(ctx context.Context, proof []string) ([][]byte, *svpdt.Verified, error)
}

// Sender delivers a task with a credential chain to a downstream agent.
type Sender interface {
	Send(ctx context.Context, endpoint string, envelope []byte, tokens []string) (Reply, error)
}

// Reply is what a downstream agent answered.
type Reply struct {
	TaskID   string `json:"task_id,omitempty"`
	State    string `json:"state,omitempty"`
	Response string `json:"response"`
}

// Recorder records a spend against a settlement order. Method name and
// signature match delegated.Service, so the DEX agent's service satisfies it
// directly rather than through an adapter.
type Recorder interface {
	ExecuteRecordSpend(ctx context.Context, in delegated.ExecRecordSpendInput) (delegated.ExecResult, error)
}

// Service is the intermediary.
type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = nowUnix
	}
	return &Service{cfg: cfg}
}

// ForwardInput is one hop: the credential this agent received, the downstream
// agent to pass a narrowed copy to, and the task to run there.
type ForwardInput struct {
	// Proof is the inbound credential chain, base64, root-issued first.
	Proof []string `json:"proof"`

	// Downstream identifies the next hop. The DID must be one the inbound
	// credential's redelegate_to allows; the endpoint is where its A2A
	// service lives (a real deployment reads both from the chain registry).
	DownstreamDID      string `json:"downstream_did"`
	DownstreamEndpoint string `json:"downstream_endpoint"`

	// Envelope is the request body for the downstream agent, verbatim — the
	// {skill, tool, args} JSON its A2A surface expects. This agent does not
	// interpret it: what the downstream may do is bounded by the credential
	// it is handed, not by this agent's reading of the request.
	Envelope []byte `json:"envelope"`

	// Narrow optionally tightens the child grant beyond the inherited
	// defaults. Anything left empty keeps the parent's value; anything set
	// must still be a subset, or Attenuate refuses to sign.
	Narrow NarrowSpec `json:"narrow,omitempty"`

	// Fee, when set with a Recorder, is what this agent charges for its own
	// work — recorded against the task's settlement order before the
	// downstream leg runs.
	Fee *agentchain.Coin `json:"fee,omitempty"`
}

// NarrowSpec is the child's requested grant, as a narrowing of the parent's.
type NarrowSpec struct {
	Actions     []string         `json:"actions,omitempty"`
	Subaccounts []uint32         `json:"subaccounts,omitempty"`
	Budget      *agentchain.Coin `json:"budget,omitempty"`
	SvcBudget   *agentchain.Coin `json:"svc_budget,omitempty"`
	// TTLSeconds shortens the child's life from now. Zero inherits the
	// parent's expiry, which is already the ceiling either way.
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// ForwardOutput reports the hop.
type ForwardOutput struct {
	// Principal is whose account the whole chain acts on — unchanged by
	// re-delegation, which is the property that keeps the position with the
	// user rather than with either agent.
	Principal string `json:"principal"`

	// Depth is the forwarded chain's length: the inbound chain plus this hop.
	Depth int `json:"depth"`

	// ChildGrant is what the downstream agent actually received, for the
	// caller's audit and for the demo's narrative.
	ChildGrant Grant `json:"child_grant"`

	// Recorded is this agent's own fee, when it charged one.
	Recorded *agentchain.Coin `json:"recorded,omitempty"`

	// Reply is the downstream agent's answer, passed through untouched.
	Reply Reply `json:"reply"`
}

// Grant is the JSON-friendly view of a credential's effective capability.
type Grant struct {
	Actions     []string          `json:"actions"`
	Subaccounts []uint32          `json:"subaccounts"`
	Budget      []agentchain.Coin `json:"budget,omitempty"`
	SvcBudget   []agentchain.Coin `json:"svc_budget,omitempty"`
	Settlement  string            `json:"settlement,omitempty"`
	Expires     int64             `json:"expires"`
	Redelegable bool              `json:"redelegable"`
}

// Forward is the intermediary role, end to end.
func (s *Service) Forward(ctx context.Context, in ForwardInput) (ForwardOutput, error) {
	if strings.TrimSpace(in.DownstreamDID) == "" || strings.TrimSpace(in.DownstreamEndpoint) == "" {
		return ForwardOutput{}, fmt.Errorf("downstream_did and downstream_endpoint are required")
	}
	if len(in.Envelope) == 0 {
		return ForwardOutput{}, fmt.Errorf("envelope is required")
	}

	// 1. Verify the inbound chain — the same check the final executor will
	// run, so an unusable credential fails here rather than one hop later.
	tokens, verified, err := s.cfg.Verify.VerifyInbound(ctx, in.Proof)
	if err != nil {
		return ForwardOutput{}, err
	}

	// 2. Re-delegability is the parent's decision, and the target list is
	// enforced here because the parent's issuer is who wrote it. Checking
	// before minting turns "the downstream will reject this" into a refusal
	// that names the reason.
	leaf := verified.Tokens[len(verified.Tokens)-1]
	if !leaf.Payload.Caveats.Redelegable {
		return ForwardOutput{}, fmt.Errorf(
			"the credential is not re-delegable: its issuer did not authorise passing it on")
	}
	if !leaf.Payload.Caveats.RedelegateTo.Allows(in.DownstreamDID) {
		return ForwardOutput{}, fmt.Errorf(
			"the credential does not allow re-delegating to %s", in.DownstreamDID)
	}

	// 3. Charge for this agent's own work first, against the task's escrow.
	// Before the downstream leg deliberately: a fee the escrow cannot cover
	// should abort the task, not leave a half-executed one behind.
	var recorded *agentchain.Coin
	if in.Fee != nil {
		if s.cfg.Spend == nil {
			return ForwardOutput{}, fmt.Errorf("this deployment cannot record spend: no settlement recorder configured")
		}
		if _, err := s.cfg.Spend.ExecuteRecordSpend(ctx, delegated.ExecRecordSpendInput{
			Proof:  in.Proof,
			Record: delegated.RecordSpendParams{Amount: *in.Fee},
		}); err != nil {
			return ForwardOutput{}, fmt.Errorf("record own fee: %w", err)
		}
		fee := *in.Fee
		recorded = &fee
	}

	// 4. Mint the child. Attenuate re-runs monotonicity against the parent
	// and refuses to sign anything wider — this agent cannot hand on more
	// than it holds even if it tries.
	childCaveats, err := s.narrow(leaf.Payload.Caveats, in.Narrow)
	if err != nil {
		return ForwardOutput{}, err
	}
	parent := tokens[len(tokens)-1]
	nonce, err := drawNonce()
	if err != nil {
		return ForwardOutput{}, err
	}
	child, encoded, err := svpdt.Attenuate(s.cfg.Signer, svpdt.AttenuateParams{
		Parent:   parent,
		Issuer:   s.cfg.AgentID,
		Audience: strings.TrimSpace(in.DownstreamDID),
		Caveats:  childCaveats,
		Nonce:    nonce,
	})
	if err != nil {
		return ForwardOutput{}, fmt.Errorf("attenuate for %s: %w", in.DownstreamDID, err)
	}

	// 5. Forward: the whole chain travels, root-issued first, so the
	// downstream agent can verify back to the user without asking us.
	forwarded := make([]string, 0, len(in.Proof)+1)
	forwarded = append(forwarded, in.Proof...)
	forwarded = append(forwarded, base64.StdEncoding.EncodeToString(encoded))

	reply, err := s.cfg.Downstream.Send(ctx, in.DownstreamEndpoint, in.Envelope, forwarded)
	if err != nil {
		return ForwardOutput{}, fmt.Errorf("forward to %s: %w", in.DownstreamDID, err)
	}

	return ForwardOutput{
		Principal:  verified.Principal,
		Depth:      len(forwarded),
		ChildGrant: grantView(child.Payload.Caveats),
		Recorded:   recorded,
		Reply:      reply,
	}, nil
}

// narrow builds the child's caveats: the parent's, with the requested fields
// tightened. Everything not named is inherited — including the principal and
// the settlement binding, which must never change down the chain.
func (s *Service) narrow(parent svpdt.Caveats, spec NarrowSpec) (svpdt.Caveats, error) {
	child := parent

	// A child never re-delegates further. The one hop of headroom the user
	// granted is spent here; passing on the right to pass on would widen the
	// blast radius the user approved.
	//
	// The target set is explicitly constrained-to-nothing, never the zero
	// OptionalStringSet: that value is the *unconstrained* form, which reads
	// like a deny-default and behaves as "anyone". Attenuate refuses it
	// outright as a widening, which is the library catching exactly the
	// mistake this comment exists to prevent.
	empty, err := svpdt.ConstrainedTo()
	if err != nil {
		return svpdt.Caveats{}, fmt.Errorf("empty redelegate_to: %w", err)
	}
	child.Redelegable = false
	child.RedelegateTo = empty

	if len(spec.Actions) > 0 {
		actions, err := svpdt.NewStringSet(spec.Actions...)
		if err != nil {
			return svpdt.Caveats{}, fmt.Errorf("narrow actions: %w", err)
		}
		child.Actions = actions
	}
	if len(spec.Subaccounts) > 0 {
		child.Subaccounts = svpdt.NewUint32Set(spec.Subaccounts...)
	}
	if spec.Budget != nil {
		coins, err := toDTCoins(*spec.Budget)
		if err != nil {
			return svpdt.Caveats{}, fmt.Errorf("narrow budget: %w", err)
		}
		child.Budget = coins
	}
	if spec.SvcBudget != nil {
		coins, err := toDTCoins(*spec.SvcBudget)
		if err != nil {
			return svpdt.Caveats{}, fmt.Errorf("narrow svc_budget: %w", err)
		}
		child.SvcBudget = coins
	}
	if spec.TTLSeconds > 0 {
		if exp := s.cfg.Now() + spec.TTLSeconds; exp < child.Expires {
			child.Expires = exp
		}
	}
	return child, nil
}

func grantView(c svpdt.Caveats) Grant {
	return Grant{
		Actions:     c.Actions,
		Subaccounts: c.Subaccounts,
		Budget:      coinsView(c.Budget),
		SvcBudget:   coinsView(c.SvcBudget),
		Settlement:  c.Settlement,
		Expires:     c.Expires,
		Redelegable: c.Redelegable,
	}
}
