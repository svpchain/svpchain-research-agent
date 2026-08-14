package intermediary

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"

	"github.com/svpchain/svpdt"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
)

const (
	testChainID   = "svp-inter-1"
	testNow       = int64(1_800_000_000)
	testPrincipal = "svp199tqg4wdlnu4qjlxchpd7seg454937hjk505pe"
	testOrderHex  = "c001000000000000000000000000000000000000000000000000000000000000"
)

// party is one signing identity in the chain: the user, the intermediary, or
// the downstream executor.
type party struct {
	did    string
	signer *svpdt.PrivateKeySigner
	pubKey []byte
}

func newParty(t *testing.T, did string) party {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer, err := svpdt.NewPrivateKeySigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return party{did: did, signer: signer, pubKey: signer.PublicKey()}
}

// scenario is the three-party world: user → intermediary → downstream.
type scenario struct {
	user, inter, down party
	resolver          svpdt.Resolver
	sent              *captureSender
	svc               *Service
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	user := newParty(t, "did:svp:"+testPrincipal)
	inter := newParty(t, "did:svp:svp1intermediaryxxxxxxxxxxxxxxxxxxxxxx")
	down := newParty(t, "did:svp:svp1downstreamxxxxxxxxxxxxxxxxxxxxxxxx")

	resolver := svpdt.SingleKeyResolver(map[string][]byte{
		user.did:  user.pubKey,
		inter.did: inter.pubKey,
		down.did:  down.pubKey,
	})
	sender := &captureSender{}

	s := &scenario{user: user, inter: inter, down: down, resolver: resolver, sent: sender}
	s.svc = New(Config{
		AgentID:    inter.did,
		Signer:     inter.signer,
		Verify:     &fakeVerifier{resolver: resolver, audience: inter.did},
		Downstream: sender,
		Now:        func() int64 { return testNow },
	})
	return s
}

// fakeVerifier mirrors what delegated.Service.VerifyInbound does — the same
// svpdt.VerifyChain call with this agent as the audience — without needing a
// chain to read params and keys from.
type fakeVerifier struct {
	resolver svpdt.Resolver
	audience string
	// now overrides the fixed test clock, for the scenarios that must agree
	// with a real deployment's wall clock.
	now int64
}

var _ Verifier = (*fakeVerifier)(nil)

func (v *fakeVerifier) VerifyInbound(_ context.Context, proof []string) ([][]byte, *svpdt.Verified, error) {
	tokens := make([][]byte, len(proof))
	for i, p := range proof {
		raw, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, nil, err
		}
		tokens[i] = raw
	}
	now := v.now
	if now == 0 {
		now = testNow
	}
	verified, err := svpdt.VerifyChain(tokens, v.resolver, svpdt.VerifyOpts{
		ChainID:  testChainID,
		Now:      now,
		MaxDepth: 4,
		Audience: v.audience,
	})
	return tokens, verified, err
}

type captureSender struct {
	endpoint string
	envelope []byte
	tokens   []string
}

var _ Sender = (*captureSender)(nil)

func (c *captureSender) Send(_ context.Context, endpoint string, envelope []byte, tokens []string) (Reply, error) {
	c.endpoint, c.envelope, c.tokens = endpoint, envelope, tokens
	return Reply{TaskID: "task-1", State: "completed", Response: `{"ok":true}`}, nil
}

type captureRecorder struct {
	calls []delegated.ExecRecordSpendInput
	err   error
}

var _ Recorder = (*captureRecorder)(nil)

func (c *captureRecorder) ExecuteRecordSpend(_ context.Context, in delegated.ExecRecordSpendInput) (delegated.ExecResult, error) {
	c.calls = append(c.calls, in)
	if c.err != nil {
		return delegated.ExecResult{}, c.err
	}
	return delegated.ExecResult{TxHash: "REC1"}, nil
}

func coin(amount int64) svpdt.Coins {
	c, _ := svpdt.NewCoins(svpdt.Coin{Denom: "uusdc", Amount: big.NewInt(amount)})
	return c
}

