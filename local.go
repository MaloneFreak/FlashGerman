package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LocalManager cuida da "IA local": baixa o motor (llama.cpp / llama-server) e o modelo
// (arquivo .gguf do Hugging Face) e mantém o llama-server rodando enquanto o app estiver aberto.
type LocalManager struct {
	app *App

	startMu   sync.Mutex
	mu        sync.Mutex
	cmd       *exec.Cmd
	exited    chan struct{}
	port      int
	modelPath string
	variant   string
	ready     bool
	busy      bool
	lastUsed  time.Time

	instMu     sync.Mutex
	inst       InstallState
	instCancel context.CancelFunc
}

type InstallState struct {
	Active  bool    `json:"active"`
	Stage   string  `json:"stage"` // idle, engine, model, verify, done, error, cancelled
	Label   string  `json:"label"`
	ModelID string  `json:"model_id,omitempty"`
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Speed   float64 `json:"speed"` // bytes/s
	Error   string  `json:"error,omitempty"`
}

// InstalledModel fica registrado em data/models/models.json.
type InstalledModel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type EngineManifest struct {
	Tag     string `json:"tag"`
	Variant string `json:"variant"`
	Asset   string `json:"asset"`
	Server  string `json:"server"` // caminho relativo do llama-server
}

func newLocalManager(app *App) *LocalManager {
	lm := &LocalManager{app: app, inst: InstallState{Stage: "idle"}}
	lm.killStaleServer()
	go lm.idleWatcher()
	return lm
}

func (lm *LocalManager) engineRoot() string { return filepath.Join(lm.app.dataDir, "engine") }
func (lm *LocalManager) modelsDir() string  { return filepath.Join(lm.app.dataDir, "models") }
func (lm *LocalManager) pidFile() string    { return filepath.Join(lm.app.dataDir, "llama-server.pid") }

// gpuSupported indica se oferecemos a opção de GPU (Vulkan). No Mac (Apple Silicon)
// a aceleração Metal já vem ligada na versão padrão.
func gpuSupported() bool {
	return (runtime.GOOS == "windows" || runtime.GOOS == "linux") && runtime.GOARCH == "amd64"
}

func engineVariant(gpu bool) string {
	if gpu && gpuSupported() {
		return "vulkan"
	}
	return "cpu"
}

func platformKey(variant string) string {
	return runtime.GOOS + "-" + runtime.GOARCH + "-" + variant
}

func (lm *LocalManager) engineManifest(variant string) (*EngineManifest, error) {
	data, err := os.ReadFile(filepath.Join(lm.engineRoot(), variant, "engine.json"))
	if err != nil {
		return nil, err
	}
	var m EngineManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(lm.engineRoot(), variant, m.Server)); err != nil {
		return nil, err
	}
	return &m, nil
}

func (lm *LocalManager) installedModels() map[string]InstalledModel {
	out := map[string]InstalledModel{}
	data, err := os.ReadFile(filepath.Join(lm.modelsDir(), "models.json"))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	for id, m := range out {
		st, err := os.Stat(filepath.Join(lm.modelsDir(), m.File))
		if err != nil || (m.Size > 0 && st.Size() != m.Size) {
			delete(out, id)
		}
	}
	return out
}

func (lm *LocalManager) saveInstalledModels(models map[string]InstalledModel) error {
	data, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(lm.modelsDir(), "models.json"), data, 0o644)
}

// ---------------------------------------------------------------------------
// Status

type LocalStatus struct {
	Engine struct {
		Installed bool   `json:"installed"`
		Variant   string `json:"variant"`
		Tag       string `json:"tag,omitempty"`
	} `json:"engine"`
	Installed    map[string]InstalledModel `json:"installed"`
	Install      InstallState              `json:"install"`
	Server       map[string]any            `json:"server"`
	GPUSupported bool                      `json:"gpu_supported"`
	Platform     string                    `json:"platform"`
	PlatformOK   bool                      `json:"platform_ok"`
}

