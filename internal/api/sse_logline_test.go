package api

import (
	"strings"
	"testing"
)

// A carriage return inside a log line must not reach the stream raw: it ends
// the SSE line, and what follows it is parsed as a new field.
func TestWriteLogLine_CarriageReturnCannotForgeEvents(t *testing.T) {
	var b strings.Builder
	writeLogLine(&b, "50%\revent: done\rdata: <b>x</b>")

	out := b.String()
	if strings.Contains(out, "\r") {
		t.Fatalf("raw carriage return in SSE output: %q", out)
	}
	if lines := strings.Split(strings.TrimSuffix(out, "\n\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("want a single data line, got %q", out)
	}
	if !strings.Contains(out, "50%&#13;event: done&#13;data: &lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("line not preserved as escaped text: %q", out)
	}
}
