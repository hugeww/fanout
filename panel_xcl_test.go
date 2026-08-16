package main

import (
	"errors"
	"testing"
)

// xray-cf-lite 后端已按计划禁用，绑定/改配置等功能不应再可调用。
func TestXCLDisabled(t *testing.T) {
	x := &XCL{}
	if _, err := x.Inbounds(nil); !errors.Is(err, errXrayDisabled) {
		t.Errorf("Inbounds 应返回 errXrayDisabled，实际 %v", err)
	}
	if err := x.Bind("in", "host", nil); !errors.Is(err, errXrayDisabled) {
		t.Errorf("Bind 应返回 errXrayDisabled，实际 %v", err)
	}
	if _, err := x.CreateInbound(NewInboundSpec{}, nil); !errors.Is(err, errXrayDisabled) {
		t.Errorf("CreateInbound 应返回 errXrayDisabled，实际 %v", err)
	}
	if _, err := DetectXCL(); !errors.Is(err, errXrayDisabled) {
		t.Errorf("DetectXCL 应返回 errXrayDisabled，实际 %v", err)
	}
}

func TestConfigurePanelReadsSavedMode(t *testing.T) {
	dir := t.TempDir()
	if err := savePanelMode(dir, "xray-cf-lite"); err != nil {
		t.Fatal(err)
	}

	// 命令行没指定时，应该沿用界面上次选的后端
	configurePanel(dir, "")
	if got := currentPanelMode(); got != "xray-cf-lite" {
		t.Fatalf("重启后应恢复界面选过的模式，实际 %q", got)
	}

	// 命令行显式指定时优先级更高
	configurePanel(dir, "native")
	if got := currentPanelMode(); got != "native" {
		t.Fatalf("-panel 应压过盘上的记录，实际 %q", got)
	}

	// 空值等于删档回到自动探测
	if err := savePanelMode(dir, ""); err != nil {
		t.Fatal(err)
	}
	configurePanel(dir, "")
	if got := currentPanelMode(); got != "" {
		t.Fatalf("清空后应回到自动探测，实际 %q", got)
	}
}
