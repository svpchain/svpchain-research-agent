package deps_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★ The worst cost of splitting the agents into their own repos: the go.mod
// replace directives that pin the chain's protocol module now exist in five
// copies instead of one. They must all track ../svpagent/protocol, and drift
// does not fail cleanly — the build silently resolves upstream cosmos and dies
// somewhere unrelated-looking.
//
// So compare against core's go.mod on every `go test ./...`, rather than
// trusting five copies to be maintained by hand. Core's go.mod is the single
// source of truth; a separate checked-in copy of the blocks would just be a
// sixth thing that can drift.
func TestReplaceBlocksMatchCore(t *testing.T) {
	coreDir := moduleDir(t, "github.com/svpchain/svpchain-agent-core")
	core := replaceLines(t, filepath.Join(coreDir, "go.mod"))
	ours := replaceLines(t, "go.mod")

	// Our own replace of core itself is local scaffolding, not an inherited
	// pin; it is expected to differ and to disappear once core is published.
	delete(ours, "github.com/svpchain/svpchain-agent-core")

	for path, target := range core {
		got, ok := ours[path]
		if !ok {
			t.Errorf("go.mod is missing core's replace for %s => %s", path, target)
			continue
		}
		if got != target {
			t.Errorf("replace for %s is %q, core has %q", path, got, target)
		}
	}
	for path := range ours {
		if _, ok := core[path]; !ok {
			t.Errorf("go.mod replaces %s, which core does not — it will resolve differently here", path)
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

// moduleDir asks the go tool where a required module resolves on disk, so this
// works both with the local replace and with a published, tagged core.
func moduleDir(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		t.Skipf("cannot locate %s (module not resolvable here): %v", module, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Skipf("%s resolved to an empty directory", module)
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
