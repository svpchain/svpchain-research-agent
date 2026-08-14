package toolbridge

import (
	"github.com/svpchain/svpchain-research-agent/internal/delegated"
)

// executionPerpsTools are the perps-domain delegated writes — only a binary
// that serves the perps CLOB registers these.
var executionPerpsTools = []string{
	"execute_place_order",
	"execute_cancel_order",
	"execute_batch_cancel",
	"execute_deposit_to_subaccount",
}

// executionCoreTools are the domain-agnostic execution operations — identity,
// self-registration, and settlement — that every delegation-capable binary
// serves regardless of which domain it executes in.
var executionCoreTools = []string{
	"execute_record_spend",
	"get_settlement",
	"agent_claim",
	"agent_identity",
	"agent_self_register",
	"agent_self_update",
}

// executionTools is the execution skill's full operation set, shared by both
// the live and the refusing registration so the advertised surface is
// identical either way.
var executionTools = append(append([]string{}, executionPerpsTools...), executionCoreTools...)

// keylessReason is the refusal for execution tools on a deployment with no
// operator key; the skill is still advertised so callers learn the requirement
// instead of meeting an unknown-tool error.
const keylessReason = "delegated execution requires this deployment to configure an operator key " +
	"([operator] section); the skill is advertised so callers know it exists, but this " +
	"deployment cannot serve it"

// RegisterExecutionCore adds the domain-agnostic execution operations. A nil
// service (no operator key configured) registers the same tool names as
// refusals.
func (r *Registry) RegisterExecutionCore(s *delegated.Service) {
	if s == nil {
		for _, tool := range executionCoreTools {
			r.addRefusing(SkillExecution, tool, keylessReason)
		}
		return
	}
	// execute_record_spend nests its parameters under "record"; flat args
	// silently decoding to zero values would record a zero spend.
	r.add(SkillExecution, "execute_record_spend", adaptStrictNative(s.ExecuteRecordSpend))
	r.add(SkillExecution, "get_settlement", adaptNative(s.GetSettlement))
	r.add(SkillExecution, "agent_claim", adaptNative(s.Claim))
	r.add(SkillExecution, "agent_identity", adaptNative(s.Identity))
	r.add(SkillExecution, "agent_self_register", adaptNative(s.SelfRegister))
	r.add(SkillExecution, "agent_self_update", adaptNative(s.SelfUpdate))
}

// RegisterExecutionPerps adds the perps-domain delegated writes. A nil service
// registers the same tool names as refusals.
func (r *Registry) RegisterExecutionPerps(s *delegated.Service) {
	if s == nil {
		for _, tool := range executionPerpsTools {
			r.addRefusing(SkillExecution, tool, keylessReason)
		}
		return
	}
	// Strict decoding on the execute inputs: their parameters nest under a
	// wrapper key ("order", "cancel", "deposit"), and flat args silently
	// decoding to zero values would target subaccount 0.
	r.add(SkillExecution, "execute_place_order", adaptStrictNative(s.ExecutePlaceOrder))
	r.add(SkillExecution, "execute_cancel_order", adaptStrictNative(s.ExecuteCancelOrder))
	r.add(SkillExecution, "execute_batch_cancel", adaptStrictNative(s.ExecuteBatchCancel))
	r.add(SkillExecution, "execute_deposit_to_subaccount", adaptStrictNative(s.ExecuteDepositToSubaccount))
}

// RegisterExecution adds the full delegated-execution surface: the perps
// writes plus the domain-agnostic core.
func (r *Registry) RegisterExecution(s *delegated.Service) {
	r.RegisterExecutionPerps(s)
	r.RegisterExecutionCore(s)
}
