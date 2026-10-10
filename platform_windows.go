//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	user32                       = syscall.NewLazyDLL("user32.dll")
	procGlobalMemoryStatusEx     = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW      = kernel32.NewProc("GetDiskFreeSpaceExW")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procMessageBoxW              = user32.NewProc("MessageBoxW")
)

const (
	createNoWindow                    = 0x08000000
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func totalRAM() uint64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return m.TotalPhys
}

func diskFree(dir string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0, e
	}
	return free, nil
}

// hideWindow evita que processos auxiliares abram uma janela preta de terminal.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func prepareChild(cmd *exec.Cmd) { hideWindow(cmd) }

// Um "job object" com KILL_ON_JOB_CLOSE garante que o llama-server morre junto com o app,
// mesmo se o app for fechado à força pelo Gerenciador de Tarefas.
var jobHandle uintptr

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func ensureJob() uintptr {
	if jobHandle != 0 {
		return jobHandle
	}
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0
	}
	var info jobExtendedLimit
	info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0
	}
	jobHandle = h
	return h
}

func afterStart(cmd *exec.Cmd) {
	job := ensureJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(ph)
	procAssignProcessToJobObject.Call(job, uintptr(ph))
}

func terminateProcess(p *os.Process) { _ = p.Kill() }

func killIfLlamaServer(pid int) {
	cmd := exec.Command("tasklist", "/FI", "PID eq "+itoa(pid), "/FO", "CSV", "/NH")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return
	}
	if strings.Contains(strings.ToLower(string(out)), "llama-server") {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func showFatal(msg string) {
	title, _ := syscall.UTF16PtrFromString(appName)
	text, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10) // MB_ICONERROR
}

func openURLDefault(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errors.New("endereço inválido")
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	return cmd.Start()
}

func openFolder(path string) error {
	return exec.Command("explorer", path).Start()
}
