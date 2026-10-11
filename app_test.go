package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain permite que o próprio binário de teste finja ser o llama-server
// (usado no teste da IA local, sem precisar baixar nada).
func TestMain(m *testing.M) {
	if os.Getenv("FG_FAKE_LLAMA") == "1" {
		runFakeLlamaServer()
		return
	}
	offlineMode = true // os testes não dependem da internet
	os.Exit(m.Run())
}

const sampleJSON = `{"frases":[
 {"german_phrase":"Ich hätte gern ein Brötchen.","pt_translation":"Eu queria um pãozinho.","literal_translation":"Eu teria com prazer um pãozinho.","tense":"Konjunktiv II","grammar_note":"hätte gern = forma educada de pedir.","pronunciation_tip":"ich RÉ-te guérn","context_dialogue":"A: Was darf's sein? B: Ich hätte gern ein Brötchen."},
 {"german_phrase":"Kann ich mit Karte zahlen?","pt_translation":"Posso pagar com cartão?","literal_translation":"","tense":"Präsens","grammar_note":"Verbo modal + infinitivo no fim.","pronunciation_tip":"","context_dialogue":"A: Kann ich mit Karte zahlen? B: Ja, klar."}
]}`

func TestParseCardsVariants(t *testing.T) {
	cases := map[string]string{
		"puro":      sampleJSON,
		"cercas":    "Claro! Aqui está:\n```json\n" + sampleJSON + "\n```\nBom estudo!",
		"think":     "<think>vou pensar...</think>" + sampleJSON,
		"lista":     `[{"german":"Ich hätte gern ein Brötchen.","translation":"Eu queria um pãozinho."},{"german_phrase":"Kann ich mit Karte zahlen?","pt_translation":"Posso pagar com cartão?"}]`,
		"cortado":   sampleJSON[:strings.LastIndex(sampleJSON, `{"german_phrase":"Kann`)] + `{"german_phrase":"Kann ich`,
		"outra-chv": strings.Replace(sampleJSON, `"frases"`, `"phrases"`, 1),
	}
	for name, input := range cases {
		cards, err := parseCards(input)
		if err != nil {
			t.Fatalf("%s: erro inesperado: %v", name, err)
		}
		if cards[0].German != "Ich hätte gern ein Brötchen." || cards[0].PT == "" {
			t.Fatalf("%s: primeiro cartão errado: %+v", name, cards[0])
		}
		want := 2
		if name == "cortado" {
			want = 1
		}
		if len(cards) != want {
			t.Fatalf("%s: esperava %d cartões, veio %d", name, want, len(cards))
		}
	}
	if _, err := parseCards("Desculpe, não posso ajudar."); err == nil {
		t.Fatal("texto sem JSON deveria falhar")
	}
}

func TestRankModels(t *testing.T) {
	cat := newCatalogStore(t.TempDir())
	get := func(id string) ProviderInfo { p, _ := cat.Provider(id); return p }

	gem := rankModels(get("gemini"), []ModelEntry{
		{ID: "gemini-2.5-flash"}, {ID: "gemini-3.8-flash-lite"}, {ID: "gemini-3.8-flash"},
		{ID: "gemini-3.1-pro-preview"}, {ID: "gemini-embedding-001"}, {ID: "gemini-3.8-flash-image"}, {ID: "imagen-4"},
	})
	if gem[0].ID != "gemini-3.8-flash" {
		t.Fatalf("gemini: sugeriu %q", gem[0].ID)
	}
	for _, m := range gem {
		if strings.Contains(m.ID, "embedding") || strings.Contains(m.ID, "image") {
			t.Fatalf("gemini: deveria esconder %q", m.ID)
		}
	}

	oai := rankModels(get("openai"), []ModelEntry{
		{ID: "gpt-4o-mini"}, {ID: "gpt-5"}, {ID: "gpt-5-mini-2025-08-07"}, {ID: "gpt-5-mini"}, {ID: "o3"}, {ID: "gpt-realtime"}, {ID: "text-embedding-3-small"},
	})
	if oai[0].ID != "gpt-5-mini" {
		t.Fatalf("openai: sugeriu %q", oai[0].ID)
	}

	or := rankModels(get("openrouter"), []ModelEntry{
		{ID: "openai/gpt-5"}, {ID: "google/gemma-4-12b-it:free", Free: true}, {ID: "meta-llama/llama-4-scout:free", Free: true},
	})
	if !or[0].Free || !strings.Contains(or[0].ID, "gemma") {
		t.Fatalf("openrouter: sugeriu %q", or[0].ID)
	}
}

