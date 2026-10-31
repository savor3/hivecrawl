package node

import (
	"os"
	"strings"
	"testing"
)

// Every category engine must be kept in SearXNG's settings and disabled
// there, or it either does not exist or leaks into ordinary web searches.
func TestTopicEnginesAreKeptAndDisabled(t *testing.T) {
	b, err := os.ReadFile("../../deploy/searxng/settings.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	kept := s[strings.Index(s, "keep_only:"):strings.Index(s, "\nserver:")]
	for topic, engines := range Topics {
		for _, e := range engines {
			if !strings.Contains(kept, "      - "+e+"\n") {
				t.Errorf("%s: engine %q is not in keep_only", topic, e)
			}
			if !strings.Contains(s, "  - name: "+e+"\n    disabled: true\n") {
				t.Errorf("%s: engine %q is not disabled", topic, e)
			}
		}
	}
}

func TestAllTopicsCoversEveryCategory(t *testing.T) {
	all := AllTopics()
	if all[0] != TopicWeb {
		t.Fatalf("all should start with web: %v", all)
	}
	if len(all) != len(Topics) { // web added, github left out
		t.Fatalf("all = %v", all)
	}
}