func (lm *LocalManager) Status() LocalStatus {
	cfg := lm.app.config.Get()
	var st LocalStatus
	st.Engine.Variant = engineVariant(cfg.Local.GPU)
	if m, err := lm.engineManifest(st.Engine.Variant); err == nil {
		st.Engine.Installed = true
		st.Engine.Tag = m.Tag
	}
	st.Installed = lm.installedModels()
	lm.instMu.Lock()
	st.Install = lm.inst
	lm.instMu.Unlock()
	lm.mu.Lock()
	st.Server = map[string]any{"running": lm.cmd != nil, "ready": lm.ready}
	if lm.modelPath != "" {
		st.Server["model"] = filepath.Base(lm.modelPath)
	}
	lm.mu.Unlock()
	st.GPUSupported = gpuSupported()
	st.Platform = runtime.GOOS + "/" + runtime.GOARCH
	_, st.PlatformOK = lm.app.catalog.Get().Engine.Assets[platformKey("cpu")]
	return st
}

// ---------------------------------------------------------------------------
// Instalação (motor + modelo)

type InstallRequest struct {
	ModelID string `json:"model_id"`
	Repo    string `json:"repo"` // para modelo personalizado (usuário/repositório-GGUF)
	File    string `json:"file"` // opcional: arquivo .gguf específico do repositório
	GPU     bool   `json:"gpu"`
}

func (lm *LocalManager) setInstall(fn func(s *InstallState)) {
	lm.instMu.Lock()
	fn(&lm.inst)
	lm.instMu.Unlock()
}

func (lm *LocalManager) StartInstall(req InstallRequest) error {
	lm.instMu.Lock()
	if lm.inst.Active {
		lm.instMu.Unlock()
		return errors.New("já existe um download em andamento")
	}
	ctx, cancel := context.WithCancel(lm.app.ctx)
	lm.instCancel = cancel
	lm.inst = InstallState{Active: true, Stage: "engine", Label: "Preparando…", ModelID: req.ModelID}
	lm.instMu.Unlock()

	go func() {
		defer cancel()
		err := lm.install(ctx, req)
		lm.setInstall(func(s *InstallState) {
			s.Active = false
			s.Speed = 0
			switch {
			case err == nil:
				s.Stage = "done"
				s.Label = "Pronto! A IA local está instalada."
				s.Error = ""
			case errors.Is(err, context.Canceled):
				s.Stage = "cancelled"
				s.Label = "Download pausado. Clique em baixar de novo para continuar de onde parou."
				s.Error = ""
			default:
				s.Stage = "error"
				s.Error = friendlyError(err)
				log.Printf("instalação local falhou: %v", err)
			}
		})
	}()
	return nil
}

