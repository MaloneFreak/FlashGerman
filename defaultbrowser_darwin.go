//go:build darwin

package main

import "os/exec"

// defaultBrowserName lê o navegador padrão das preferências do macOS (ex.: "com.google.chrome").
// Sem registro para https, o padrão é o Safari.
func defaultBrowserName() string {
	out, err := exec.Command("defaults", "read", "com.apple.LaunchServices/com.apple.launchservices.secure", "LSHandlers").Output()
	if err != nil {
		return "com.apple.safari"
	}
	return parseLSHandlers(string(out))
}