// Servidor falso no formato OpenAI que recusa json_schema (como alguns provedores fazem)
// e responde em streaming quando o pedido vem sem ele.
func fakeOpenAI(t *testing.T, rejectSchema bool, modes *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"modelo-bom"},{"id":"modelo-embed"}]}`)
			return
		case "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer chave-teste" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mode := "none"
		if rf, ok := body["response_format"].(map[string]any); ok {
			mode, _ = rf["type"].(string)
		}
		if modes != nil {
			*modes = append(*modes, mode)
		}
		if rejectSchema && mode == "json_schema" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"response_format json_schema is not supported for this model"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		text := sampleJSON
		for i := 0; i < len(text); i += 40 {
			end := min(i+40, len(text))
			chunk, _ := json.Marshal(text[i:end])
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", chunk)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	app, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func waitJob(t *testing.T, j *Job) JobView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v := j.View()
		if v.Status != "running" {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job não terminou")
	return JobView{}
}

func TestGenerateViaAPIWithSchemaFallback(t *testing.T) {
	var modes []string
	srv := fakeOpenAI(t, true, &modes)
	defer srv.Close()
	app := newTestApp(t)
	_ = app.config.Update(func(c *Config) {
		c.Mode = "api"
		c.ActiveProvider = "custom"
		c.Providers["custom"] = &ProviderSettings{BaseURL: srv.URL + "/v1", APIKey: "chave-teste", Model: "modelo-bom"}
	})
	if !app.configured(app.config.Get()) {
		t.Fatal("deveria estar configurado")
	}
	p := GenerateParams{Theme: "Supermercado", Tense: "mixed", Level: "A1-A2", Count: 2, Colloquial: true, Pronunciation: true}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	j, err := app.jobs.Start(app, p)
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j)
	if v.Status != "done" || len(v.Cards) != 2 {
		t.Fatalf("resultado inesperado: %+v", v)
	}
	if strings.Join(modes, ",") != "json_schema,json_object" {
		t.Fatalf("sequência de modos inesperada: %v", modes)
	}
}

func TestGenerateWrongKeyGivesFriendlyError(t *testing.T) {
	srv := fakeOpenAI(t, false, nil)
	defer srv.Close()
	app := newTestApp(t)
	_ = app.config.Update(func(c *Config) {
		c.Mode = "api"
		c.ActiveProvider = "custom"
		c.Providers["custom"] = &ProviderSettings{BaseURL: srv.URL + "/v1", APIKey: "errada", Model: "modelo-bom"}
	})
	j, _ := app.jobs.Start(app, GenerateParams{Theme: "x", Count: 2})
	v := waitJob(t, j)
	if v.Status != "error" || !strings.Contains(v.Error, "chave de API foi recusada") {
		t.Fatalf("erro inesperado: %+v", v)
	}
}

func TestHTTPFlowListAndSave(t *testing.T) {
	srv := fakeOpenAI(t, false, nil)
	defer srv.Close()
	app := newTestApp(t)
	app.port = 4242
	h := app.routes()

	call := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:4242"
		req.Header.Set("X-FG-Token", app.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := call("POST", "/api/providers/models", `{"provider_id":"custom","base_url":"`+srv.URL+`/v1","api_key":"chave-teste"}`)
	if code != 200 || out["suggested"] != "modelo-bom" {
		t.Fatalf("listar modelos: %d %v", code, out)
	}
	code, out = call("POST", "/api/providers/save", `{"provider_id":"custom","base_url":"`+srv.URL+`/v1","api_key":"chave-teste","model":"modelo-bom"}`)
	if code != 200 {
		t.Fatalf("salvar: %d %v", code, out)
	}
	code, out = call("GET", "/api/state", "")
	if code != 200 || out["configured"] != true {
		t.Fatalf("estado: %d %v", code, out)
	}
	// A chave nunca volta para a interface.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "chave-teste") {
		t.Fatal("a chave de API vazou para a interface")
	}

	// Sem token: recusado.
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:4242"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("sem token deveria dar 401, deu %d", rec.Code)
	}
	// Host estranho (DNS rebinding): recusado.
	req = httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.example:4242"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("host estranho deveria dar 403, deu %d", rec.Code)
	}

	// Baralho: salvar e ler.
	code, _ = call("PUT", "/api/deck", `[{"id":"1","german":"Hallo","pt":"Olá"}]`)
	if code != 200 {
		t.Fatalf("salvar baralho: %d", code)
	}
	req = httptest.NewRequest("GET", "/api/deck", nil)
	req.Host = "127.0.0.1:4242"
	req.Header.Set("X-FG-Token", app.token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Hallo") {
		t.Fatalf("baralho não persistiu: %s", rec.Body.String())
	}
}

func TestAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		for _, part := range []string{sampleJSON[:100], sampleJSON[100:]} {
			b, _ := json.Marshal(part)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", b)
		}
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	text, err := streamChat(context.Background(), ChatRequest{
		Provider: ProviderInfo{Kind: "anthropic"}, BaseURL: srv.URL, APIKey: "k", Model: "claude-x", System: "s", User: "u",
	}, StreamCallbacks{})
	if err != nil || text != sampleJSON {
		t.Fatalf("anthropic: err=%v texto=%q", err, text)
	}
}

func TestDownloadResumeAndHash(t *testing.T) {
	payload := bytes.Repeat([]byte("FlashGerman-"), 100000)
	sum := sha256.Sum256(payload)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "m.gguf", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "m.gguf")
	// Simula um download interrompido pela metade.
	if err := os.WriteFile(dest+".part", payload[:len(payload)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := downloadFile(context.Background(), srv.URL+"/m.gguf", dest, int64(len(payload)), nil); err != nil {
		t.Fatal(err)
	}
	got, err := sha256File(context.Background(), dest, nil)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash diferente: %v", err)
	}
}

func TestExtractArchivesAndBlockZipSlip(t *testing.T) {
	dir := t.TempDir()
	// zip válido
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("llama-server.exe")
	f.Write([]byte("bin"))
	zw.Close()
	zpath := filepath.Join(dir, "a.zip")
	os.WriteFile(zpath, zb.Bytes(), 0o644)
	if err := extractArchive(zpath, filepath.Join(dir, "z")); err != nil {
		t.Fatal(err)
	}
	// zip malicioso
	zb.Reset()
	zw = zip.NewWriter(&zb)
	f, _ = zw.Create("../../fora.txt")
	f.Write([]byte("x"))
	zw.Close()
	os.WriteFile(zpath, zb.Bytes(), 0o644)
	if err := extractArchive(zpath, filepath.Join(dir, "z2")); err == nil {
		t.Fatal("zip-slip deveria ser bloqueado")
	}
	// tar.gz com pasta
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "llama-b1/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "llama-b1/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3})
	tw.Write([]byte("bin"))
	tw.Close()
	gz.Close()
	tpath := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(tpath, tb.Bytes(), 0o644)
	out := filepath.Join(dir, "t")
	if err := extractArchive(tpath, out); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		rel, err := findServerBinary(out)
		if err != nil || rel != filepath.Join("llama-b1", "llama-server") {
			t.Fatalf("findServerBinary: %q %v", rel, err)
		}
	}
}

func TestChooseGGUF(t *testing.T) {
	entries := []hfTreeEntry{
		{Type: "file", Path: "README.md"},
		{Type: "file", Path: "mmproj-BF16.gguf", Size: 10},
		{Type: "file", Path: "m-BF16.gguf", Size: 9000},
		{Type: "file", Path: "m-Q8_0.gguf", Size: 5000},
		{Type: "file", Path: "m-Q4_K_M.gguf", Size: 3000},
		{Type: "file", Path: "m-UD-Q4_K_XL.gguf", Size: 3100},
		{Type: "directory", Path: "MTP"},
	}
	if got := chooseGGUF(entries, []string{"Q4_K_M", "Q4_0"}, ""); got == nil || got.Path != "m-Q4_K_M.gguf" {
		t.Fatalf("escolheu %+v", got)
	}
	if got := chooseGGUF(entries, []string{"Q4_K_M"}, "m-Q8_0.gguf"); got == nil || got.Path != "m-Q8_0.gguf" {
		t.Fatalf("arquivo explícito ignorado: %+v", got)
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("v1.2.0", "v1.10.0") >= 0 || compareVersions("v2.0.0", "1.9.9") <= 0 || compareVersions("v1.0.0", "v1.0.0") != 0 {
		t.Fatal("comparação de versões errada")
	}
}

// ---------------------------------------------------------------------------
// IA local com um llama-server falso

func runFakeLlamaServer() {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rf, _ := body["response_format"].(map[string]any)
		schema, _ := rf["schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		frases, _ := props["frases"].(map[string]any)
		if frases["minItems"] == nil {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"faltou o schema com minItems"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(sampleJSON)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\ndata: [DONE]\n\n", b)
	})
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
}

func TestLocalEngineLifecycle(t *testing.T) {
	app := newTestApp(t)
	lm := app.local

	// "Instala" o motor falso (o próprio binário de teste) e um modelo falso.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engDir := filepath.Join(lm.engineRoot(), "cpu")
	os.MkdirAll(engDir, 0o755)
	name := "llama-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data, _ := os.ReadFile(self)
	os.WriteFile(filepath.Join(engDir, name), data, 0o755)
	man, _ := json.Marshal(EngineManifest{Tag: "b0", Variant: "cpu", Server: name})
	os.WriteFile(filepath.Join(engDir, "engine.json"), man, 0o644)
	os.MkdirAll(lm.modelsDir(), 0o755)
	os.WriteFile(filepath.Join(lm.modelsDir(), "fake.gguf"), []byte("GGUF"), 0o644)
	lm.saveInstalledModels(map[string]InstalledModel{"fake": {ID: "fake", Name: "Fake", File: "fake.gguf", Size: 4}})

	t.Setenv("FG_FAKE_LLAMA", "1")
	if err := lm.UseInstalled("fake", false); err != nil {
		t.Fatal(err)
	}
	if !app.configured(app.config.Get()) {
		t.Fatal("IA local deveria estar configurada")
	}
	j, err := app.jobs.Start(app, GenerateParams{Theme: "Bahn", Tense: "perfekt", Level: "B1-B2", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j)
	if v.Status != "done" || len(v.Cards) != 2 {
		t.Fatalf("geração local falhou: %+v (log: %s)", v, lastLogLines(filepath.Join(app.dataDir, "logs", "llama-server.log"), 5))
	}
	st := lm.Status()
	if st.Server["ready"] != true {
		t.Fatalf("servidor deveria estar pronto: %+v", st.Server)
	}
	lm.Stop()
	if lm.Status().Server["running"] != false {
		t.Fatal("servidor deveria ter parado")
	}
}

func TestLifecycleClosesAfterBye(t *testing.T) {
	l := newLifecycle()
	l.Heartbeat("a")
	l.Bye("a")
	l.mu.Lock()
	l.emptySince = time.Now().Add(-time.Minute)
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.run(ctx)
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("deveria encerrar depois que a janela fecha")
	}
}

var _ = io.Discard

// Instalação "com um clique" de ponta a ponta, com GitHub e Hugging Face falsos.
func TestOneClickInstall(t *testing.T) {
	// Pacote do motor (tar.gz com pasta, como o do llama.cpp para Linux/macOS).
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	bin := "llama-server"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	tw.WriteHeader(&tar.Header{Name: "llama-b9/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "llama-b9/" + bin, Typeflag: tar.TypeReg, Mode: 0o755, Size: 3})
	tw.Write([]byte("bin"))
	tw.Close()
	gz.Close()
	engine := tb.Bytes()
	engineSum := sha256.Sum256(engine)
	model := bytes.Repeat([]byte("GGUF"), 50000)
	modelSum := sha256.Sum256(model)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases":
			fmt.Fprintf(w, `[{"tag_name":"b9","prerelease":true,"assets":[
				{"name":"llama-b9-bin-win-cpu-x64.zip","browser_download_url":"%[1]s/x","size":1},
				{"name":"llama-b9-bin-ubuntu-x64.tar.gz","browser_download_url":"%[1]s/engine.tar.gz","size":%[2]d,"digest":"sha256:%[3]s"},
				{"name":"llama-b9-bin-macos-arm64.tar.gz","browser_download_url":"%[1]s/engine.tar.gz","size":%[2]d,"digest":"sha256:%[3]s"},
				{"name":"llama-b9-bin-macos-x64.tar.gz","browser_download_url":"%[1]s/engine.tar.gz","size":%[2]d,"digest":"sha256:%[3]s"},
				{"name":"llama-b9-bin-ubuntu-arm64.tar.gz","browser_download_url":"%[1]s/engine.tar.gz","size":%[2]d,"digest":"sha256:%[3]s"}]}]`,
				srv.URL, len(engine), hex.EncodeToString(engineSum[:]))
		case r.URL.Path == "/engine.tar.gz":
			http.ServeContent(w, r, "e.tar.gz", time.Now(), bytes.NewReader(engine))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			fmt.Fprintf(w, `[{"type":"file","path":"mmproj-F16.gguf","size":10},{"type":"file","path":"tiny-Q4_K_M.gguf","size":%d,"lfs":{"oid":"%s","size":%d}}]`,
				len(model), hex.EncodeToString(modelSum[:]), len(model))
		case strings.HasSuffix(r.URL.Path, "/resolve/main/tiny-Q4_K_M.gguf"):
			http.ServeContent(w, r, "m.gguf", time.Now(), bytes.NewReader(model))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := hfBase
	hfBase = srv.URL
	defer func() { hfBase = old }()

	app := newTestApp(t)
	app.catalog.mu.Lock()
	cat := *app.catalog.cat
	cat.Engine.ReleasesAPI = srv.URL + "/releases"
	cat.LocalModels = []LocalModelInfo{{ID: "tiny", Name: "Tiny", Repo: "teste/tiny-GGUF", Quants: []string{"Q4_K_M"}}}
	app.catalog.cat = &cat
	app.catalog.mu.Unlock()

	if runtime.GOOS == "windows" {
		t.Skip("o pacote falso de teste é tar.gz (formato de Linux/macOS)")
	}
	if err := app.local.StartInstall(InstallRequest{ModelID: "tiny"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var st LocalStatus
	for time.Now().Before(deadline) {
		st = app.local.Status()
		if !st.Install.Active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Install.Stage != "done" {
		t.Fatalf("instalação não terminou bem: %+v", st.Install)
	}
	cfg := app.config.Get()
	if cfg.Mode != "local" || cfg.Local.File != "tiny-Q4_K_M.gguf" || !st.Engine.Installed || st.Engine.Tag != "b9" {
		t.Fatalf("estado final inesperado: cfg=%+v engine=%+v", cfg.Local, st.Engine)
	}
	if !app.configured(cfg) {
		t.Fatal("app deveria estar pronto para gerar")
	}
}

func TestDefaultBrowserParsing(t *testing.T) {
	cases := map[string]string{
		"ChromeHTML": "chrome", "MSEdgeHTM": "edge", "BraveHTML": "brave", "FirefoxURL-308046B0AF4A39CB": "other",
		"com.google.chrome": "chrome", "com.microsoft.edgemac": "edge", "com.apple.safari": "other",
		"google-chrome.desktop": "chrome", "chromium-browser.desktop": "chromium", "firefox.desktop": "other", "": "",
	}
	for in, want := range cases {
		if got := browserIDFromName(in); got != want {
			t.Errorf("%q: esperava %q, veio %q", in, want, got)
		}
	}
	plist := `(
        {
        LSHandlerContentType = "public.html";
        LSHandlerRoleAll = "com.apple.safari";
    },
        {
        LSHandlerPreferredVersions =         {
            LSHandlerRoleAll = "-";
        };
        LSHandlerRoleAll = "com.google.chrome";
        LSHandlerURLScheme = https;
    },
        {
        LSHandlerRoleAll = "com.google.chrome";
        LSHandlerURLScheme = http;
    }
)`
	if got := parseLSHandlers(plist); got != "com.google.chrome" {
		t.Fatalf("macOS: esperava com.google.chrome, veio %q", got)
	}
	if got := parseLSHandlers("(\n)"); got != "com.apple.safari" {
		t.Fatalf("macOS sem registro deveria ser Safari, veio %q", got)
	}
	if !isTempDir(filepath.Join(os.TempDir(), "Temp1_FlashGerman.zip", "FlashGerman")) {
		t.Fatal("pasta temporária não detectada")
	}
	if home, err := os.UserHomeDir(); err == nil && isTempDir(filepath.Join(home, "Downloads", "FlashGerman")) {
		t.Fatal("pasta comum detectada como temporária")
	}
}
