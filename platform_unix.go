//go:build !windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func totalRAM() uint64 {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			v, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			return v
		}
	}
	return 0
}

func diskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func hideWindow(cmd *exec.Cmd) {}

func terminateProcess(p *os.Process) { _ = p.Signal(syscall.SIGTERM) }

func killIfLlamaServer(pid int) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil || !strings.Contains(string(out), "llama-server") {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
		time.Sleep(time.Second)
		_ = p.Kill()
	}
}

func showFatal(msg string) {
	fmt.Fprintln(os.Stderr, appName+": "+msg)
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display dialog %q with title %q buttons {"OK"} default button 1 with icon stop`, msg, appName)
		_ = exec.Command("osascript", "-e", script).Run()
	default:
		if p, err := exec.LookPath("zenity"); err == nil {
			_ = exec.Command(p, "--error", "--title="+appName, "--text="+msg).Run()
		} else if p, err := exec.LookPath("kdialog"); err == nil {
			_ = exec.Command(p, "--error", msg, "--title", appName).Run()
		}
	}
}

func openURLDefault(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errors.New("endereço inválido")
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func openFolder(path string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", path).Start()
	}
	return exec.Command("xdg-open", path).Start()
}
