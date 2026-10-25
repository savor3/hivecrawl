package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hivecrawl/internal/llm"
	"hivecrawl/internal/settings"
)

// LLM settings from the web UI. Saved to the node's data directory (owner
// read/write only) and applied immediately. The API key is never returned.

// llm is the LLM in use (nil = off). It can change at runtime.
func (s *Server) llm() *llm.Client { return s.llmCur.Load() }

type llmSettingsIn struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"` // empty = keep the saved key
}

func keyHint(k string) string {
	if len(k) < 8 {
		return ""
	}
	return k[:3] + "…" + k[len(k)-4:]
}

func (s *Server) currentLLM() settings.LLM {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.llmCfg == nil {
		return settings.LLM{}
	}
	return *s.llmCfg
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	c := s.currentLLM()
	writeJSON(w, http.StatusOK, map[string]any{
		"llm": map[string]any{
			"configured": s.llm() != nil, "provider": c.Provider, "baseUrl": c.BaseURL, "model": c.Model,
			"keySet": c.APIKey != "", "keyHint": keyHint(c.APIKey),
		},
		"saved": s.DataDir != "",
	})
}

// resolve fills an empty key from the saved one and validates the input.
func (s *Server) resolve(in llmSettingsIn) (settings.LLM, error) {
	in.BaseURL = strings.TrimSuffix(strings.TrimSpace(in.BaseURL), "/")
	in.Model = strings.TrimSpace(in.Model)
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return settings.LLM{}, errors.New("base URL must be an http(s) URL, e.g. https://api.openai.com/v1")
	}
	if in.Model == "" {
		return settings.LLM{}, errors.New("model is required")
	}
	key := strings.TrimSpace(in.APIKey)
	if key == "" {
		key = s.currentLLM().APIKey
	}
	return settings.LLM{Provider: in.Provider, BaseURL: in.BaseURL, Model: in.Model, APIKey: key}, nil
}

func (s *Server) saveLLM(w http.ResponseWriter, r *http.Request) {
	var in llmSettingsIn
	if !decode(w, r, &in) {
		return
	}
	c, err := s.resolve(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := s.persist(&c); err != nil {
		writeJSON(w, http.StatusInternalServerError, fcError{Error: "could not save settings: " + err.Error()})
		return
	}
	s.getSettings(w, r)
}

func (s *Server) deleteLLM(w http.ResponseWriter, r *http.Request) {
	if err := s.persist(nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, fcError{Error: "could not save settings: " + err.Error()})
		return
	}
	s.getSettings(w, r)
}

// persist saves (c == nil: removes) the LLM settings and switches to them.
func (s *Server) persist(c *settings.LLM) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.DataDir != "" {
		st, err := settings.Load(s.DataDir)
		if err != nil {
			return err
		}
		st.LLM = c
		if err := settings.Save(s.DataDir, st); err != nil {
			return err
		}
	}
	s.llmCfg = c
	if c == nil {
		s.llmCur.Store(nil)
	} else {
		s.llmCur.Store(llm.NewFor(c.Provider, c.BaseURL, c.APIKey, c.Model))
	}
	return nil
}

// testLLM sends a tiny prompt with the given (unsaved) settings.
func (s *Server) testLLM(w http.ResponseWriter, r *http.Request) {
	var in llmSettingsIn
	if !decode(w, r, &in) {
		return
	}
	c, err := s.resolve(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	reply, err := llm.NewFor(c.Provider, c.BaseURL, c.APIKey, c.Model).Chat(ctx, []llm.Message{{Role: "user", Content: "Reply with the single word OK."}}, false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reply": llm.Truncate(strings.TrimSpace(reply), 200), "latencyMs": time.Since(start).Milliseconds()})
}

// listModels asks the provider which models the key can use.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	var in llmSettingsIn
	if !decode(w, r, &in) {
		return
	}
	in.Model = "x" // not needed to list
	c, err := s.resolve(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := llm.NewFor(c.Provider, c.BaseURL, c.APIKey, "").Models(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "models": models})
}
