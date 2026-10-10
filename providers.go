package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// httpClient é usado para todas as chamadas externas. Respeita HTTPS_PROXY do sistema.
// Não há timeout total porque respostas em streaming e downloads podem demorar;
// os tempos são controlados por contexto e por vigias de inatividade.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

func userAgent() string { return appName + "/" + version + " (+https://github.com/" + repoSlug + ")" }

// APIError representa uma resposta de erro HTTP de um provedor.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body, 400))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// friendlyError traduz erros técnicos em mensagens claras em português.
func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "Cancelado."
	}
	var ae *APIError
	if errors.As(err, &ae) {
		detail := extractAPIMessage(ae.Body)
		switch {
		case ae.Status == 401 || ae.Status == 403:
			return "A chave de API foi recusada (HTTP " + strconv.Itoa(ae.Status) + "). Confira se ela foi copiada inteira e se está ativa. " + detail
		case ae.Status == 404:
			return "Modelo ou endereço não encontrado (HTTP 404). Escolha outro modelo nas configurações. " + detail
		case ae.Status == 402:
			return "Sem créditos nesta conta (HTTP 402). Adicione créditos ou use um modelo/provedor gratuito. " + detail
		case ae.Status == 429:
			return "Limite de uso atingido (HTTP 429). Espere um pouco, gere menos frases por vez ou troque de provedor. " + detail
		case ae.Status >= 500:
			return "O provedor está com problemas no momento (HTTP " + strconv.Itoa(ae.Status) + "). Tente novamente em instantes. " + detail
		default:
			return "O provedor recusou o pedido (HTTP " + strconv.Itoa(ae.Status) + "). " + detail
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "A conexão demorou demais para responder. Verifique sua internet e tente de novo."
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") {
		return "Não foi possível conectar ao endereço configurado (conexão recusada). Se for Ollama/LM Studio, confira se o programa está aberto."
	}
	if strings.Contains(msg, "no such host") || strings.Contains(msg, "dial tcp") {
		return "Sem conexão com o servidor. Verifique sua internet ou o endereço configurado."
	}
	return msg
}

func extractAPIMessage(body string) string {
	var v struct {
		Error any `json:"error"`
		Msg   any `json:"message"`
	}
	if json.Unmarshal([]byte(body), &v) == nil {
		switch e := v.Error.(type) {
		case string:
			return truncate(e, 300)
		case map[string]any:
			if m, ok := e["message"].(string); ok {
				return truncate(m, 300)
			}
		}
		if m, ok := v.Msg.(string); ok {
			return truncate(m, 300)
		}
	}
	// Alguns provedores devolvem uma lista de erros (Gemini).
	var arr []struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &arr) == nil && len(arr) > 0 && arr[0].Error.Message != "" {
		return truncate(arr[0].Error.Message, 300)
	}
	return truncate(body, 200)
}

// ---------------------------------------------------------------------------
// Listagem de modelos

type ModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Free bool   `json:"free,omitempty"`
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func listModels(ctx context.Context, p ProviderInfo, baseURL, apiKey string) ([]ModelEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var url string
	if p.Kind == "anthropic" {
		url = joinURL(baseURL, "/v1/models?limit=100")
	} else {
		url = joinURL(baseURL, "/models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setAuthHeaders(req, p, apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Body: string(body)}
	}

	var payload struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("resposta inesperada ao listar modelos: %w", err)
	}
	items := payload.Data
	if len(items) == 0 {
		items = payload.Models
	}
	seen := map[string]bool{}
	var out []ModelEntry
	for _, raw := range items {
		var m struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Model       string `json:"model"`
			DisplayName string `json:"display_name"`
			Pricing     *struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		id := m.ID
		if id == "" {
			id = m.Model
		}
		if id == "" {
			id = m.Name
		}
		if p.StripPrefix != "" {
			id = strings.TrimPrefix(id, p.StripPrefix)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		e := ModelEntry{ID: id, Name: m.DisplayName}
		if e.Name == "" && m.Name != "" && m.Name != id {
			e.Name = m.Name
		}
		if strings.HasSuffix(id, ":free") {
			e.Free = true
		} else if m.Pricing != nil && isZeroPrice(m.Pricing.Prompt) && isZeroPrice(m.Pricing.Completion) && p.PreferFree {
			e.Free = true
		}
		out = append(out, e)
	}
	return out, nil
}

func isZeroPrice(s string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && f == 0
}

func setAuthHeaders(req *http.Request, p ProviderInfo, apiKey string) {
	req.Header.Set("User-Agent", userAgent())
	if p.Kind == "anthropic" {
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if p.ID == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://github.com/"+repoSlug)
		req.Header.Set("X-Title", appName)
	}
}

var (
	reDated   = regexp.MustCompile(`(-\d{4}-\d{2}-\d{2}|-\d{8}|-\d{4}|@\d{8})$`)
	reNumbers = regexp.MustCompile(`\d+(\.\d+)?`)
)

func matchesPattern(id, pattern string) bool {
	id = strings.ToLower(id)
	pattern = strings.ToLower(pattern)
	if strings.HasPrefix(pattern, "^") {
		return strings.HasPrefix(id, pattern[1:])
	}
	return strings.Contains(id, pattern)
}

// rankModels filtra modelos inúteis para esta tarefa (imagem, áudio, embeddings...)
// e ordena colocando primeiro os mais indicados. Assim não precisamos fixar nenhuma
// versão de modelo no código: a lista vem sempre atualizada do próprio provedor.
func rankModels(p ProviderInfo, models []ModelEntry) []ModelEntry {
	var kept []ModelEntry
	for _, m := range models {
		excluded := false
		for _, ex := range p.ModelExclude {
			if ex != "" && matchesPattern(m.ID, ex) {
				excluded = true
				break
			}
		}
		if !excluded {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, models...)
	}

	score := func(m ModelEntry) float64 {
		s := float64(len(p.ModelPrefer) + 1)
		for i, pref := range p.ModelPrefer {
			if matchesPattern(m.ID, pref) {
				s = float64(i)
				break
			}
		}
		id := strings.ToLower(m.ID)
		if p.PreferFree && !m.Free {
			s += 100
		}
		for _, w := range []string{"preview", "exp", "beta", "test", "latest-preview"} {
			if strings.Contains(id, w) {
				s += 0.5
				break
			}
		}
		if reDated.MatchString(id) {
			s += 0.3
		}
		return s
	}
	version := func(id string) []float64 {
		var v []float64
		for _, n := range reNumbers.FindAllString(id, -1) {
			f, _ := strconv.ParseFloat(n, 64)
			if f >= 1900 { // datas não contam como versão
				continue
			}
			v = append(v, f)
		}
		return v
	}
	sort.SliceStable(kept, func(i, j int) bool {
		si, sj := score(kept[i]), score(kept[j])
		if si != sj {
			return si < sj
		}
		vi, vj := version(kept[i].ID), version(kept[j].ID)
		for k := 0; k < len(vi) && k < len(vj); k++ {
			if vi[k] != vj[k] {
				return vi[k] > vj[k]
			}
		}
		if len(kept[i].ID) != len(kept[j].ID) {
			return len(kept[i].ID) < len(kept[j].ID)
		}
		return kept[i].ID < kept[j].ID
	})
	return kept
}

// ---------------------------------------------------------------------------
// Chat com streaming

// Modos de saída estruturada, do mais rígido ao mais simples.
const (
	schemaStrict     = "json_schema" // padrão OpenAI (response_format.json_schema)
	schemaJSONObject = "json_object" // apenas "responda JSON"
	schemaNone       = "none"        // só o prompt
	schemaLlama      = "llama"       // llama.cpp: json_object + schema (gramática)
)

type ChatRequest struct {
	Provider   ProviderInfo
	BaseURL    string
	APIKey     string
	Model      string
	System     string
	User       string
	Schema     map[string]any
	SchemaMode string
	MaxTokens  int
	IdleLimit  time.Duration // tempo máximo sem receber nada antes de desistir
}

type StreamCallbacks struct {
	OnText     func(chunk string)
	OnActivity func() // recebeu algo (ex.: raciocínio interno), mas ainda não é texto
}

// streamChat envia o pedido e devolve o texto completo da resposta.
func streamChat(parent context.Context, cr ChatRequest, cb StreamCallbacks) (string, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var (
		url  string
		body map[string]any
	)
	if cr.Provider.Kind == "anthropic" {
		url = joinURL(cr.BaseURL, "/v1/messages")
		maxTok := cr.MaxTokens
		if maxTok <= 0 {
			maxTok = 8192
		}
		body = map[string]any{
			"model":      cr.Model,
			"max_tokens": maxTok,
			"system":     cr.System,
			"messages":   []map[string]any{{"role": "user", "content": cr.User}},
			"stream":     true,
		}
	} else {
		url = joinURL(cr.BaseURL, "/chat/completions")
		body = map[string]any{
			"model": cr.Model,
			"messages": []map[string]any{
				{"role": "system", "content": cr.System},
				{"role": "user", "content": cr.User},
			},
			"stream": true,
		}
		if cr.MaxTokens > 0 {
			body["max_tokens"] = cr.MaxTokens
		}
		switch cr.SchemaMode {
		case schemaStrict:
			body["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "flashcards",
					"strict": true,
					"schema": cr.Schema,
				},
			}
		case schemaJSONObject:
			body["response_format"] = map[string]any{"type": "json_object"}
		case schemaLlama:
			body["response_format"] = map[string]any{"type": "json_object", "schema": cr.Schema}
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	setAuthHeaders(req, cr.Provider, cr.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return "", &APIError{Status: resp.StatusCode, Body: string(b)}
	}

	// Vigia de inatividade: se nada chegar por muito tempo, cancela.
	idle := cr.IdleLimit
	if idle <= 0 {
		idle = 3 * time.Minute
	}
	var idleFired atomic.Bool
	timer := time.AfterFunc(idle, func() { idleFired.Store(true); cancel() })
	defer timer.Stop()
	reader := &activityReader{r: resp.Body, onRead: func() { timer.Reset(idle) }}
	fail := func(err error) error {
		if idleFired.Load() {
			return fmt.Errorf("o modelo ficou %s sem enviar nada e a geração foi interrompida", idle.Round(time.Second))
		}
		if parent.Err() != nil {
			return parent.Err()
		}
		return err
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		// O servidor ignorou o streaming e mandou tudo de uma vez.
		b, err := io.ReadAll(io.LimitReader(reader, 16<<20))
		if err != nil {
			return "", fail(err)
		}
		text, err := parseNonStreamResponse(cr.Provider.Kind, b)
		if err == nil && cb.OnText != nil {
			cb.OnText(text)
		}
		return text, err
	}

	var full strings.Builder
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		chunk, activity, done, err := parseStreamEvent(cr.Provider.Kind, data)
		if err != nil {
			return full.String(), err
		}
		if chunk != "" {
			full.WriteString(chunk)
			if cb.OnText != nil {
				cb.OnText(chunk)
			}
		} else if activity && cb.OnActivity != nil {
			cb.OnActivity()
		}
		if done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), fail(err)
	}
	if ctx.Err() != nil {
		return full.String(), fail(ctx.Err())
	}
	return full.String(), nil
}

