package main

import "fmt"

// XCL is the disabled compatibility backend for xray-cf-lite.
type XCL struct{}

func DetectXCL() (*XCL, error) {
	return nil, fmt.Errorf("%w：xray-cf-lite 后端已禁用", errXrayDisabled)
}

func (x *XCL) Kind() string { return "xray-cf-lite" }

func (x *XCL) Describe() string {
	return "xray-cf-lite 后端已禁用"
}

func (x *XCL) disabled() error {
	return errXrayDisabled
}

func (x *XCL) Inbounds(live map[string]bool) ([]Inbound, error) {
	return nil, x.disabled()
}

func (x *XCL) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	return nil, x.disabled()
}

func (x *XCL) InboundLinks(ids []int, publicHost string) ([]string, error) {
	return nil, x.disabled()
}

func (x *XCL) Bind(inboundTag string, hostname string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error) {
	return nil, x.disabled()
}

func (x *XCL) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) CreateInbound(spec NewInboundSpec, tunnels []*Tunnel) (*CreatedInbound, error) {
	return nil, x.disabled()
}

func (x *XCL) UpdateInbound(id int, patch InboundPatch, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) AddClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) DeleteClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) ResetClient(id int, email string, tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) OnTunnelsChanged(tunnels []*Tunnel) error {
	return x.disabled()
}

func (x *XCL) Close() {}

var errXCLReadOnly = fmt.Errorf("%w：xray-cf-lite 后端已禁用", errXrayDisabled)
