package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// O app não traz um navegador próprio (isso deixaria o download com 100+ MB).
// Em vez disso, abre a interface numa janela "modo aplicativo" (sem barra de endereço)
// do navegador baseado em Chromium que a pessoa já usa: Chrome, Edge, Brave...
//
// A janela usa o PERFIL NORMAL do navegador. Um perfil separado parecia "mais limpo",
// mas cada perfil novo é tratado como um navegador recém-instalado: abre as boas-vindas
// de todas as extensões sincronizadas e pede login de novo. Com o perfil normal, se o
// navegador já estiver aberto, ele só abre mais uma janela.
//
// Se o navegador padrão não for baseado em Chromium (ex.: Firefox), a interface abre
// numa aba normal dele.

type browserInfo struct {
	id    string // chrome, edge, brave, chromium, vivaldi, opera
	paths []string
}

func knownBrowsers() []browserInfo {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LocalAppData")
		join := func(rel string, bases ...string) []string {
			var out []string
			for _, b := range bases {
				if b != "" {
					out = append(out, filepath.Join(b, rel))
				}
			}
			return out
		}
		return []browserInfo{
			{"chrome", join(`Google\Chrome\Application\chrome.exe`, pf, pf86, local)},
			{"edge", join(`Microsoft\Edge\Application\msedge.exe`, pf86, pf, local)},
			{"brave", join(`BraveSoftware\Brave-Browser\Application\brave.exe`, pf, pf86, local)},
			{"vivaldi", join(`Vivaldi\Application\vivaldi.exe`, local, pf, pf86)},
			{"chromium", join(`Chromium\Application\chrome.exe`, local, pf, pf86)},
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		app := func(name, bin string) []string {
			return []string{
				filepath.Join("/Applications", name+".app/Contents/MacOS", bin),
				filepath.Join(home, "Applications", name+".app/Contents/MacOS", bin),
			}
		}
		return []browserInfo{
			{"chrome", app("Google Chrome", "Google Chrome")},
			{"edge", app("Microsoft Edge", "Microsoft Edge")},
			{"brave", app("Brave Browser", "Brave Browser")},
			{"vivaldi", app("Vivaldi", "Vivaldi")},
			{"chromium", app("Chromium", "Chromium")},
		}
	default:
		look := func(names ...string) []string {
			var out []string
			for _, n := range names {
				if p, err := exec.LookPath(n); err == nil {
					out = append(out, p)
				}
			}
			return out
		}
		return []browserInfo{
			{"chrome", look("google-chrome", "google-chrome-stable")},
			{"edge", look("microsoft-edge", "microsoft-edge-stable")},
			{"brave", look("brave-browser", "brave")},
			{"vivaldi", look("vivaldi", "vivaldi-stable")},
			{"chromium", look("chromium", "chromium-browser")},
		}
	}
}

// browserIDFromName traduz o identificador do navegador padrão do sistema
// (ProgId do Windows, bundle id do macOS ou .desktop do Linux) para os nomes acima.
// Devolve "other" para navegadores que não são baseados em Chromium.
func browserIDFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "":
		return ""
	case strings.Contains(n, "edge") || strings.Contains(n, "msedge"):
		return "edge"
	case strings.Contains(n, "brave"):
		return "brave"
	case strings.Contains(n, "vivaldi"):
		return "vivaldi"
	case strings.Contains(n, "chromium"):
		return "chromium"
	case strings.Contains(n, "chrome"): // ChromeHTML, com.google.chrome, google-chrome.desktop
		return "chrome"
	}
	return "other"
}

func findBrowser(id string) string {
	for _, b := range knownBrowsers() {
		if b.id != id {
			continue
		}
		for _, p := range b.paths {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// pickAppBrowser escolhe o navegador da janela. Devolve "" quando o melhor é abrir
// no navegador padrão como uma aba normal.
func pickAppBrowser() string {
	def := browserIDFromName(defaultBrowserName())
	switch def {
	case "other":
		return "" // Firefox, Safari...: respeita a escolha da pessoa
	case "":
		// Não deu para descobrir o padrão: tenta os mais comuns.
		for _, id := range []string{"chrome", "edge", "brave", "vivaldi", "chromium"} {
			if p := findBrowser(id); p != "" {
				return p
			}
		}
		return ""
	default:
		return findBrowser(def)
	}
}

// openAppWindow abre a interface. O processo do navegador não é acompanhado: se o
// navegador já estava aberto, o comando só pede a ele uma janela nova e termina na hora.
// O app percebe que a janela foi fechada pelos sinais que a própria página envia.
func openAppWindow(url string) error {
	if browser := pickAppBrowser(); browser != "" {
		cmd := exec.Command(browser, "--app="+url, "--new-window")
		if err := cmd.Start(); err == nil {
			log.Printf("janela aberta com %s", browser)
			go func() { _ = cmd.Wait() }() // evita processo zumbi
			return nil
		} else {
			log.Printf("não foi possível abrir %s: %v", browser, err)
		}
	}
	log.Printf("abrindo no navegador padrão")
	return openURLDefault(url)
}
