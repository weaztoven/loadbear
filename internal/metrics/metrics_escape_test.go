package metrics

import "testing"

func TestEscapeLabel(t *testing.T) {
	const value = "backend\"\\\nname"
	const want = `backend\"\\\nname`
	if got := escapeLabel(value); got != want {
		t.Fatalf("escaped label = %q, want %q", got, want)
	}
}
