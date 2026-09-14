package realtime

import (
	"strings"
	"unicode/utf8"
)

// REMEMBERED block (plan.md agentcore-memory, milestone mint-preload).
//
// The broker retrieves the top long-term records for the caller at mint and
// renders them here, after the BASE KNOWLEDGE block. The memory layer used to
// be read-never in production because recall depended on the model choosing
// to call memory_search; a block that is simply present in the instructions
// does not depend on that choice. memoryUsageDirective tells the model how
// to treat it.

const (
	// RememberedHeader is the block's first line. Unique on purpose — tests
	// locate the block by it, the same way they locate BASE KNOWLEDGE.
	RememberedHeader = "REMEMBERED — learned in earlier conversations (may be out of date; confirm when it matters, and call memory_search for anything not listed here):"
	// maxRememberedItems bounds the block; the broker asks for ten.
	maxRememberedItems = 10
	// maxRememberedRunes bounds one line so a long record cannot crowd out
	// the persona.
	maxRememberedRunes = 400
)

// RememberedBlock renders the records as an instructions suffix. Empty input
// renders "" so a session with nothing remembered mints exactly as before.
func RememberedBlock(items []string) string {
	seen := make(map[string]struct{}, len(items))
	lines := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(strings.ReplaceAll(it, "\n", " "))
		if it == "" {
			continue
		}
		if utf8.RuneCountInString(it) > maxRememberedRunes {
			r := []rune(it)
			it = strings.TrimSpace(string(r[:maxRememberedRunes])) + "…"
		}
		key := strings.ToLower(it)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		lines = append(lines, it)
		if len(lines) == maxRememberedItems {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n")
	b.WriteString(RememberedHeader)
	for _, l := range lines {
		b.WriteString("\n- ")
		b.WriteString(l)
	}
	return b.String()
}
