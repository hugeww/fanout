package main

import (
	"fmt"
	"strings"
)

// XUI is kept as a disabled compatibility backend. Xray and 3x-ui are not
// detected, started, configured, or exposed by fanout.
type XUI struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	BasePath string `json:"base_path"`
	Scheme   string `json:"scheme"`
	TLS      bool   `json:"-"`
	Base     string `json:"-"`
	Token    string `json:"-"`
	workDir  string
}

var (
	xuiBinary = "/usr/local/x-ui/x-ui"
	xuiMenu   = "/usr/bin/x-ui"
)

const xuiTagPrefix = "fanout-"

// sanitizeTag keeps the stable tunnel tag format used by the non-Xray
// management layer.
func sanitizeTag(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func tunnelTag(t *Tunnel) string {
	return xuiTagPrefix + sanitizeTag(t.Node.HostName)
}

func exitLabel(t *Tunnel) string {
	region := t.Node.CountryCode
	if region == "" {
		region = t.Node.Country
	}
	suffix := t.Node.HostName
	if t.ExitIP != "" {
		if i := strings.LastIndex(t.ExitIP, "."); i >= 0 {
			suffix = t.ExitIP[i+1:]
		} else {
			suffix = t.ExitIP
		}
	}
	if region == "" {
		return suffix
	}
	return region + "-" + suffix
}

func renameExitSuffix(remark, newLabel string) string {
	if remark == "" {
		return remark
	}
	parts := strings.Split(remark, "-")
	if len(parts) < 2 {
		return remark
	}
	keep := parts[:len(parts)-2]
	if len(keep) == 0 {
		return newLabel
	}
	return strings.Join(keep, "-") + "-" + newLabel
}

// Inbound is the backend-neutral inbound summary used by the existing
// management and exit APIs. It is retained for non-Xray API compatibility.
type Inbound struct {
	ID       int    `json:"id"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Remark   string `json:"remark"`
	Enable   bool   `json:"enable"`
	Tag      string `json:"tag"`
	BoundTo  string `json:"bound_to"`
	BoundUp  bool   `json:"bound_up"`
}

type InboundDetail struct {
	Inbound
	Clients []ClientInfo `json:"clients"`
	Links   []string     `json:"links"`
	Listen  string       `json:"listen"`
	Network string       `json:"network"`
	TLS     string       `json:"tls"`
}

type ClientInfo struct {
	Email  string `json:"email"`
	ID     string `json:"id"`
	Enable bool   `json:"enable"`
}

func DetectXUI(workDir string) (*XUI, error) {
	return nil, fmt.Errorf("%w：3x-ui 后端已禁用", errXrayDisabled)
}

func (x *XUI) Kind() string { return "3x-ui" }

func (x *XUI) Describe() string {
	return "3x-ui 后端已禁用"
}

func (x *XUI) disabled() error {
	return errXrayDisabled
}

func (x *XUI) Inbounds(live map[string]bool) ([]Inbound, error) {
	return nil, x.disabled()
}

func (x *XUI) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	return nil, x.disabled()
}

func (x *XUI) InboundLinks(ids []int, publicHost string) ([]string, error) {
	return nil, x.disabled()
}

func (x *XUI) Bind(inboundTag string, hostname string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error) {
	return nil, x.disabled()
}

func (x *XUI) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) CreateInbound(spec NewInboundSpec, tunnels []*Tunnel) (*CreatedInbound, error) {
	return nil, x.disabled()
}

func (x *XUI) UpdateInbound(id int, patch InboundPatch, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) AddClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) DeleteClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) ResetClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) OnTunnelsChanged(tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XUI) Close() {}
