package wire

import (
	"testing"

	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
	"github.com/svpchain/svpchain-research-agent/internal/toolbridge"
)

// ★ This binary registers nothing, and that is the whole point.
//
// The intermediary does not dispatch a tool registry: it receives a re-delegable
// SVP-DT credential, narrows it, and hands the execution leg to a downstream
// agent that executes on the original user's account. Registering any operation
// family here would mean this hop could act on the credential itself, which is
// exactly the property the multi-hop design exists to avoid.
//
// It used to compare the union of four profiles against the full registry, back
// when core was shared. With only the delegation profile left, the assertion
// that matters is the empty one.
func TestDelegationProfileRegistersNothing(t *testing.T) {
	h := &tools.Handlers{}
	agentSvc := agentchain.New(nil, nil, nil, nil, nil, nil, nil)

	r := toolbridge.NewEmpty()
	DelegationProfile.Register(r, h, agentSvc, nil)

	if got := r.BySkill(); len(got) != 0 {
		t.Errorf("the delegation profile registered %v; it must serve no operation family", got)
	}
}