// rootCredential is the user's depth-1 grant to the intermediary: re-delegable
// to the downstream agent only, bound to a settlement order.
func (s *scenario) rootCredential(t *testing.T, mutate func(*svpdt.Caveats)) []string {
	t.Helper()
	redelegateTo, err := svpdt.ConstrainedTo(s.down.did)
	if err != nil {
		t.Fatal(err)
	}
	caveats := svpdt.Caveats{
		Principal:    testPrincipal,
		Actions:      svpdt.StringSet{"clob.place_order", "settlement.record_spend"},
		Subaccounts:  svpdt.Uint32Set{0, 7},
		Budget:       coin(1_000_000),
		SvcBudget:    coin(500_000),
		Settlement:   testOrderHex,
		Redelegable:  true,
		RedelegateTo: redelegateTo,
		MaxDepth:     3,
		NotBefore:    testNow - 60,
		Expires:      testNow + 600,
	}
	if mutate != nil {
		mutate(&caveats)
	}
	_, encoded, err := svpdt.Issue(s.user.signer, svpdt.IssueParams{
		ChainID:   testChainID,
		Root:      [32]byte{0xAA},
		RootEpoch: 1,
		Issuer:    s.user.did,
		Audience:  s.inter.did,
		Caveats:   caveats,
		Nonce:     [16]byte{0x01},
	})
	if err != nil {
		t.Fatal(err)
	}
	return []string{base64.StdEncoding.EncodeToString(encoded)}
}

func (s *scenario) forward(t *testing.T, proof []string, in ForwardInput) (ForwardOutput, error) {
	t.Helper()
	in.Proof = proof
	if in.DownstreamDID == "" {
		in.DownstreamDID = s.down.did
	}
	if in.DownstreamEndpoint == "" {
		in.DownstreamEndpoint = "https://downstream.example.com"
	}
	if len(in.Envelope) == 0 {
		in.Envelope = []byte(`{"skill":"svpchain-execution","tool":"execute_place_order","args":{}}`)
	}
	return s.svc.Forward(context.Background(), in)
}

