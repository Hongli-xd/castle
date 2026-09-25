package idgen

import (
	"strings"
	"testing"
)

func TestNewPrefixAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New(PrefixSandbox)
		if !strings.HasPrefix(id, "sbx_") {
			t.Fatalf("missing prefix: %q", id)
		}
		if len(id) != len("sbx_")+rawLen {
			t.Fatalf("wrong length: %q", id)
		}
		if seen[id] {
			t.Fatalf("collision at %d: %q", i, id)
		}
		seen[id] = true
	}
}

func TestParse(t *testing.T) {
	id := New(PrefixSnapshot)
	if err := Parse(PrefixSnapshot, id); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	// kind confusion must be caught at the boundary
	if err := Parse(PrefixSandbox, id); err == nil {
		t.Fatal("snapshot id accepted as sandbox id")
	}
	if err := Parse(PrefixSandbox, "sbx_short"); err == nil {
		t.Fatal("short id accepted")
	}
	if err := Parse(PrefixSandbox, "sbx_!!!!!@@@@@@@@@@@@@@@@@@@@@@"); err == nil {
		t.Fatal("invalid charset accepted")
	}
}
