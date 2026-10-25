package settings

import (
	"os"
	"testing"
)

func TestSaveLoadPrivate(t *testing.T) {
	dir := t.TempDir()
	if s, err := Load(dir); err != nil || s.LLM != nil {
		t.Fatalf("empty dir: %v %+v", err, s)
	}
	want := Settings{LLM: &LLM{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-chat", APIKey: "sk-x"}}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || *got.LLM != *want.LLM {
		t.Fatalf("got %+v %v", got.LLM, err)
	}
	if fi, _ := os.Stat(path(dir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode %v, want 0600", fi.Mode().Perm())
	}
}
