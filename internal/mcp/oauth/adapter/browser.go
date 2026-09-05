package adapter

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"runtime"
)

// SystemBrowserOpener opens the platform browser.
type SystemBrowserOpener struct{}

// Open launches the default browser on the OS.
func (b *SystemBrowserOpener) Open(_ context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return errors.New("unsupported platform for browser open")
	}
	return cmd.Start()
}

// FreeLoopbackPort asks the OS for a free loopback port.
func FreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}
