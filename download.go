package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// downloadFile baixa url para dest com:
//   - retomada: se existir dest+".part", continua de onde parou (Range);
//   - progresso: chama progress(feito, total) algumas vezes por segundo;
//   - vigia de travamento: aborta se nenhum byte chegar por 90 s.
func downloadFile(ctx context.Context, url, dest string, expectedSize int64, progress func(done, total int64)) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
		if expectedSize > 0 && offset > expectedSize {
			_ = os.Remove(part)
			offset = 0
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var flags int
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags = os.O_WRONLY | os.O_APPEND
	case http.StatusOK:
		offset = 0
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	case http.StatusRequestedRangeNotSatisfiable:
		if expectedSize > 0 && offset == expectedSize {
			return os.Rename(part, dest)
		}
		_ = os.Remove(part)
		return errors.New("o servidor recusou continuar o download; tente novamente")
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}

	total := expectedSize
	if total <= 0 && resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}

	f, err := os.OpenFile(part, flags|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}

	var stalled atomic.Bool
	const stallLimit = 90 * time.Second
	timer := time.AfterFunc(stallLimit, func() { stalled.Store(true); cancel() })
	defer timer.Stop()

	done := offset
	lastReport := time.Time{}
	buf := make([]byte, 256<<10)
	var copyErr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			timer.Reset(stallLimit)
			if _, werr := f.Write(buf[:n]); werr != nil {
				copyErr = werr
				break
			}
			done += int64(n)
			if progress != nil && time.Since(lastReport) > 250*time.Millisecond {
				progress(done, total)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			copyErr = rerr
			break
		}
	}
	if cerr := f.Close(); copyErr == nil && cerr != nil {
		copyErr = cerr
	}
	if progress != nil {
		progress(done, total)
	}
	if copyErr != nil {
		if stalled.Load() {
			return errors.New("o download parou de receber dados; verifique a internet e clique em baixar de novo (ele continua de onde parou)")
		}
		return copyErr
	}
	if total > 0 && done != total {
		return fmt.Errorf("download incompleto (%d de %d bytes); tente novamente", done, total)
	}
	return os.Rename(part, dest)
}

// sha256File calcula o hash para conferir se o arquivo baixado está íntegro.
func sha256File(ctx context.Context, path string, progress func(done, total int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var done int64
	last := time.Time{}
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil && time.Since(last) > 250*time.Millisecond {
				progress(done, st.Size())
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// Extração de .zip e .tar.gz (com proteção contra caminhos maliciosos "../")

func safeJoin(root, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("caminho inválido no arquivo compactado: %q", name)
	}
	return filepath.Join(root, clean), nil
}

func extractArchive(src, destDir string) error {
	lower := strings.ToLower(src)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(src, destDir)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(src, destDir)
	}
	return fmt.Errorf("formato de arquivo não suportado: %s", filepath.Base(src))
}

func extractZip(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target, err := safeJoin(destDir, f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case mode&os.ModeSymlink != 0:
			if runtime.GOOS == "windows" {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			linkTarget, _ := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err := makeSymlink(destDir, target, string(linkTarget)); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		perm := mode.Perm() | 0o600
		if runtime.GOOS != "windows" && perm&0o111 == 0 && looksExecutable(f.Name) {
			perm |= 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(src, destDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := makeSymlink(destDir, target, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := os.FileMode(hdr.Mode).Perm() | 0o600
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

func makeSymlink(root, target, linkTarget string) error {
	if filepath.IsAbs(linkTarget) {
		return nil // ignora links absolutos
	}
	resolved := filepath.Join(filepath.Dir(target), linkTarget)
	rel, err := filepath.Rel(root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // ignora links que apontam para fora da pasta
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target)
	return os.Symlink(linkTarget, target)
}

func looksExecutable(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, "llama-") || strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.") || strings.HasSuffix(base, ".dylib")
}