func (lm *LocalManager) CancelInstall() {
	lm.instMu.Lock()
	cancel := lm.instCancel
	lm.instMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (lm *LocalManager) progressFn(label string) func(done, total int64) {
	start := time.Now()
	var startDone int64 = -1
	return func(done, total int64) {
		if startDone < 0 {
			startDone = done
		}
		speed := 0.0
		if el := time.Since(start).Seconds(); el > 0.5 {
			speed = float64(done-startDone) / el
		}
		lm.setInstall(func(s *InstallState) {
			s.Label = label
			s.Done = done
			s.Total = total
			s.Speed = speed
		})
	}
}

func (lm *LocalManager) install(ctx context.Context, req InstallRequest) error {
	variant := engineVariant(req.GPU)

	// 1) Motor
	if _, err := lm.engineManifest(variant); err != nil {
		lm.setInstall(func(s *InstallState) {
			s.Stage = "engine"
			s.Label = "Procurando a versão mais recente do motor (llama.cpp)…"
			s.Done, s.Total = 0, 0
		})
		if err := lm.installEngine(ctx, variant); err != nil {
			return fmt.Errorf("motor de IA: %w", err)
		}
	}

	// 2) Modelo
	var info LocalModelInfo
	if req.ModelID != "" && req.ModelID != "custom" {
		var ok bool
		info, ok = lm.app.catalog.LocalModel(req.ModelID)
		if !ok {
			return errors.New("modelo desconhecido")
		}
	} else {
		repo := strings.TrimSpace(req.Repo)
		repo = strings.TrimPrefix(repo, "https://huggingface.co/")
		repo = strings.Trim(repo, "/")
		if !regexp.MustCompile(`^[\w.-]+/[\w.-]+$`).MatchString(repo) {
			return errors.New("informe o repositório no formato usuario/nome-do-modelo-GGUF")
		}
		info = LocalModelInfo{ID: "custom:" + repo, Name: repo, Repo: repo, Quants: []string{"Q4_K_M", "Q4_K_S", "Q4_0", "IQ4_XS", "Q5_K_M", "Q8_0"}}
	}

	installed := lm.installedModels()
	model, have := installed[info.ID]
	if !have {
		lm.setInstall(func(s *InstallState) {
			s.Stage = "model"
			s.Label = "Consultando o modelo no Hugging Face…"
			s.Done, s.Total = 0, 0
		})
		file, size, sha, err := resolveModelFile(ctx, info, req.File)
		if err != nil {
			return err
		}
		if err := checkFreeSpace(lm.modelsDir(), size); err != nil {
			return err
		}
		url := hfBase + "/" + info.Repo + "/resolve/main/" + file + "?download=true"
		dest := filepath.Join(lm.modelsDir(), filepath.Base(file))
		label := fmt.Sprintf("Baixando o modelo %s (%s)", info.Name, humanBytes(size))
		lm.setInstall(func(s *InstallState) { s.Stage = "model"; s.Label = label; s.Total = size })
		if err := downloadFile(ctx, url, dest, size, lm.progressFn(label)); err != nil {
			return fmt.Errorf("download do modelo: %w", err)
		}
		if sha != "" {
			lm.setInstall(func(s *InstallState) {
				s.Stage = "verify"
				s.Label = "Conferindo a integridade do arquivo…"
				s.Done = 0
			})
			got, err := sha256File(ctx, dest, lm.progressFn("Conferindo a integridade do arquivo…"))
			if err != nil {
				return err
			}
			if !strings.EqualFold(got, sha) {
				_ = os.Remove(dest)
				return errors.New("o arquivo do modelo veio corrompido (hash diferente). Clique em baixar de novo")
			}
		}
		model = InstalledModel{ID: info.ID, Name: info.Name, Repo: info.Repo, File: filepath.Base(file), Size: size, SHA256: sha}
		installed[info.ID] = model
		if err := lm.saveInstalledModels(installed); err != nil {
			return err
		}
	}

	// 3) Passa a usar o modelo local.
	return lm.app.config.Update(func(c *Config) {
		c.Mode = "local"
		c.Local.ModelID = model.ID
		c.Local.Name = model.Name
		c.Local.Repo = model.Repo
		c.Local.File = model.File
		c.Local.GPU = req.GPU && gpuSupported()
	})
}

// UseInstalled ativa um modelo que já foi baixado antes.
func (lm *LocalManager) UseInstalled(modelID string, gpu bool) error {
	m, ok := lm.installedModels()[modelID]
	if !ok {
		return errors.New("este modelo não está baixado")
	}
	if _, err := lm.engineManifest(engineVariant(gpu)); err != nil {
		return errors.New("o motor para esta opção ainda não foi baixado")
	}
	return lm.app.config.Update(func(c *Config) {
		c.Mode = "local"
		c.Local.ModelID = m.ID
		c.Local.Name = m.Name
		c.Local.Repo = m.Repo
		c.Local.File = m.File
		c.Local.GPU = gpu && gpuSupported()
	})
}

func (lm *LocalManager) DeleteModel(modelID string) error {
	installed := lm.installedModels()
	m, ok := installed[modelID]
	if !ok {
		return errors.New("modelo não encontrado")
	}
	path := filepath.Join(lm.modelsDir(), m.File)
	lm.mu.Lock()
	inUse := lm.modelPath == path
	lm.mu.Unlock()
	if inUse {
		lm.Stop()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(installed, modelID)
	return lm.saveInstalledModels(installed)
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"` // "sha256:..." (quando o GitHub informa)
	} `json:"assets"`
}

func (lm *LocalManager) installEngine(ctx context.Context, variant string) error {
	eng := lm.app.catalog.Get().Engine
	key := platformKey(variant)
	pattern, ok := eng.Assets[key]
	if !ok {
		return fmt.Errorf("a IA local ainda não tem versão pronta para %s/%s; use uma API ou o Ollama", runtime.GOOS, runtime.GOARCH)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}

	tag, assetName, assetURL, size, sha, apiErr := findEngineAsset(ctx, eng.ReleasesAPI, re)
	if apiErr != nil {
		log.Printf("API do GitHub falhou (%v); usando versão reserva %s", apiErr, eng.FallbackTag)
		tmpl, ok := eng.FallbackAssets[key]
		if !ok || eng.FallbackTag == "" {
			return apiErr
		}
		tag = eng.FallbackTag
		assetName = strings.ReplaceAll(tmpl, "{tag}", tag)
		assetURL = strings.NewReplacer("{tag}", tag, "{asset}", assetName).Replace(eng.FallbackURL)
		size = 0
		sha = ""
	}

	dlDir := filepath.Join(lm.engineRoot(), "downloads")
	archive := filepath.Join(dlDir, assetName)
	label := fmt.Sprintf("Baixando o motor de IA llama.cpp %s", tag)
	if size > 0 {
		label += " (" + humanBytes(size) + ")"
	}
	lm.setInstall(func(s *InstallState) { s.Stage = "engine"; s.Label = label; s.Done = 0; s.Total = size })
	if err := downloadFile(ctx, assetURL, archive, size, lm.progressFn(label)); err != nil {
		return err
	}
	if len(sha) == 64 {
		got, err := sha256File(ctx, archive, nil)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, sha) {
			_ = os.Remove(archive)
			return errors.New("o arquivo do motor veio corrompido (hash diferente); tente novamente")
		}
	}

	lm.setInstall(func(s *InstallState) { s.Label = "Descompactando o motor…" })
	final := filepath.Join(lm.engineRoot(), variant)
	tmp := final + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := extractArchive(archive, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("falha ao descompactar: %w", err)
	}
	serverRel, err := findServerBinary(tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	manifest := EngineManifest{Tag: tag, Variant: variant, Asset: assetName, Server: serverRel}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "engine.json"), data, 0o644); err != nil {
		return err
	}
	if lm.isUsingVariant(variant) {
		lm.Stop()
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	_ = os.Remove(archive)
	return nil
}

