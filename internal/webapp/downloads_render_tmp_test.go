package webapp

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderDownloadsPageTmp(t *testing.T) {
	_, rend := newTestShell(t)
	var buf bytes.Buffer
	if err := rend.Render(&buf, "pages/downloads", nil); err != nil {
		t.Fatalf("render downloads: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"<title>Downloads — Live Ninja</title>",
		`id="dlTable"`,
		`id="dlBulkBar"`,
		`id="dlLoading"`,
		`id="dlEmpty"`,
		`id="dlError"`,
		`href="/downloads" aria-current="page"`,
		"downloads.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("downloads output missing %q", want)
		}
	}
}
