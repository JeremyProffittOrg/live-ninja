package realtime

import (
	"strings"
	"testing"
)

// The code-update clause is spliced into the MIDDLE of coreInstructions, so the
// join has to read as grammatical English. Nothing else catches this: the
// coverage test only asks whether each tool is named somewhere, and a doubled
// em dash or a missing space degrades the prompt silently.
func TestCodeUpdateSpliceReadsCleanly(t *testing.T) {
	idx := strings.Index(coreInstructions, codeUpdateToolInstructions)
	if idx < 0 {
		t.Fatal("the code-update clause is not present in coreInstructions")
	}
	start := idx - 60
	if start < 0 {
		start = 0
	}
	end := idx + len(codeUpdateToolInstructions) + 60
	if end > len(coreInstructions) {
		end = len(coreInstructions)
	}
	window := coreInstructions[start:end]

	for _, bad := range []string{"— —", "  ", " ,", " .", "—for", "..", ",,"} {
		if strings.Contains(window, bad) {
			t.Errorf("the splice produced %q:\n...%s...", bad, window)
		}
	}
	for _, tool := range []string{"code_update_repos", "code_update_start", "code_update_status"} {
		if !strings.Contains(coreInstructions, tool) {
			t.Errorf("%s is not named in coreInstructions", tool)
		}
	}
}