func (lm *LocalManager) isUsingVariant(v string) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.cmd != nil && lm.variant == v
}

func findEngineAsset(ctx context.Context, apiURL string, re *regexp.Regexp) (tag, name, url string, size int64, sha string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err = &APIError{Status: resp.StatusCode, Body: string(b)}
		return
	}
	var releases []ghRelease
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&releases); err != nil {
		return
	}
	for _, r := range releases {
		if r.Draft {
			continue
		}
		for _, a := range r.Assets {
			if re.MatchString(a.Name) {
				return r.TagName, a.Name, a.URL, a.Size, strings.TrimPrefix(a.Digest, "sha256:"), nil
			}
		}
	}
	err = errors.New("não encontrei o motor para este sistema nas versões recentes do llama.cpp")
	return
}

func findServerBinary(root string) (string, error) {
	want := "llama-server"
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), want) {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", errors.New("o pacote baixado não contém o llama-server")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(found, 0o755)
	}
	return filepath.Rel(root, found)
}

// hfBase pode ser trocado nos testes.
var hfBase = "https://huggingface.co"

type hfTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Oid  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// resolveModelFile consulta o repositório no Hugging Face e escolhe o arquivo .gguf
// com a quantização preferida (Q4_K_M etc.). Assim o catálogo não precisa fixar nomes de arquivo.
func resolveModelFile(ctx context.Context, info LocalModelInfo, explicit string) (file string, size int64, sha string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hfBase+"/api/models/"+info.Repo+"/tree/main", nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		if info.FallbackFile != "" {
			return info.FallbackFile, 0, "", nil
		}
		return "", 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", 0, "", errors.New("este repositório do Hugging Face exige login/aceite de licença; escolha outro modelo")
	}
	if resp.StatusCode == 404 {
		return "", 0, "", errors.New("repositório não encontrado no Hugging Face")
	}
	if resp.StatusCode != http.StatusOK {
		if info.FallbackFile != "" {
			return info.FallbackFile, 0, "", nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", 0, "", &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	var entries []hfTreeEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&entries); err != nil {
		return "", 0, "", err
	}
	pick := chooseGGUF(entries, info.Quants, explicit)
	if pick == nil {
		return "", 0, "", errors.New("não encontrei um arquivo .gguf compatível neste repositório")
	}
	size = pick.Size
	if pick.LFS != nil {
		if pick.LFS.Size > 0 {
			size = pick.LFS.Size
		}
		if len(pick.LFS.Oid) == 64 {
			sha = pick.LFS.Oid
		}
	}
	return pick.Path, size, sha, nil
}

