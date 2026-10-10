package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed all:web
var webFiles embed.FS

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(webRoot)
	mux.HandleFunc("GET /{$}", app.handleIndex)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.html" {
			app.handleIndex(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		static.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "app": appName, "version": version})
	})
	mux.HandleFunc("GET /api/state", app.handleState)
	mux.HandleFunc("GET /api/catalog", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, app.catalog.Get()) })
	mux.HandleFunc("POST /api/providers/models", app.handleListModels)
	mux.HandleFunc("POST /api/providers/save", app.handleSaveProvider)
	mux.HandleFunc("POST /api/providers/activate", app.handleActivateProvider)
	mux.HandleFunc("POST /api/providers/forget", app.handleForgetProvider)

	mux.HandleFunc("POST /api/generate", app.handleGenerate)
	mux.HandleFunc("GET /api/jobs/{id}", app.handleJob)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": app.jobs.Cancel(r.PathValue("id"))})
	})

	mux.HandleFunc("GET /api/deck", app.handleGetDeck)
	mux.HandleFunc("PUT /api/deck", app.handlePutDeck)

	mux.HandleFunc("GET /api/local/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, app.local.Status()) })
	mux.HandleFunc("POST /api/local/install", app.handleLocalInstall)
	mux.HandleFunc("POST /api/local/cancel", func(w http.ResponseWriter, r *http.Request) {
		app.local.CancelInstall()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/local/use", app.handleLocalUse)
	mux.HandleFunc("POST /api/local/delete", app.handleLocalDelete)

	mux.HandleFunc("POST /api/open-url", app.handleOpenURL)
	mux.HandleFunc("POST /api/open-data-dir", func(w http.ResponseWriter, r *http.Request) {
		if err := openFolder(app.dataDir); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientID string `json:"client_id"`
		}
		_ = readJSON(r, &req)
		app.life.Heartbeat(req.ClientID)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/bye", func(w http.ResponseWriter, r *http.Request) {
		app.life.Bye(r.URL.Query().Get("client"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/window", func(w http.ResponseWriter, r *http.Request) {
		go app.openWindow()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		go func() {
			time.Sleep(300 * time.Millisecond)
			app.life.Shutdown("pedido do usuário")
		}()
	})

	return app.guard(mux)
}

// guard protege o servidor local:
//   - só aceita o Host 127.0.0.1/localhost na porta do app (contra "DNS rebinding");
//   - toda rota /api exige o token secreto que vai embutido na página.
//
// Assim, nenhum site aberto no navegador consegue usar o app (ou suas chaves) por trás.
func (app *App) guard(next http.Handler) http.Handler {
	port := strconv.Itoa(app.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host != "127.0.0.1:"+port && host != "localhost:"+port {
			http.Error(w, "host inválido", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			token := r.Header.Get("X-FG-Token")
			if r.URL.Path == "/api/bye" {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(app.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "token inválido: recarregue a página")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://127.0.0.1:"+port && origin != "http://localhost:"+port {
				writeError(w, http.StatusForbidden, "origem não permitida")
				return
			}
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index ausente", http.StatusInternalServerError)
		return
	}
	data = bytes.ReplaceAll(data, []byte("__FG_TOKEN__"), []byte(app.token))
	data = bytes.ReplaceAll(data, []byte("__FG_VERSION__"), []byte(version))
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; media-src 'self' blob: data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------

func (app *App) handleState(w http.ResponseWriter, r *http.Request) {
	cfg := app.config.Get()
	providers := map[string]any{}
	for id, ps := range cfg.Providers {
		hint := ""
		if n := len(ps.APIKey); n > 0 {
			if n > 8 {
				hint = "…" + ps.APIKey[n-4:]
			} else {
				hint = "…"
			}
		}
		providers[id] = map[string]any{"has_key": ps.APIKey != "", "key_hint": hint, "model": ps.Model, "base_url": ps.BaseURL}
	}
	active := map[string]any{"kind": cfg.Mode}
	switch cfg.Mode {
	case "api":
		prov, _ := app.catalog.Provider(cfg.ActiveProvider)
		active["provider_id"] = cfg.ActiveProvider
		active["provider_name"] = prov.Name
		if ps := cfg.Providers[cfg.ActiveProvider]; ps != nil {
			active["model"] = ps.Model
		}
	case "local":
		active["model_id"] = cfg.Local.ModelID
		active["name"] = cfg.Local.Name
		active["gpu"] = cfg.Local.GPU
	}
	free, _ := diskFree(app.dataDir)
	app.updMu.Lock()
	upd := app.update
	app.updMu.Unlock()
	writeJSON(w, map[string]any{
		"version":      version,
		"repo":         repoSlug,
		"update":       upd,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"ram_bytes":    totalRAM(),
		"free_disk":    free,
		"data_dir":     app.dataDir,
		"configured":   app.configured(cfg),
		"mode":         cfg.Mode,
		"active":       active,
		"providers":    providers,
		"local":        app.local.Status(),
		"local_config": cfg.Local,
	})
}

type providerRequest struct {
	ProviderID string `json:"provider_id"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	SkipTest   bool   `json:"skip_test"`
}

// resolveProvider junta o que veio da tela com o que já estava salvo (ex.: chave antiga).
func (app *App) resolveProvider(req providerRequest) (ProviderInfo, string, string, error) {
	prov, ok := app.catalog.Provider(req.ProviderID)
	if !ok {
		return prov, "", "", errors.New("provedor desconhecido")
	}
	cfg := app.config.Get()
	saved := cfg.Providers[req.ProviderID]
	key := strings.TrimSpace(req.APIKey)
	if key == "" && saved != nil {
		key = saved.APIKey
	}
	baseURL := prov.BaseURL
	if prov.EditableURL || prov.ID == "custom" {
		if u := strings.TrimSpace(req.BaseURL); u != "" {
			baseURL = u
		} else if saved != nil && saved.BaseURL != "" {
			baseURL = saved.BaseURL
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return prov, "", "", errors.New("informe o endereço da API (por exemplo https://api.exemplo.com/v1)")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return prov, "", "", errors.New("o endereço precisa começar com http:// ou https://")
	}
	if prov.NeedsKey && key == "" {
		return prov, "", "", errors.New("cole a sua chave de API")
	}
	return prov, baseURL, key, nil
}

func (app *App) handleListModels(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prov, baseURL, key, err := app.resolveProvider(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	models, err := listModels(r.Context(), prov, baseURL, key)
	if err != nil {
		writeError(w, http.StatusBadGateway, friendlyError(err))
		return
	}
	ranked := rankModels(prov, models)
	suggested := ""
	if len(ranked) > 0 {
		suggested = ranked[0].ID
	}
	writeJSON(w, map[string]any{"models": ranked, "suggested": suggested, "total": len(models)})
}

func (app *App) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prov, baseURL, key, err := app.resolveProvider(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	model := cleanModelName(req.Model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "escolha um modelo")
		return
	}
	if !req.SkipTest {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		_, err := streamChat(ctx, ChatRequest{
			Provider: prov, BaseURL: baseURL, APIKey: key, Model: model,
			System: "Você é um assistente de testes.", User: "Responda apenas: OK", SchemaMode: schemaNone,
			IdleLimit: 80 * time.Second,
		}, StreamCallbacks{})
		if err != nil {
			writeError(w, http.StatusBadGateway, "O teste falhou: "+friendlyError(err))
			return
		}
	}
	err = app.config.Update(func(c *Config) {
		ps := &ProviderSettings{APIKey: key, Model: model}
		if prov.EditableURL || prov.ID == "custom" {
			ps.BaseURL = baseURL
		}
		c.Providers[prov.ID] = ps
		c.Mode = "api"
		c.ActiveProvider = prov.ID
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleActivateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := app.config.Get()
	if ps := cfg.Providers[req.ProviderID]; ps == nil || ps.Model == "" {
		writeError(w, http.StatusBadRequest, "este provedor ainda não foi configurado")
		return
	}
	_ = app.config.Update(func(c *Config) { c.Mode = "api"; c.ActiveProvider = req.ProviderID })
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleForgetProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = app.config.Update(func(c *Config) {
		delete(c.Providers, req.ProviderID)
		if c.Mode == "api" && c.ActiveProvider == req.ProviderID {
			c.Mode = ""
			c.ActiveProvider = ""
		}
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var p GenerateParams
	if err := readJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := app.jobs.Start(app, p)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, job.View())
}

func (app *App) handleJob(w http.ResponseWriter, r *http.Request) {
	j := app.jobs.Get(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, "geração não encontrada")
		return
	}
	writeJSON(w, j.View())
}

func (app *App) handleGetDeck(w http.ResponseWriter, r *http.Request) {
	data, err := app.deck.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func (app *App) handlePutDeck(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 50<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "baralho grande demais")
		return
	}
	if err := app.deck.Save(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleLocalInstall(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := app.local.StartInstall(req); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleLocalUse(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := app.local.UseInstalled(req.ModelID, req.GPU); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := app.local.DeleteModel(req.ModelID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (app *App) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := openURLDefault(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.New("pedido inválido: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("erro ao responder: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// listenLocal tenta a porta preferida (para o endereço ser sempre o mesmo) e, se estiver
// ocupada, usa qualquer porta livre.
func listenLocal(preferred int) (net.Listener, int, error) {
	if preferred > 0 {
		if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred)); err == nil {
			return l, preferred, nil
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}
