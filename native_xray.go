package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Xray integration is intentionally disabled. Keep this compatibility
// surface because native.go is retained as a legacy backend, but make every
// path fail before it can discover, write, validate, or start Xray.
var errXrayDisabled = errors.New("Xray 功能已禁用")

func findXray(workDir string) (string, error) {
	return "", fmt.Errorf("%w，native/3x-ui 后端不可用", errXrayDisabled)
}

func buildXrayConfig(inbounds []*nativeInbound, tunnels []*Tunnel) map[string]any {
	return nil
}

func writeXrayConfig(dir string, cfg map[string]any) (string, error) {
	return "", errXrayDisabled
}

func verifyXrayConfig(bin, cfgPath string) error {
	return errXrayDisabled
}

// xrayProc remains only to keep the legacy Native type buildable. It never
// launches or terminates a process now that findXray always rejects the
// backend.
type xrayProc struct {
	bin string
	dir string
}

func (p *xrayProc) restart(cfgPath string) error { return errXrayDisabled }
func (p *xrayProc) stop()                        {}

func (p *xrayProc) pidPath() string {
	if p == nil {
		return ""
	}
	return filepath.Join(p.dir, "xray.pid")
}

func (p *xrayProc) writePID(pid int) {}
func (p *xrayProc) reapOrphan()      {}

func trimOutput(b []byte) string { return string(b) }