var reSplitGGUF = regexp.MustCompile(`-\d{5}-of-\d{5}\.gguf$`)

func chooseGGUF(entries []hfTreeEntry, quants []string, explicit string) *hfTreeEntry {
	var candidates []*hfTreeEntry
	for i := range entries {
		e := &entries[i]
		lower := strings.ToLower(e.Path)
		if e.Type != "file" || !strings.HasSuffix(lower, ".gguf") || strings.Contains(lower, "/") {
			continue
		}
		base := strings.ToLower(filepath.Base(e.Path))
		if strings.HasPrefix(base, "mmproj") || strings.HasPrefix(base, "mtp-") || reSplitGGUF.MatchString(base) {
			continue
		}
		if explicit != "" && e.Path == explicit {
			return e
		}
		candidates = append(candidates, e)
	}
	if explicit != "" {
		return nil
	}
	for _, q := range quants {
		suffix := "-" + strings.ToLower(q) + ".gguf"
		for _, e := range candidates {
			if strings.HasSuffix(strings.ToLower(e.Path), suffix) {
				return e
			}
		}
	}
	// Sem a quantização preferida: pega o menor arquivo que não seja minúsculo demais.
	var best *hfTreeEntry
	for _, e := range candidates {
		if best == nil || e.Size < best.Size {
			best = e
		}
	}
	return best
}

func checkFreeSpace(dir string, need int64) error {
	_ = os.MkdirAll(dir, 0o755)
	free, err := diskFree(dir)
	if err != nil || need <= 0 {
		return nil
	}
	if free < uint64(need)+500<<20 {
		return fmt.Errorf("espaço insuficiente no disco: o modelo precisa de %s e há só %s livres", humanBytes(need), humanBytes(int64(free)))
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strings.Replace(fmt.Sprintf("%.1f GB", float64(n)/(1<<30)), ".", ",", 1)
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n/(1<<20))
	case n > 0:
		return fmt.Sprintf("%d KB", n/1024)
	}
	return "tamanho desconhecido"
}

// ---------------------------------------------------------------------------
// Servidor llama-server

func (lm *LocalManager) MarkBusy(b bool) {
	lm.mu.Lock()
	lm.busy = b
	lm.lastUsed = time.Now()
	lm.mu.Unlock()
}

