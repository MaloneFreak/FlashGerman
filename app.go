package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dataDir string
	token   string
	port    int

	config  *ConfigStore
	deck    *DeckStore
	catalog *CatalogStore
	jobs    *JobManager
	local   *LocalManager
	life    *Lifecycle

	updMu  sync.Mutex
	update UpdateInfo

	winMu      sync.Mutex
	browserCmd *exec.Cmd
	noWindow   bool
}

type UpdateInfo struct {
	Available bool   `json:"available"`
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
}

func newApp(dataDir string) (*App, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, err := newConfigStore(dataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("não foi possível ler a configuração: %w", err)
	}
	app := &App{
		ctx:     ctx,
		cancel:  cancel,
		dataDir: dataDir,
		token:   randomID(24),
		config:  cfg,
		deck:    newDeckStore(dataDir),
		catalog: newCatalogStore(dataDir),
		jobs:    newJobManager(),
	}
	app.life = newLifecycle()
	app.local = newLocalManager(app)
	go app.catalog.RefreshRemote(ctx)
	go app.checkUpdate()
	go app.life.run(ctx)
	return app, nil
}

func (app *App) url() string { return fmt.Sprintf("http://127.0.0.1:%d/", app.port) }

func (app *App) openWindow() {
	if app.noWindow {
		return
	}
	cmd, err := openAppWindow(app.url(), filepath.Join(app.dataDir, "browser-profile"))
	if err != nil {
		log.Printf("não foi possível abrir a janela: %v", err)
		showFatal("O FlashGerman está rodando, mas não consegui abrir a janela.\nAbra no navegador: " + app.url())
		return
	}
	if cmd == nil {
		return
	}
	app.winMu.Lock()
	first := app.browserCmd == nil
	if first {
		app.browserCmd = cmd
	}
	app.winMu.Unlock()
	go func() {
		_ = cmd.Wait()
		if first {
			// O processo principal do navegador terminou: todas as janelas foram fechadas.
			app.winMu.Lock()
			app.browserCmd = nil
			app.winMu.Unlock()
			app.life.BrowserExited()
		}
	}()
}

// Close encerra tudo de forma organizada.
func (app *App) Close() {
	app.jobs.CancelAll()
	app.local.CancelInstall()
	app.local.Stop()
	app.cancel()
	app.winMu.Lock()
	if app.browserCmd != nil && app.browserCmd.Process != nil {
		_ = app.browserCmd.Process.Kill()
	}
	app.winMu.Unlock()
}

// configured diz se o app já pode gerar frases (senão a interface mostra a tela de setup).
func (app *App) configured(cfg Config) bool {
	switch cfg.Mode {
	case "api":
		prov, ok := app.catalog.Provider(cfg.ActiveProvider)
		ps := cfg.Providers[cfg.ActiveProvider]
		if !ok || ps == nil || ps.Model == "" {
			return false
		}
		if prov.NeedsKey && ps.APIKey == "" {
			return false
		}
		if prov.BaseURL == "" && ps.BaseURL == "" {
			return false
		}
		return true
	case "local":
		if cfg.Local.File == "" {
			return false
		}
		if _, ok := app.local.installedModels()[cfg.Local.ModelID]; !ok {
			return false
		}
		_, err := app.local.engineManifest(engineVariant(cfg.Local.GPU))
		return err == nil
	}
	return false
}

func (app *App) checkUpdate() {
	if version == "dev" {
		return
	}
	ctx, cancel := context.WithTimeout(app.ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repoSlug+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel) != nil {
		return
	}
	if compareVersions(rel.TagName, version) > 0 {
		app.updMu.Lock()
		app.update = UpdateInfo{Available: true, Latest: rel.TagName, URL: rel.HTMLURL}
		app.updMu.Unlock()
	}
}

var reVersion = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// compareVersions compara "v1.2.3" com "1.2.0": >0 se a for mais nova.
func compareVersions(a, b string) int {
	pa, pb := reVersion.FindStringSubmatch(a), reVersion.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Ciclo de vida: o app fecha sozinho quando a janela é fechada.

type Lifecycle struct {
	mu            sync.Mutex
	clients       map[string]time.Time
	everConnected bool
	emptySince    time.Time
	done          chan struct{}
	once          sync.Once
	Reason        string
}

const (
	clientTimeout = 3 * time.Minute  // navegadores reduzem timers de janelas minimizadas
	closeGrace    = 10 * time.Second // tempo para um "recarregar página" reconectar
)

func newLifecycle() *Lifecycle {
	return &Lifecycle{clients: map[string]time.Time{}, done: make(chan struct{})}
}

func (l *Lifecycle) Done() <-chan struct{} { return l.done }

func (l *Lifecycle) Shutdown(reason string) {
	l.once.Do(func() {
		l.Reason = reason
		close(l.done)
	})
}

func (l *Lifecycle) Heartbeat(id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	l.clients[id] = time.Now()
	l.everConnected = true
	l.emptySince = time.Time{}
	l.mu.Unlock()
}

func (l *Lifecycle) Bye(id string) {
	l.mu.Lock()
	delete(l.clients, id)
	if len(l.clients) == 0 {
		l.emptySince = time.Now()
	}
	l.mu.Unlock()
}

func (l *Lifecycle) BrowserExited() {
	l.mu.Lock()
	l.clients = map[string]time.Time{}
	l.everConnected = true
	l.emptySince = time.Now()
	l.mu.Unlock()
}

func (l *Lifecycle) run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		l.mu.Lock()
		for id, last := range l.clients {
			if time.Since(last) > clientTimeout {
				delete(l.clients, id)
				if len(l.clients) == 0 {
					l.emptySince = time.Now()
				}
			}
		}
		shouldClose := l.everConnected && len(l.clients) == 0 && !l.emptySince.IsZero() && time.Since(l.emptySince) > closeGrace
		l.mu.Unlock()
		if shouldClose {
			l.Shutdown("janela fechada")
			return
		}
	}
}

func cleanModelName(s string) string {
	return strings.TrimSpace(s)
}
