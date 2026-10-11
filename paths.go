package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// resolveDataDir decide onde ficam configurações, baralho, motor e modelos.
//
// Ordem de preferência:
//  1. --data-dir (ou variável FLASHGERMAN_DATA)
//  2. pasta "data" ao lado do executável (modo portátil: dá para levar num pendrive)
//  3. pasta de configuração do usuário (%APPDATA%\FlashGerman, ~/Library/Application Support/FlashGerman, ~/.config/FlashGerman)
//
// No macOS, um .app não deve escrever dentro do próprio pacote, então vamos direto para a opção 3.
func resolveDataDir(override string) (string, error) {
	if override == "" {
		override = os.Getenv("FLASHGERMAN_DATA")
	}
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if err := ensureWritable(abs); err != nil {
			return "", fmt.Errorf("a pasta de dados %q não pode ser usada: %w", abs, err)
		}
		return abs, nil
	}

	exe, err := os.Executable()
	if err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
	}
	exeDir := filepath.Dir(exe)

	insideAppBundle := runtime.GOOS == "darwin" && strings.Contains(exeDir, ".app/Contents/MacOS")
	goRun := strings.Contains(exe, "go-build") // executando com "go run"

	if goRun {
		if wd, err := os.Getwd(); err == nil {
			candidate := filepath.Join(wd, "data")
			if ensureWritable(candidate) == nil {
				return candidate, nil
			}
		}
	} else if !insideAppBundle && exe != "" && !isTempDir(exeDir) {
		candidate := filepath.Join(exeDir, "data")
		if ensureWritable(candidate) == nil {
			return candidate, nil
		}
	}

	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", errors.New("não foi possível encontrar uma pasta para salvar os dados")
		}
		base = home
	}
	candidate := filepath.Join(base, appName)
	if err := ensureWritable(candidate); err != nil {
		return "", fmt.Errorf("não foi possível criar a pasta de dados %q: %w", candidate, err)
	}
	return candidate, nil
}

func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// setupLogging escreve o log em data/logs/app.log (e também no terminal, se houver).
func setupLogging(dataDir string) {
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	path := filepath.Join(logDir, "app.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
}

// writeFileAtomic grava em um arquivo temporário e renomeia, para nunca deixar um JSON pela metade.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, path); err != nil {
		// No Windows, renomear por cima pode falhar se o destino estiver aberto; tenta remover antes.
		_ = os.Remove(path)
		if err2 := os.Rename(tmpName, path); err2 != nil {
			os.Remove(tmpName)
			return err2
		}
	}
	return nil
}

// isTempDir detecta quando o programa foi aberto de dentro do .zip sem extrair
// (o Windows copia para uma pasta temporária e apaga depois). Nesse caso salvar os
// dados ao lado do executável faria tudo se perder; usamos a pasta do usuário.
func isTempDir(dir string) bool {
	tmp := os.TempDir()
	candidates := []string{tmp}
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil && resolved != tmp {
		candidates = append(candidates, resolved) // macOS: /var/... -> /private/var/...
	}
	dirs := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		dirs = append(dirs, resolved)
	}
	for _, t := range candidates {
		for _, d := range dirs {
			rel, err := filepath.Rel(t, d)
			if err == nil && (rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))) {
				return true
			}
		}
	}
	return false
}
