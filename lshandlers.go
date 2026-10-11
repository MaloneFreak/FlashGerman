package main

import "strings"

// parseLSHandlers extrai o app que abre links https da saída de
// "defaults read com.apple.LaunchServices/... LSHandlers" (formato texto do plist).
func parseLSHandlers(out string) string {
	depth := 0
	entry := map[string]string{}
	found := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if depth == 2 {
			if k, v, ok := strings.Cut(strings.TrimSuffix(t, ";"), " = "); ok {
				entry[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		depth += strings.Count(t, "{") + strings.Count(t, "(") - strings.Count(t, "}") - strings.Count(t, ")")
		if depth < 2 && len(entry) > 0 {
			scheme := strings.ToLower(entry["LSHandlerURLScheme"])
			if role := entry["LSHandlerRoleAll"]; role != "" && role != "-" {
				if scheme == "https" {
					return role
				}
				if scheme == "http" && found == "" {
					found = role
				}
			}
			entry = map[string]string{}
		}
	}
	if found != "" {
		return found
	}
	return "com.apple.safari"
}
