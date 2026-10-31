package node

import (
	"slices"
	"testing"
)

func TestIntents(t *testing.T) {
	for q, want := range map[string][]string{
		"python asyncio timeout error":                 {"developer"},
		"TypeError: cannot read property of undefined": {"developer"},
		"useEffect cleanup function":                   {"developer"},
		"crispr off-target effects studies":            {"research"},
		"bohemian rhapsody lyrics":                     {"music"},
		"inception movie cast of":                      {"movies"},
		"best pizza in naples":                         nil,
		"capital of france":                            nil, // "api" inside a word
		"apple watch review":                           nil,
		"book a flight to paris":                       nil,
	} {
		if got := Intents(q); !slices.Equal(got, want) {
			t.Errorf("Intents(%q) = %v, want %v", q, got, want)
		}
	}
	if got := Intents("python video tutorial with lyrics paper on arxiv"); len(got) != maxIntents {
		t.Errorf("not capped: %v", got)
	}
}
