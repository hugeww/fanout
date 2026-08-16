package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// webServer 管理 HTTP/HTTPS 监听，支持在运行时切换端口/监听地址而不重启进程。
// 切换端口或监听地址会新起一个 net.Listener，旧的优雅关闭。
type webServer struct {
	handler http.Handler
	dir     string

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	addr string
	tls  bool
}

func newWebServer(dir string, h http.Handler) *webServer {
	return &webServer{handler: h, dir: dir}
}

// serve 用当前 WebSettings 起第一个监听并阻塞。返回时说明监听彻底退出。
func (s *webServer) serve() error {
	cfg := getWebSettings()
	if err := s.reload(cfg); err != nil {
		return err
	}
	// 主 goroutine 就地阻塞，等监听被 reload 或退出替换。
	// 这里靠一个永不返回的 select 挂住：真正的 Serve 在 reload 里各自的 goroutine 跑。
	select {}
}

// reload 切换到新的监听配置。分两种情形：
//   - 端口/监听地址变化：先探测新地址能绑上（TLS 时还要能加载证书），
//     绑得上再关旧的、启新的，绑不上保持旧监听不动。
//   - 仅 TLS 变化（地址不变）：旧监听占着同一端口，没法先探测，只能
//     先关旧的再绑新的。窗口期很短，管理界面短暂断开可接受。
func (s *webServer) reload(cfg WebSettings) error {
	addr := cfg.listenAddrString()

	// 先准备 TLS 配置：证书无效就及早报错，此时还没动现有监听。
	var tlsCfg *tls.Config
	if cfg.TLS {
		certFile, keyFile := settingsCertPaths(s.dir)
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("加载 HTTPS 证书失败（先把证书上传到设置面板）: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	s.mu.Lock()
	curAddr := s.addr
	curTLS := s.tls
	s.mu.Unlock()

	// TLS 状态变了、监听地址没变：同一端口，先关旧的再绑新的。
	if curAddr == addr && curTLS != cfg.TLS {
		s.mu.Lock()
		oldSrv := s.srv
		oldLn := s.ln
		s.srv = nil
		s.ln = nil
		s.mu.Unlock()
		if oldSrv != nil {
			_ = oldSrv.Close()
			_ = oldLn.Close()
		}
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("无法监听 %s：%w", addr, err)
	}

	s.mu.Lock()
	oldSrv := s.srv
	oldLn := s.ln
	srv := &http.Server{Handler: s.handler}
	s.srv = srv
	s.ln = ln
	s.addr = addr
	s.tls = cfg.TLS
	s.mu.Unlock()

	go func() {
		var serveErr error
		if tlsCfg != nil {
			tlsLn := tls.NewListener(ln, tlsCfg)
			serveErr = srv.Serve(tlsLn)
		} else {
			serveErr = srv.Serve(ln)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("管理界面监听 %s 退出: %v", addr, serveErr)
		}
	}()

	// 关掉旧监听。给正在处理的请求一点收尾时间，
	// 尤其是触发这次 reload 的那个请求本身要先把响应写完。
	if oldSrv != nil {
		go func() {
			time.Sleep(1 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = oldSrv.Shutdown(ctx)
			_ = oldLn.Close()
		}()
	}

	log.Printf("管理界面监听已切换到 %s%s", addr, map[bool]string{true: " (HTTPS)", false: ""}[cfg.TLS])
	return nil
}

// applyWebSettings 校验、落盘并切换监听。任一步失败都不改动线上监听。
func (s *webServer) applyWebSettings(next WebSettings) error {
	if err := validatePort(next.Port); err != nil {
		return err
	}
	norm, err := normalizeListenAddr(next.ListenAddr)
	if err != nil {
		return err
	}
	next.ListenAddr = norm

	cur := getWebSettings()
	// 端口、监听地址、TLS 都没变就只需要确保已生效，避免无谓重绑
	if next.Port == cur.Port && next.ListenAddr == cur.ListenAddr && next.TLS == cur.TLS {
		return nil
	}

	if err := s.reload(next); err != nil {
		return err
	}

	webSettingsMu.Lock()
	webSettingsCur = next
	webSettingsMu.Unlock()
	if err := saveWebSettings(); err != nil {
		log.Printf("保存 Web 设置失败: %v", err)
		return err
	}
	return nil
}

// applyWebTLSCert 校验并落盘设置面板上传的证书，然后以 HTTPS 重新监听。
// 任一步失败都不改动现有监听。
func (s *webServer) applyWebTLSCert(certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("证书与私钥不匹配或格式错误: %w", err)
	}
	certFile, keyFile := settingsCertPaths(s.dir)
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		return fmt.Errorf("写入证书失败: %w", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		return fmt.Errorf("写入私钥失败: %w", err)
	}

	next := getWebSettings()
	next.TLS = true
	if err := s.applyWebSettings(next); err != nil {
		return err
	}
	log.Printf("管理界面已切换为 HTTPS")
	return nil
}
