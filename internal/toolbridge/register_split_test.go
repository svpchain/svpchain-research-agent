package toolbridge

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"
)

// The per-family registration methods exist so per-category binaries can
// compose subsets of the bridged surface. These tests pin each family's tool
// set against the full table in register_test.go, so a tool added to New()
// without landing in exactly one family method fails here.

func sorted(ss []string) []string {
	out := append([]string{}, ss...)
	sort.Strings(out)
	return out
}

func TestFamilyMethodsMatchTheFullTable(t *testing.T) {
	h := &tools.Handlers{}
	families := map[string]func(*Registry){
		SkillMarketData: func(r *Registry) { r.RegisterMarketData(h) },
		SkillAccount:    func(r *Registry) { r.RegisterAccount(h) },
		SkillTrading:    func(r *Registry) { r.RegisterTrading(h) },
		SkillFunds:      func(r *Registry) { r.RegisterFunds(h) },
		SkillBroadcast:  func(r *Registry) { r.RegisterBroadcast(h) },
		SkillAuth:       func(r *Registry) { r.RegisterAuth(h) },
		SkillFaucet:     func(r *Registry) { r.RegisterFaucet(h) },
		SkillEVM:        func(r *Registry) { r.RegisterEVM(h) },
	}
	for skill, register := range families {
		r := NewEmpty()
		register(r)
		got := r.BySkill()
		if len(got) != 1 {
			t.Errorf("family %q registered tools under %d skills, expected 1: %v", skill, len(got), got)
			continue
		}
		if !reflect.DeepEqual(got[skill], sorted(expectedOps[skill])) {
			t.Errorf("family %q tools = %v, expected table = %v", skill, got[skill], sorted(expectedOps[skill]))
		}
	}
}

// The lending binary registers only the EVM landing rail; the split must
// cover the pinned EVM table exactly, with the rail holding the two broadcast
// tools.
func TestEVMBroadcastAndDeFiSplitCoverTheFamily(t *testing.T) {
	h := &tools.Handlers{}

	rail := NewEmpty()
	rail.RegisterEVMBroadcast(h)
	if got, want := rail.BySkill()[SkillEVM], []string{"broadcast_evm_tx", "evm_tx_status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EVM broadcast rail = %v, expected %v", got, want)
	}

	both := NewEmpty()
	both.RegisterEVMBroadcast(h)
	both.RegisterEVMDeFi(h)
	if got, want := both.BySkill()[SkillEVM], sorted(expectedOps[SkillEVM]); !reflect.DeepEqual(got, want) {
		t.Errorf("EVM broadcast+defi = %v, expected the full pinned table %v", got, want)
	}
}

// The core/perps execution split must partition the full execution surface:
// non-perps binaries register only the domain-agnostic core, and the perps
// writes stay unknown there — not refusing — so their cards never advertise
// perps execution.
func TestExecutionCorePerpsSplit(t *testing.T) {
	if got := sorted(executionTools); len(got) != len(executionCoreTools)+len(executionPerpsTools) {
		t.Fatalf("core+perps do not partition executionTools: %v", got)
	}

	core := NewEmpty()
	core.RegisterExecutionCore(nil)
	for _, tool := range executionCoreTools {
		op, ok := core.Lookup(tool)
		if !ok {
			t.Errorf("core tool %q missing", tool)
			continue
		}
		if _, err := op.Call(nil, nil); err == nil || !strings.Contains(err.Error(), "operator key") {
			t.Errorf("keyless core %q must refuse naming the operator-key requirement, got %v", tool, err)
		}
	}
	for _, tool := range executionPerpsTools {
		if _, ok := core.Lookup(tool); ok {
			t.Errorf("perps write %q must be unknown on a core-only registry, not registered", tool)
		}
	}

	perps := NewEmpty()
	perps.RegisterExecutionPerps(nil)
	for _, tool := range executionPerpsTools {
		if _, ok := perps.Lookup(tool); !ok {
			t.Errorf("perps tool %q missing", tool)
		}
	}

	full := NewEmpty()
	full.RegisterExecution(nil)
	for _, tool := range executionTools {
		if _, ok := full.Lookup(tool); !ok {
			t.Errorf("full execution tool %q missing", tool)
		}
	}
}
