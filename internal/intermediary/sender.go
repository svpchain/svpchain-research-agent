package intermediary

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// DelegationMetadataKey is the message-metadata key the SVP delegation
// extension defines for an SVP-DT credential chain. Must stay byte-identical
// to the key the receiving side reads.
const DelegationMetadataKey = "svp.delegation/v1"

// A2ASender forwards a task over A2A, carrying the credential chain in the
// message metadata — the same carrier the delegation extension advertises, so
// a downstream agent verifies it before parsing the request.
type A2ASender struct{}

var _ Sender = A2ASender{}

func (A2ASender) Send(
	ctx context.Context, endpoint string, envelope []byte, tokens []string,
) (Reply, error) {
	card, err := agentcard.DefaultResolver.Resolve(ctx, endpoint)
	if err != nil {
		return Reply{}, fmt.Errorf("resolve agent card: %w", err)
	}
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		return Reply{}, fmt.Errorf("create a2a client: %w", err)
	}

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(string(envelope)))
	msg.Metadata = map[string]any{
		DelegationMetadataKey: map[string]any{"tokens": tokens},
	}

	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		return Reply{}, fmt.Errorf("send message: %w", err)
	}

	out := Reply{Response: resultText(result)}
	switch v := result.(type) {
	case *a2a.Task:
		out.TaskID = string(v.ID)
		out.State = string(v.Status.State)
	case *a2a.Message:
		out.TaskID = string(v.TaskID)
	}
	return out, nil
}

func resultText(result a2a.SendMessageResult) string {
	switch v := result.(type) {
	case *a2a.Message:
		return messageText(v)
	case *a2a.Task:
		if v.Status.Message != nil {
			if text := messageText(v.Status.Message); text != "" {
				return text
			}
		}
		for i := len(v.History) - 1; i >= 0; i-- {
			if m := v.History[i]; m != nil && m.Role == a2a.MessageRoleAgent {
				if text := messageText(m); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func messageText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	var out string
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		out += part.Text()
	}
	return out
}
