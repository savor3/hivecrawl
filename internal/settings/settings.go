// Package settings stores options changed from the web UI (currently the
// LLM provider) in the node's data directory, readable only by the node.
package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type LLM struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"`
}

type Settings struct {
	LLM *LLM `json:"llm,omitempty"`
}

func path(dir string) string { return filepath.Join(dir, "settings.json") }

// Load returns saved settings (empty if none were saved).
func Load(dir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Save writes settings with owner-only permissions.
func Save(dir string, s Settings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(dir))
}
