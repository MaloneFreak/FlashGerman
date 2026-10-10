package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// O app não traz um navegador próprio (isso deixaria o download com 100+ MB).
// Em vez disso, abre a interface numa janela "modo aplicativo" do Edge/Chrome/Brave
// que já existe no computador: sem barra de endereço, parece um programa normal.
// Se nenhum for encontrado, abre no navegador padrão.

func browserCandidates() []string {
	var c []string
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LocalAppData")
		for _, base := range []string{pf86, pf, local} {
			if base == "" {
				continue
			}
			c = append(c, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		}
		for _, base := range []string{pf, pf86, local} {
			if base == "" {
				continue
			}
			c = append(c,
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`),
				filepath.Join(base, `Chromium\Application\chrome.exe`),
			)
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, base := range []string{"/Applications", filepath.Join(home, "Applications")} {
			c = append(c,
				filepath.Join(base, "Google Chrome.app/Contents/MacOS/Google Chrome"),
				filepath.Join(base, "Microsoft Edge.app/Contents/MacOS/Microsoft Edge"),
				filepath.Join(base, "Brave Browser.app/Contents/MacOS/Brave Browser"),
				filepath.Join(base, "Chromium.app/Contents/MacOS/Chromium"),
			)
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "brave-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				c = append(c, p)
			}
		}
	}
	return c
}

func findAppBrowser() string {
	for _, p := range browserCandidates() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// openAppWindow abre a janela. Devolve o processo do navegador quando foi possível usar
// um perfil próprio (aí sabemos quando a janela foi fechada).
func openAppWindow(url, profileDir string) (*exec.Cmd, error) {
	if browser := findAppBrowser(); browser != "" {
		args := []string{
			"--app=" + url,
			"--user-data-dir=" + profileDir,
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-background-mode",
			"--disable-features=Translate,msEdgeSidebarV2,msHubApps",
			"--window-size=1280,860",
		}
		cmd := exec.Command(browser, args...)
		if err := cmd.Start(); err == nil {
			log.Printf("janela aberta com %s", browser)
			return cmd, nil
		} else {
			log.Printf("não foi possível abrir %s: %v", browser, err)
		}
	}
	log.Printf("abrindo no navegador padrão")
	return nil, openURLDefault(url)
}
