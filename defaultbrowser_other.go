//go:build !windows && !darwin

package main

import (
	"os/exec"
	"strings"
)

// defaultBrowserName pergunta ao sistema (ex.: "google-chrome.desktop", "firefox.desktop").
func defaultBrowserName() string {
	out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
