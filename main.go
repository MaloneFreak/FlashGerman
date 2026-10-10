// FlashGerman: gerador de frases em alemão do dia a dia para o Anki,
// com IA via API (grátis ou paga) ou IA local baixada com um clique.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// version é definida na compilação (-ldflags "-X main.version=v1.0.0").
var version = "dev"

const (
	appName     = "FlashGerman"
	repoSlug    = "MaloneFreak/FlashGerman"
	defaultPort = 47321
)

type instanceInfo struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func main() {
	var (
		flagPort     = flag.Int("port", defaultPort, "porta local preferida")
		flagNoWindow = flag.Bool("no-window", false, "não abrir janela; só iniciar o servidor e mostrar o endereço")
		flagDataDir  = flag.String("data-dir", "", "pasta de dados (padrão: pasta \"data\" ao lado do executável)")
		flagVersion  = flag.Bool("version", false, "mostrar a versão e sair")
	)
	flag.Parse()
	if *flagVersion {
		fmt.Println(appName, version)
		return
	}

	dataDir, err := resolveDataDir(*flagDataDir)
	if err != nil {
		showFatal(err.Error())
		os.Exit(1)
	}
	setupLogging(dataDir)
	log.Printf("%s %s iniciando (dados em %s)", appName, version, dataDir)

	// Já está aberto? Então só pede para a instância existente abrir uma janela.
	instPath := filepath.Join(dataDir, "instance.json")
	if inst := readInstance(instPath); inst != nil && signalInstance(inst, *flagNoWindow) {
		log.Printf("já existe uma instância aberta (porta %d)", inst.Port)
		if *flagNoWindow {
			fmt.Printf("%s já está rodando em http://127.0.0.1:%d/\n", appName, inst.Port)
		}
		return
	}

	app, err := newApp(dataDir)
	if err != nil {
		showFatal(err.Error())
		os.Exit(1)
	}
	app.noWindow = *flagNoWindow

	ln, port, err := listenLocal(*flagPort)
	if err != nil {
		showFatal("Não foi possível iniciar o servidor local: " + err.Error())
		os.Exit(1)
	}
	app.port = port
	writeInstance(instPath, instanceInfo{PID: os.Getpid(), Port: port, Token: app.token})
	defer os.Remove(instPath)

	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("servidor parou: %v", err)
			app.life.Shutdown("erro no servidor")
		}
	}()

	log.Printf("interface em %s", app.url())
	if *flagNoWindow {
		fmt.Printf("%s rodando em %s\nPressione Ctrl+C para encerrar.\n", appName, app.url())
	} else {
		app.openWindow()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("sinal recebido: %v", s)
	case <-app.life.Done():
		log.Printf("encerrando: %s", app.life.Reason)
	}

	app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("até logo!")
}

func readInstance(path string) *instanceInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var inst instanceInfo
	if json.Unmarshal(data, &inst) != nil || inst.Port == 0 {
		return nil
	}
	return &inst
}

func writeInstance(path string, inst instanceInfo) {
	data, _ := json.Marshal(inst)
	_ = writeFileAtomic(path, data, 0o600)
}

// signalInstance confirma que a outra instância está viva e pede uma nova janela.
func signalInstance(inst *instanceInfo, noWindow bool) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://127.0.0.1:" + strconv.Itoa(inst.Port)
	req, _ := http.NewRequest(http.MethodGet, base+"/api/ping", nil)
	req.Header.Set("X-FG-Token", inst.Token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if !noWindow {
		req, _ = http.NewRequest(http.MethodPost, base+"/api/window", nil)
		req.Header.Set("X-FG-Token", inst.Token)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return true
}