type activityReader struct {
	r      io.Reader
	onRead func()
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.onRead()
	}
	return n, err
}

func parseStreamEvent(kind, data string) (chunk string, activity bool, done bool, err error) {
	if kind == "anthropic" {
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return "", false, false, nil
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				return ev.Delta.Text, true, false, nil
			}
			return "", true, false, nil
		case "message_stop":
			return "", false, true, nil
		case "error":
			msg := "erro no streaming"
			status := 500
			if ev.Error != nil {
				msg = ev.Error.Message
				if ev.Error.Type == "overloaded_error" {
					status = 529
				} else if ev.Error.Type == "rate_limit_error" {
					status = 429
				}
			}
			return "", false, true, &APIError{Status: status, Body: msg}
		}
		return "", true, false, nil
	}

	var ev struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return "", false, false, nil
	}
	if len(ev.Error) > 0 && string(ev.Error) != "null" {
		return "", false, true, &APIError{Status: 500, Body: `{"error":` + string(ev.Error) + `}`}
	}
	if len(ev.Choices) == 0 {
		return "", true, false, nil
	}
	d := ev.Choices[0].Delta
	if d.Content != nil && *d.Content != "" {
		return *d.Content, true, false, nil
	}
	return "", (d.Reasoning != nil || d.ReasoningContent != nil), false, nil
}

func parseNonStreamResponse(kind string, b []byte) (string, error) {
	if kind == "anthropic" {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, c := range r.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), nil
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", errors.New("o provedor não retornou nenhuma resposta")
	}
	return r.Choices[0].Message.Content, nil
}

// isSchemaRejection diz se o erro parece ser "não suporto response_format/json_schema".
func isSchemaRejection(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status != 400 && ae.Status != 422 && ae.Status != 404 && ae.Status != 501 {
		return false
	}
	b := strings.ToLower(ae.Body)
	for _, k := range []string{"response_format", "json_schema", "schema", "structured", "json mode", "json_object", "not supported", "unsupported", "does not support"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

func isRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status == 500 || ae.Status == 502 || ae.Status == 503 || ae.Status == 504 || ae.Status == 529
	}
	return false
}
