package intermediary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
)

// The A2A face of the example agent. Deliberately minimal: one skill, one
// tool, and a card declaring the delegation extension — enough to be a real
// participant in the network, not enough to be mistaken for a product.

const (
	AgentName    = "svpchain-research-agent"
	AgentVersion = "0.1.0"

	SkillResearch = "svpchain-research"
	ToolResearch  = "research_and_execute"

	DelegationExtensionURI = "https://svpchain.io/a2a/ext/delegation/v1"
)

// Request is the task body: what to research, and the execution leg to hand
// downstream once this agent has done its part.
type Request struct {
	Skill string `json:"skill"`
	Tool  string `json:"tool"`
	Args  struct {
		// Topic is this agent's own (illustrative) work.
		Topic string `json:"topic"`

		// Downstream names the next hop. A production agent would resolve
		// the endpoint from the chain registry by DID; the example takes
		// both so a demo can point at a local instance.
		DownstreamDID      string `json:"downstream_did"`
		DownstreamEndpoint string `json:"downstream_endpoint"`

		// Envelope is the downstream request, verbatim.
		Envelope json.RawMessage `json:"envelope"`

		Narrow NarrowSpec `json:"narrow,omitempty"`
		Fee    *coinArg   `json:"fee,omitempty"`
	} `json:"args"`
}

type coinArg struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

// Response is the agent's answer: its own finding plus the forwarded hop.
type Response struct {
	Skill   string         `json:"skill"`
	Tool    string         `json:"tool"`
	OK      bool           `json:"ok"`
	Finding string         `json:"finding,omitempty"`
	Forward *ForwardOutput `json:"forward,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Executor answers A2A tasks by running the intermediary role.
type Executor struct {
	svc *Service
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

func NewExecutor(svc *Service) *Executor { return &Executor{svc: svc} }

func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.Message == nil {
			yield(nil, fmt.Errorf("empty message"))
			return
		}
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		result, err := e.handle(ctx, execCtx)
		if err != nil {
			// A refusal is an answer, not a transport failure — the caller
			// learns why its credential or request was not usable.
			result = fmt.Sprintf("error: %v", err)
		}
		reply := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(result))
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, reply), nil)
	}
}

func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *Executor) handle(ctx context.Context, execCtx *a2asrv.ExecutorContext) (string, error) {
	var req Request
	if err := json.Unmarshal([]byte(messageText(execCtx.Message)), &req); err != nil {
		return "", fmt.Errorf("request must be JSON naming a skill: %w", err)
	}
	if req.Skill != SkillResearch || req.Tool != ToolResearch {
		return "", fmt.Errorf("this agent serves %s / %s only", SkillResearch, ToolResearch)
	}

	// The credential rides the message metadata, as the card's extension
	// declares. Absent means the task cannot be delegated onward, which is
	// the only thing this agent does — refuse before working.
	tokens, err := delegationTokens(execCtx.Message)
	if err != nil {
		return "", err
	}

	in := ForwardInput{
		Proof:              tokens,
		DownstreamDID:      req.Args.DownstreamDID,
		DownstreamEndpoint: req.Args.DownstreamEndpoint,
		Envelope:           req.Args.Envelope,
		Narrow:             req.Args.Narrow,
	}
	if f := req.Args.Fee; f != nil {
		in.Fee = &agentchain.Coin{Denom: f.Denom, Amount: f.Amount}
	}

	out, err := e.svc.Forward(ctx, in)
	if err != nil {
		return "", err
	}

	resp := Response{
		Skill:   req.Skill,
		Tool:    req.Tool,
		OK:      true,
		Finding: finding(req.Args.Topic),
		Forward: &out,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(b), nil
}

// finding stands in for the agent's own work. An example agent earns its keep
// by demonstrating the delegation hop, not by having opinions about markets.
func finding(topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return "no topic given; forwarded the execution leg unchanged"
	}
	return "reviewed " + topic + "; forwarded the execution leg under a narrowed credential"
}

// delegationTokens reads the credential chain from the message metadata.
func delegationTokens(msg *a2a.Message) ([]string, error) {
	if msg == nil || msg.Metadata == nil {
		return nil, fmt.Errorf("no delegation credential: attach the chain as message.metadata[%q]", DelegationMetadataKey)
	}
	raw, ok := msg.Metadata[DelegationMetadataKey]
	if !ok {
		return nil, fmt.Errorf("no delegation credential: attach the chain as message.metadata[%q]", DelegationMetadataKey)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("metadata %q is not encodable: %w", DelegationMetadataKey, err)
	}
	var m struct {
		Tokens []string `json:"tokens"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("metadata %q is malformed: %w", DelegationMetadataKey, err)
	}
	if len(m.Tokens) == 0 {
		return nil, fmt.Errorf("metadata %q carries no tokens", DelegationMetadataKey)
	}
	return m.Tokens, nil
}

// BuildAgentCard returns the agent's public card.
func BuildAgentCard(publicURL, agentID string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:    AgentName,
		Version: AgentVersion,
		Description: "Example intermediary agent: accepts a task under a re-delegable SVP-DT " +
			"credential, does its own work, then narrows the credential and hands the " +
			"execution leg to a downstream agent. Holds no user keys and takes no " +
			"custody; the position lands on the original user's account.",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(publicURL+"/invoke", a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"application/json", "text/plain"},
		DefaultOutputModes: []string{"application/json"},
		Capabilities: a2a.AgentCapabilities{
			Streaming: true,
			Extensions: []a2a.AgentExtension{{
				URI:      DelegationExtensionURI,
				Required: true,
				Description: "Requires a re-delegable SVP-DT credential naming this agent as " +
					"audience and the intended executor in redelegate_to, carried as " +
					"message.metadata[\"" + DelegationMetadataKey + "\"].",
				Params: map[string]any{"metadataKey": DelegationMetadataKey, "agentId": agentID},
			}},
		},
		Skills: []a2a.AgentSkill{{
			ID:   SkillResearch,
			Name: "Research and delegated execution",
			Description: "Reviews a topic and forwards the execution leg to a downstream agent " +
				"under a narrowed credential. Tools: " + ToolResearch + ".",
			Tags: []string{"research", "delegation", "svp-dt", "multi-hop"},
			Examples: []string{
				`message.metadata: {"svp.delegation/v1":{"tokens":["<base64 token>"]}} · ` +
					`text: {"skill":"svpchain-research","tool":"research_and_execute","args":{"topic":"BTC","downstream_did":"did:svp:…","downstream_endpoint":"http://localhost:8082","envelope":{"skill":"svpchain-execution","tool":"execute_place_order","args":{"order":{}}}}}`,
			},
		}},
	}
}

// Serve runs the agent's HTTP surface until ctx is cancelled.
func Serve(ctx context.Context, listenAddr, publicURL, agentID string, svc *Service) error {
	handler := a2asrv.NewHandler(NewExecutor(svc))
	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(BuildAgentCard(publicURL, agentID)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
