package realtime

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func TestRememberedBlockEmptyRendersNothing(t *testing.T) {
	assert.Equal(t, "", RememberedBlock(nil))
	assert.Equal(t, "", RememberedBlock([]string{"", "   ", "\n"}))
}

func TestRememberedBlockRendersDedupedClippedLines(t *testing.T) {
	long := strings.Repeat("x", 450)
	items := make([]string, 0, 14)
	items = append(items, "Sister: Sarah, in Austin", "sister: sarah, in austin", "Line\nwith\nbreaks", long)
	for i := 0; i < 10; i++ {
		items = append(items, "filler "+string(rune('a'+i)))
	}
	out := RememberedBlock(items)

	require.True(t, strings.HasPrefix(out, "\n\n"+RememberedHeader), "block starts on its own paragraph")
	lines := strings.Split(strings.TrimPrefix(out, "\n\n"+RememberedHeader+"\n"), "\n")
	assert.Len(t, lines, maxRememberedItems, "capped at ten lines")
	assert.Equal(t, "- Sister: Sarah, in Austin", lines[0])
	assert.Equal(t, "- Line with breaks", lines[1], "case-insensitive duplicate dropped, newlines flattened")
	assert.True(t, strings.HasSuffix(lines[2], "…"), "long line clipped with an ellipsis")
	assert.LessOrEqual(t, len([]rune(lines[2])), maxRememberedRunes+3)
}

func TestRememberedBlockComposesAfterBaseKnowledge(t *testing.T) {
	p := store.Profile{DisplayName: "Jeremy", HomeLocation: testHome()}
	instructions := "PERSONA." + SessionDirectives + BuildBaseKnowledge(p, time.Now()) +
		RememberedBlock([]string{"Drives a 2019 Outback"}) + "\n\nGUIDES."

	baseAt := strings.Index(instructions, "BASE KNOWLEDGE")
	rememberedAt := strings.Index(instructions, RememberedHeader)
	guidesAt := strings.Index(instructions, "GUIDES.")
	require.True(t, baseAt < rememberedAt, "remembered after base knowledge")
	require.True(t, rememberedAt < guidesAt, "remembered before guides")
	assert.Equal(t, 1, strings.Count(instructions, RememberedHeader),
		"the header must be unique — the directive mentions REMEMBERED but never spells the header")
	assert.Contains(t, SessionDirectives, "REMEMBERED", "the directive tells the model the block exists")
}
