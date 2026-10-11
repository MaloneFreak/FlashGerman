//go:build windows

package main

import (
	"os/exec"
	"strings"
)

// defaultBrowserName lê o navegador padrão no registro (ex.: "ChromeHTML", "MSEdgeHTM").
func defaultBrowserName() string {
	for _, scheme := range []string{"https", "http"} {
		cmd := exec.Command("reg", "query",
			`HKCU\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\`+scheme+`\UserChoice`, "/v", "ProgId")
		hideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && strings.EqualFold(f[0], "ProgId") {
				return strings.Join(f[2:], " ")
			}
		}
	}
	return ""
}
