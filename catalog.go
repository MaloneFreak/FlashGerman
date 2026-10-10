package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// O catálogo lista os provedores de API sugeridos, os modelos locais e onde baixar o motor.
// Uma cópia vai embutida no executável; ao abrir, o app tenta buscar a versão mais nova
// no GitHub, assim as recomendações podem ser atualizadas sem lançar uma nova versão.

//go:embed catalog.json
var embeddedCatalog []byte

const catalogSchemaVersion = 1

type ProviderInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"` // "openai" (compatível) ou "anthropic"
	BaseURL      string   `json:"base_url"`
	Pricing      string   `json:"pricing"` // free, paid, local, custom
	Badge        string   `json:"badge"`
	Description  string   `json:"description"`
	KeyURL       string   `json:"key_url"`
	KeyHint      string   `json:"key_hint"`
	NeedsKey     bool     `json:"needs_key"`
	EditableURL  bool     `json:"editable_url,omitempty"`
	PreferFree   bool     `json:"prefer_free,omitempty"`
	ModelPrefer  []string `json:"model_prefer"`
	ModelExclude []string `json:"model_exclude"`
	StripPrefix  string   `json:"strip_prefix,omitempty"`
}

type LocalModelInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Badge        string   `json:"badge"`
	Description  string   `json:"description"`
	Repo         string   `json:"repo"`
	Quants       []string `json:"quants"`
	FallbackFile string   `json:"fallback_file"`
	ApproxSizeGB float64  `json:"approx_size_gb"`
	MinRAMGB     float64  `json:"min_ram_gb"`
	License      string   `json:"license"`
	Recommended  bool     `json:"recommended,omitempty"`
}

type EngineInfo struct {
	ReleasesAPI    string            `json:"releases_api"`
	Assets         map[string]string `json:"assets"`
	FallbackTag    string            `json:"fallback_tag"`
	FallbackURL    string            `json:"fallback_url"`
	FallbackAssets map[string]string `json:"fallback_assets"`
}

type Catalog struct {
	SchemaVersion int              `json:"schema_version"`
	Updated       string           `json:"updated"`
	Providers     []ProviderInfo   `json:"providers"`
	LocalModels   []LocalModelInfo `json:"local_models"`
	Engine        EngineInfo       `json:"engine"`
}

func parseCatalog(data []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.SchemaVersion != catalogSchemaVersion {
		return nil, fmt.Errorf("versão de catálogo %d não suportada", c.SchemaVersion)
	}
	if len(c.Providers) == 0 || len(c.Engine.Assets) == 0 {
		return nil, errors.New("catálogo incompleto")
	}
	return &c, nil
}

type CatalogStore struct {
	mu        sync.RWMutex
	cat       *Catalog
	cachePath string
	remoteURL string
}

func newCatalogStore(dataDir string) *CatalogStore {
	embedded, err := parseCatalog(embeddedCatalog)
	if err != nil {
		panic("catálogo embutido inválido: " + err.Error())
	}
	s := &CatalogStore{
		cat:       embedded,
		cachePath: filepath.Join(dataDir, "catalog-cache.json"),
		remoteURL: "https://raw.githubusercontent.com/" + repoSlug + "/main/catalog.json",
	}
	if data, err := os.ReadFile(s.cachePath); err == nil {
		if cached, err := parseCatalog(data); err == nil && cached.Updated > embedded.Updated {
			s.cat = cached
		}
	}
	return s
}

func (s *CatalogStore) Get() *Catalog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cat
}

func (s *CatalogStore) Provider(id string) (ProviderInfo, bool) {
	for _, p := range s.Get().Providers {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderInfo{}, false
}

func (s *CatalogStore) LocalModel(id string) (LocalModelInfo, bool) {
	for _, m := range s.Get().LocalModels {
		if m.ID == id {
			return m, true
		}
	}
	return LocalModelInfo{}, false
}

// RefreshRemote baixa o catálogo do GitHub. Falhas são ignoradas (o app funciona offline).
func (s *CatalogStore) RefreshRemote(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.remoteURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("catálogo remoto indisponível: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("catálogo remoto: HTTP %d", resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	remote, err := parseCatalog(data)
	if err != nil {
		log.Printf("catálogo remoto ignorado: %v", err)
		return
	}
	s.mu.Lock()
	if remote.Updated >= s.cat.Updated {
		s.cat = remote
	}
	s.mu.Unlock()
	_ = writeFileAtomic(s.cachePath, data, 0o644)
}
