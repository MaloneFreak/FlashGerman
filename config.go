package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ProviderSettings guarda o que o usuário configurou para cada provedor de API.
type ProviderSettings struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

// LocalSettings descreve o modelo local escolhido.
type LocalSettings struct {
	ModelID string `json:"model_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Repo    string `json:"repo,omitempty"`
	File    string `json:"file,omitempty"`
	GPU     bool   `json:"gpu"`
	CtxSize int    `json:"ctx_size,omitempty"`
	Threads int    `json:"threads,omitempty"`
}

// Config é salvo em data/config.json.
type Config struct {
	Mode           string                       `json:"mode"` // "", "api" ou "local"
	ActiveProvider string                       `json:"active_provider,omitempty"`
	Providers      map[string]*ProviderSettings `json:"providers,omitempty"`
	Local          LocalSettings                `json:"local"`
}

type ConfigStore struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func newConfigStore(dataDir string) (*ConfigStore, error) {
	s := &ConfigStore{path: filepath.Join(dataDir, "config.json")}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.cfg); err != nil {
			// Config corrompida: guarda uma cópia e recomeça, em vez de travar o app.
			_ = os.Rename(s.path, s.path+".corrompido")
			s.cfg = Config{}
		}
	}
	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]*ProviderSettings{}
	}
	return s, nil
}

// Get devolve uma cópia profunda da configuração atual.
func (s *ConfigStore) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Providers = make(map[string]*ProviderSettings, len(s.cfg.Providers))
	for k, v := range s.cfg.Providers {
		cp := *v
		c.Providers[k] = &cp
	}
	return c
}

// Update aplica uma alteração e salva no disco.
func (s *ConfigStore) Update(fn func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]*ProviderSettings{}
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}

// DeckStore guarda o baralho (lista de cartões) em data/deck.json.
// O conteúdo é opaco para o backend: quem define o formato de cada cartão é a interface.
type DeckStore struct {
	mu   sync.Mutex
	path string
}

func newDeckStore(dataDir string) *DeckStore {
	return &DeckStore{path: filepath.Join(dataDir, "deck.json")}
}

func (d *DeckStore) Load() (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return json.RawMessage("[]"), nil
	}
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '[' || !json.Valid(data) {
		_ = os.Rename(d.path, d.path+".corrompido")
		return json.RawMessage("[]"), nil
	}
	return json.RawMessage(data), nil
}

func (d *DeckStore) Save(cards json.RawMessage) error {
	cards = bytes.TrimSpace(cards)
	if len(cards) == 0 || cards[0] != '[' || !json.Valid(cards) {
		return errors.New("o baralho precisa ser uma lista JSON")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return writeFileAtomic(d.path, cards, 0o644)
}
