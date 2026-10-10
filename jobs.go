package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

// Job é uma geração de frases em andamento. A interface consulta o progresso a cada segundo.
type Job struct {
	mu        sync.Mutex
	ID        string
	Status    string // running, done, error, cancelled
	Phase     string // preparing, loading_model, waiting, thinking, generating, parsing
	Received  int    // caracteres recebidos
	Found     int    // frases detectadas até agora
	Expected  int
	Cards     []Card
	Error     string
	Notice    string
	StartedAt time.Time
	EndedAt   time.Time
	cancel    context.CancelFunc
}

type JobView struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Phase    string `json:"phase"`
	Received int    `json:"received"`
	Found    int    `json:"found"`
	Expected int    `json:"expected"`
	Cards    []Card `json:"cards,omitempty"`
	Error    string `json:"error,omitempty"`
	Notice   string `json:"notice,omitempty"`
	Elapsed  int    `json:"elapsed_ms"`
}

func (j *Job) View() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := j.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return JobView{
		ID: j.ID, Status: j.Status, Phase: j.Phase, Received: j.Received, Found: j.Found,
		Expected: j.Expected, Cards: j.Cards, Error: j.Error, Notice: j.Notice,
		Elapsed: int(end.Sub(j.StartedAt).Milliseconds()),
	}
}

func (j *Job) set(fn func(j *Job)) {
	j.mu.Lock()
	fn(j)
	j.mu.Unlock()
}

type JobManager struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobManager() *JobManager { return &JobManager{jobs: map[string]*Job{}} }

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *JobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *JobManager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.mu.Lock()
		running := j.Status == "running"
		j.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

func (m *JobManager) Start(app *App, p GenerateParams) (*Job, error) {
	if m.Active() {
		return nil, errors.New("já existe uma geração em andamento")
	}
	ctx, cancel := context.WithCancel(app.ctx)
	j := &Job{ID: randomID(8), Status: "running", Phase: "preparing", Expected: p.Count, StartedAt: time.Now(), cancel: cancel}
	m.mu.Lock()
	// Limpa jobs antigos.
	for id, old := range m.jobs {
		old.mu.Lock()
		stale := old.Status != "running" && time.Since(old.EndedAt) > 30*time.Minute
		old.mu.Unlock()
		if stale {
			delete(m.jobs, id)
		}
	}
	m.jobs[j.ID] = j
	m.mu.Unlock()

	go func() {
		defer cancel()
		cards, notice, err := app.generate(ctx, j, p)
		j.set(func(j *Job) {
			j.EndedAt = time.Now()
			j.Notice = notice
			switch {
			case err != nil && errors.Is(err, context.Canceled):
				j.Status = "cancelled"
				j.Error = "Geração cancelada."
			case err != nil:
				j.Status = "error"
				j.Error = friendlyError(err)
				log.Printf("geração falhou: %v", err)
			default:
				j.Status = "done"
				j.Cards = cards
				j.Found = len(cards)
			}
		})
	}()
	return j, nil
}

func (m *JobManager) Cancel(id string) bool {
	j := m.Get(id)
	if j == nil {
		return false
	}
	j.cancel()
	return true
}

func (m *JobManager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.cancel()
	}
}