// EnsureRunning garante que o llama-server está no ar com o modelo configurado e
// devolve o endereço dele (http://127.0.0.1:porta).
func (lm *LocalManager) EnsureRunning(ctx context.Context) (string, error) {
	cfg := lm.app.config.Get()
	if cfg.Local.File == "" {
		return "", errors.New("nenhum modelo local escolhido: abra as Configurações e baixe um modelo")
	}
	modelPath := filepath.Join(lm.modelsDir(), cfg.Local.File)
	if _, err := os.Stat(modelPath); err != nil {
		return "", errors.New("o arquivo do modelo local não foi encontrado: abra as Configurações e baixe-o de novo")
	}
	variant := engineVariant(cfg.Local.GPU)
	manifest, err := lm.engineManifest(variant)
	if err != nil {
		return "", errors.New("o motor da IA local não está instalado: abra as Configurações e clique em baixar")
	}
	serverPath := filepath.Join(lm.engineRoot(), variant, manifest.Server)

	// startMu serializa as inicializações; lm.mu protege só os campos (assim a tela de
	// status continua respondendo enquanto o modelo carrega).
	lm.startMu.Lock()
	defer lm.startMu.Unlock()

	lm.mu.Lock()
	lm.lastUsed = time.Now()
	if lm.cmd != nil && lm.ready && lm.modelPath == modelPath && lm.variant == variant {
		port := lm.port
		lm.mu.Unlock()
		return fmt.Sprintf("http://127.0.0.1:%d", port), nil
	}
	lm.stopLocked()
	lm.mu.Unlock()

	ctxSize := cfg.Local.CtxSize
	if ctxSize <= 0 {
		ctxSize = 8192
	}
	full := []string{"--no-webui", "-np", "1", "--reasoning", "off"}
	if cfg.Local.Threads > 0 {
		full = append(full, "-t", strconv.Itoa(cfg.Local.Threads))
	}
	// Se uma versão futura do llama-server não reconhecer algum parâmetro opcional,
	// tentamos de novo só com o essencial.
	for attempt, extra := range [][]string{full, nil} {
		port, err := freePort()
		if err != nil {
			return "", err
		}
		args := append([]string{"-m", modelPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-c", strconv.Itoa(ctxSize)}, extra...)
		err = lm.startServer(ctx, serverPath, args, port, modelPath, variant)
		if err == nil {
			return fmt.Sprintf("http://127.0.0.1:%d", port), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt == 0 && strings.Contains(strings.ToLower(err.Error()), "argument") {
			log.Printf("llama-server recusou parâmetros opcionais, tentando o mínimo: %v", err)
			continue
		}
		return "", err
	}
	return "", errors.New("não foi possível iniciar a IA local")
}

func (lm *LocalManager) startServer(ctx context.Context, serverPath string, args []string, port int, modelPath, variant string) error {
	logPath := filepath.Join(lm.app.dataDir, "logs", "llama-server.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(serverPath, args...)
	cmd.Dir = filepath.Dir(serverPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	prepareChild(cmd)
	log.Printf("iniciando llama-server: %s %s", serverPath, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("não foi possível iniciar o llama-server: %w", err)
	}
	afterStart(cmd)
	_ = os.WriteFile(lm.pidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(exited)
		lm.mu.Lock()
		if lm.cmd == cmd {
			lm.cmd = nil
			lm.ready = false
		}
		lm.mu.Unlock()
	}()
	lm.mu.Lock()
	lm.cmd, lm.exited, lm.port, lm.modelPath, lm.variant, lm.ready = cmd, exited, port, modelPath, variant, false
	lm.mu.Unlock()

	// Espera o modelo carregar (pode levar de segundos a alguns minutos em HD lento).
	deadline := time.Now().Add(10 * time.Minute)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("a IA local fechou ao iniciar: %s", lastLogLines(logPath, 6))
		case <-ctx.Done():
			lm.Stop()
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			lm.Stop()
			return errors.New("a IA local demorou demais para carregar o modelo")
		}
		resp, err := client.Get(healthURL)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			lm.mu.Lock()
			if lm.cmd == cmd {
				lm.ready = true
				lm.lastUsed = time.Now()
			}
			lm.mu.Unlock()
			log.Printf("llama-server pronto na porta %d", port)
			return nil
		}
	}
}

func lastLogLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "sem detalhes"
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			lines = append(lines, t)
			if len(lines) > n {
				lines = lines[1:]
			}
		}
	}
	if len(lines) == 0 {
		return "sem detalhes (veja data/logs/llama-server.log)"
	}
	return strings.Join(lines, " | ")
}

func (lm *LocalManager) Stop() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.stopLocked()
}

func (lm *LocalManager) stopLocked() {
	if lm.cmd == nil || lm.cmd.Process == nil {
		lm.cmd = nil
		lm.ready = false
		return
	}
	cmd, exited := lm.cmd, lm.exited
	terminateProcess(cmd.Process)
	select {
	case <-exited:
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
		}
	}
	lm.cmd = nil
	lm.ready = false
	_ = os.Remove(lm.pidFile())
	log.Printf("llama-server encerrado")
}

// idleWatcher libera a memória se a IA local ficar 15 minutos sem uso.
func (lm *LocalManager) idleWatcher() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-lm.app.ctx.Done():
			return
		case <-t.C:
		}
		lm.mu.Lock()
		if lm.cmd != nil && !lm.busy && time.Since(lm.lastUsed) > 15*time.Minute {
			log.Printf("IA local ociosa, liberando memória")
			lm.stopLocked()
		}
		lm.mu.Unlock()
	}
}

// killStaleServer encerra um llama-server que tenha ficado aberto de uma execução anterior
// (por exemplo, se o app foi fechado à força).
func (lm *LocalManager) killStaleServer() {
	data, err := os.ReadFile(lm.pidFile())
	if err != nil {
		return
	}
	_ = os.Remove(lm.pidFile())
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	killIfLlamaServer(pid)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