// ★ The multi-hop property, end to end: the chain the intermediary forwards
// verifies at the downstream agent — which never speaks to the intermediary —
// back to the user, and grants strictly less than the user gave.
func TestForwardProducesAChainTheDownstreamVerifies(t *testing.T) {
	s := newScenario(t)

	out, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{
		Narrow: NarrowSpec{
			Actions:     []string{"clob.place_order"},
			Subaccounts: []uint32{7},
			Budget:      &agentchain.Coin{Denom: "uusdc", Amount: "250000"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Depth != 2 || len(s.sent.tokens) != 2 {
		t.Fatalf("forwarded chain depth = %d / %d tokens", out.Depth, len(s.sent.tokens))
	}

	// The downstream's own verification: audience is the downstream agent,
	// and nothing about the intermediary is consulted.
	raw := make([][]byte, len(s.sent.tokens))
	for i, tok := range s.sent.tokens {
		b, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			t.Fatal(err)
		}
		raw[i] = b
	}
	verified, err := svpdt.VerifyChain(raw, s.resolver, svpdt.VerifyOpts{
		ChainID:  testChainID,
		Now:      testNow,
		MaxDepth: 4,
		Audience: s.down.did,
	})
	if err != nil {
		t.Fatalf("the downstream must be able to verify the forwarded chain: %v", err)
	}

	// The principal survives the hop — this is what keeps the position with
	// the user rather than with either agent.
	if verified.Principal != testPrincipal {
		t.Errorf("principal = %s, want the user %s", verified.Principal, testPrincipal)
	}
	if out.Principal != testPrincipal {
		t.Errorf("reported principal = %s", out.Principal)
	}
	// The grant narrowed, and the settlement binding rode along untouched: the
	// downstream is paid out of the same escrow the user opened.
	if verified.Effective.Actions.Has("settlement.record_spend") {
		t.Error("the narrowed child must not keep the recording action")
	}
	if verified.Effective.Subaccounts.Has(0) {
		t.Error("the narrowed child must not keep subaccount 0")
	}
	if got := verified.Effective.Budget[0].Amount.String(); got != "250000" {
		t.Errorf("child budget = %s, want the narrowed 250000", got)
	}
	if verified.Effective.Settlement != testOrderHex {
		t.Errorf("settlement binding = %q, want it inherited", verified.Effective.Settlement)
	}
	if out.ChildGrant.Redelegable {
		t.Error("the child must not itself be re-delegable — the user granted one hop")
	}
}

func TestForwardRefusals(t *testing.T) {
	t.Run("parent is not re-delegable", func(t *testing.T) {
		s := newScenario(t)
		proof := s.rootCredential(t, func(c *svpdt.Caveats) {
			c.Redelegable = false
			c.RedelegateTo = svpdt.OptionalStringSet{}
		})
		_, err := s.forward(t, proof, ForwardInput{})
		if err == nil || !strings.Contains(err.Error(), "not re-delegable") {
			t.Errorf("want a re-delegability refusal, got %v", err)
		}
		if s.sent.tokens != nil {
			t.Error("nothing may be forwarded")
		}
	})

	t.Run("target outside redelegate_to", func(t *testing.T) {
		s := newScenario(t)
		_, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{
			DownstreamDID: "did:svp:svp1strangerxxxxxxxxxxxxxxxxxxxxxxxxx",
		})
		if err == nil || !strings.Contains(err.Error(), "does not allow re-delegating") {
			t.Errorf("want a target refusal, got %v", err)
		}
		if s.sent.tokens != nil {
			t.Error("nothing may be forwarded")
		}
	})

	// ★ An intermediary cannot hand on more than it holds, even deliberately:
	// Attenuate re-runs monotonicity and refuses to sign a wider child.
	t.Run("widening is unsignable", func(t *testing.T) {
		s := newScenario(t)
		_, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{
			Narrow: NarrowSpec{
				Budget: &agentchain.Coin{Denom: "uusdc", Amount: "9000000"},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "attenuate") {
			t.Errorf("want an attenuation refusal, got %v", err)
		}
		if s.sent.tokens != nil {
			t.Error("nothing may be forwarded")
		}
	})

	t.Run("expired credential", func(t *testing.T) {
		s := newScenario(t)
		proof := s.rootCredential(t, func(c *svpdt.Caveats) {
			c.NotBefore = testNow - 600
			c.Expires = testNow - 60
		})
		if _, err := s.forward(t, proof, ForwardInput{}); err == nil {
			t.Error("an expired credential must be refused")
		}
	})
}

// The intermediary charges for its own work against the task's escrow, under
// the credential it received — and a fee the escrow refuses aborts the task
// before the downstream leg runs.
func TestForwardRecordsItsOwnFee(t *testing.T) {
	s := newScenario(t)
	rec := &captureRecorder{}
	s.svc.cfg.Spend = rec
	fee := agentchain.Coin{Denom: "uusdc", Amount: "40000"}

	out, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{Fee: &fee})
	if err != nil {
		t.Fatal(err)
	}
	if out.Recorded == nil || out.Recorded.Amount != "40000" {
		t.Fatalf("recorded = %+v", out.Recorded)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("record calls = %d", len(rec.calls))
	}
	// Recorded under the inbound credential, so the chain checks the same
	// settlement binding and service budget the user signed.
	if len(rec.calls[0].Proof) != 1 {
		t.Errorf("the fee must be recorded under the received credential, got %d tokens", len(rec.calls[0].Proof))
	}
	if rec.calls[0].Record.Amount.Amount != "40000" {
		t.Errorf("recorded amount = %+v", rec.calls[0].Record.Amount)
	}

	t.Run("a refused fee aborts before forwarding", func(t *testing.T) {
		s := newScenario(t)
		s.svc.cfg.Spend = &captureRecorder{err: errRefused}
		_, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{Fee: &fee})
		if err == nil || !strings.Contains(err.Error(), "record own fee") {
			t.Errorf("want the fee failure surfaced, got %v", err)
		}
		if s.sent.tokens != nil {
			t.Error("the downstream leg must not run when the fee was refused")
		}
	})

	t.Run("no recorder configured", func(t *testing.T) {
		s := newScenario(t)
		_, err := s.forward(t, s.rootCredential(t, nil), ForwardInput{Fee: &fee})
		if err == nil || !strings.Contains(err.Error(), "cannot record spend") {
			t.Errorf("want a configuration refusal, got %v", err)
		}
	})
}

var errRefused = &refusedError{}

type refusedError struct{}

func (*refusedError) Error() string { return "settlement refused the record" }