// generate escolhe o motor (API ou local), envia o pedido com as melhores opções que o
// provedor aceitar e transforma a resposta em cartões.
func (app *App) generate(ctx context.Context, j *Job, p GenerateParams) ([]Card, string, error) {
	cfg := app.config.Get()
	system, user := buildPrompts(p)

	var (
		base  ChatRequest
		modes []string
	)
	switch cfg.Mode {
	case "local":
		j.set(func(j *Job) { j.Phase = "loading_model" })
		url, err := app.local.EnsureRunning(ctx)
		if err != nil {
			return nil, "", err
		}
		app.local.MarkBusy(true)
		defer app.local.MarkBusy(false)
		base = ChatRequest{
			Provider:  ProviderInfo{ID: "local", Kind: "openai"},
			BaseURL:   url + "/v1",
			Model:     "local",
			IdleLimit: 6 * time.Minute, // CPUs lentas podem demorar para ler o prompt
		}
		modes = []string{schemaLlama, schemaNone}
	case "api":
		prov, ok := app.catalog.Provider(cfg.ActiveProvider)
		if !ok {
			return nil, "", errors.New("provedor não configurado: abra as Configurações")
		}
		ps := cfg.Providers[cfg.ActiveProvider]
		if ps == nil || ps.Model == "" {
			return nil, "", errors.New("escolha um modelo nas Configurações")
		}
		baseURL := prov.BaseURL
		if ps.BaseURL != "" && (prov.EditableURL || prov.ID == "custom") {
			baseURL = ps.BaseURL
		}
		if baseURL == "" {
			return nil, "", errors.New("informe o endereço da API nas Configurações")
		}
		base = ChatRequest{Provider: prov, BaseURL: baseURL, APIKey: ps.APIKey, Model: ps.Model, IdleLimit: 3 * time.Minute}
		if prov.Kind == "anthropic" {
			modes = []string{schemaNone}
		} else if prov.Pricing == "local" {
			modes = []string{schemaJSONObject, schemaNone}
			base.IdleLimit = 6 * time.Minute
		} else {
			modes = []string{schemaStrict, schemaJSONObject, schemaNone}
		}
	default:
		return nil, "", errors.New("o app ainda não foi configurado: escolha uma API ou baixe a IA local")
	}

	base.System = system
	base.User = user
	exact := cfg.Mode == "local"
	base.Schema = buildSchema(p.Count, exact)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		text, err := app.runWithFallback(ctx, j, base, modes)
		if err != nil {
			return nil, "", err
		}
		j.set(func(j *Job) { j.Phase = "parsing" })
		cards, perr := parseCards(text)
		if perr == nil {
			notice := ""
			if len(cards) < p.Count {
				notice = "O modelo devolveu menos frases do que o pedido."
			}
			if len(cards) > p.Count {
				cards = cards[:p.Count]
			}
			return cards, notice, nil
		}
		lastErr = perr
		log.Printf("resposta inválida (tentativa %d): %v", attempt+1, perr)
		// Segunda tentativa: reforça a instrução de formato.
		base.User = user + "\n\nIMPORTANTE: sua resposta anterior não era um JSON válido. Responda APENAS com o objeto JSON {\"frases\": [...]}."
		j.set(func(j *Job) { j.Received = 0; j.Found = 0; j.Phase = "waiting" })
	}
	return nil, "", lastErr
}

// runWithFallback tenta os modos de saída estruturada em ordem; se o provedor recusar
// um modo (ex.: não suporta json_schema), passa para o próximo. Erros temporários
// (429/5xx) ganham uma nova tentativa após alguns segundos.
func (app *App) runWithFallback(ctx context.Context, j *Job, base ChatRequest, modes []string) (string, error) {
	var lastErr error
	for i := 0; i < len(modes); i++ {
		cr := base
		cr.SchemaMode = modes[i]
		for retry := 0; retry < 2; retry++ {
			j.set(func(j *Job) {
				j.Phase = "waiting"
				j.Received = 0
				j.Found = 0
			})
			var acc strings.Builder
			scanFrom := 0
			text, err := streamChat(ctx, cr, StreamCallbacks{
				OnText: func(chunk string) {
					acc.WriteString(chunk)
					s := acc.String()
					from := scanFrom - (len(`"german_phrase"`) - 1)
					if from < 0 {
						from = 0
					}
					found := strings.Count(s[from:], `"german_phrase"`)
					scanFrom = len(s)
					j.set(func(j *Job) {
						j.Phase = "generating"
						j.Received = len(s)
						j.Found += found
					})
				},
				OnActivity: func() {
					j.set(func(j *Job) {
						if j.Phase == "waiting" {
							j.Phase = "thinking"
						}
					})
				},
			})
			if err == nil {
				return text, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if isRetryable(err) && retry == 0 && acc.Len() == 0 {
				log.Printf("erro temporário (%v), tentando de novo", err)
				select {
				case <-time.After(4 * time.Second):
				case <-ctx.Done():
					return "", ctx.Err()
				}
				continue
			}
			break
		}
		if isSchemaRejection(lastErr) && i+1 < len(modes) {
			log.Printf("provedor recusou o modo %s, tentando %s: %v", modes[i], modes[i+1], lastErr)
			continue
		}
		return "", lastErr
	}
	return "", lastErr
}
