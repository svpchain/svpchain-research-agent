package deps_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★ This module depends on the chain's protocol module, which is a Cosmos chain
// carrying its own forks of cosmos-sdk, cometbft, cosmos/evm and iavl. Go does
// not apply a dependency's replace directives, so ours must restate every one of
// protocol's verbatim — and drift does not fail cleanly. The build silently
// resolves upstream cosmos and dies somewhere unrelated-looking.
//
// So diff against protocol's go.mod on every `go test ./...` rather than
// trusting a hand-maintained copy. protocol is the single source of truth; a
// checked-in copy of the blocks would just be a second thing that can drift.
//
// This used to compare against svpchain-agent-core, back when five agent repos
// each held a copy. Core is now vendored into internal/ and gone as a module —
// and comparing against it was always weaker than this, because core held the
// same hand-copied blocks. It was a mirror checking itself.
func TestReplaceBlocksMatchProtocol(t *testing.T) {
	const protocolModule = "github.com/dydxprotocol/v4-chain/protocol"

	protocolDir := moduleDir(t, protocolModule)
	upstream := replaceLines(t, filepath.Join(protocolDir, "go.mod"))
	ours := replaceLines(t, "go.mod")

	// A parse that silently yields nothing would make every assertion below
	// vacuous, which is the exact failure this test was rewritten to escape.
	if len(upstream) == 0 {
		t.Fatalf("parsed no replace directives from %s/go.mod", protocolDir)
	}

	// Our replace of protocol itself is what points at the sibling checkout,
	// not an inherited pin; protocol obviously does not replace itself.
	delete(ours, protocolModule)

	for path, target := range upstream {
		got, ok := ours[path]
		if !ok {
			t.Errorf("go.mod is missing protocol's replace for %s => %s", path, target)
			continue
		}
		if got != target {
			t.Errorf("replace for %s is %q, protocol has %q", path, got, target)
		}
	}
	for path := range ours {
		if _, ok := upstream[path]; !ok {
			t.Errorf("go.mod replaces %s, which protocol does not — it will resolve differently here", path)
		}
	}
}

// svpdt is a tagged module whose own tests forbid replacing it; a local replace
// here would silently diverge this agent's credential verification from every
// other participant's.
func TestSvpdtIsNeverReplaced(t *testing.T) {
	for path := range replaceLines(t, "go.mod") {
		if strings.Contains(path, "svpchain/svpdt") {
			t.Errorf("go.mod replaces %s; svpdt must always be the tagged module", path)
		}
	}
}

// moduleDir asks the go tool where a required module resolves on disk.
//
// -mod=mod is load-bearing, not tidiness. `make vendor` and scripts/deploy.sh
// both materialize ./vendor, and under a vendored build `go list -m` reports an
// empty Dir for every module — so without it this whole test silently skipped
// on any tree that had ever been vendored, which is most of them. That is the
// exact failure this test was rewritten to escape, so it fails rather than
// skips: the caller asks for a module this go.mod replaces with a sibling
// checkout, and an unresolvable answer means the tree is broken.
func moduleDir(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-mod=mod", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		t.Fatalf("cannot locate %s: %v", module, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("%s resolved to an empty directory", module)
	}
	return dir
}

// replaceLines returns path -> replacement target for every replace directive,
// covering both the single-line and block forms.
func replaceLines(t *testing.T, gomod string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatalf("read %s: %v", gomod, err)
	}

	out := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "replace (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimPrefix(line, "replace ")
		case !inBlock:
			continue
		}
		lhs, rhs, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		out[strings.Fields(strings.TrimSpace(lhs))[0]] = strings.TrimSpace(rhs)
	}
	return out
}
