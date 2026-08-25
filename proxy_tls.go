package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

const tlsHandshakeRecord = 0x16

func (p *unifiedProxy) serveTLS(client net.Conn, br *bufio.Reader) {
	cfg, err := p.tlsServerConfig()
	if err != nil {
		return
	}
	tc := tls.Server(&bufferedConn{Conn: client, r: br}, cfg)
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	p.serveHTTP(tc, bufio.NewReader(tc))
}

func (p *unifiedProxy) tlsServerConfig() (*tls.Config, error) {
	if p.manager != nil && p.manager.workDir != "" {
		certFile, keyFile := settingsCertPaths(p.manager.workDir)
		if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
		}
	}
	cert, err := cachedProxySelfSignedCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func proxySelfSignedCert() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "fanout-proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// cachedProxySelfSignedCert 进程内只生成一次自签证书，避免每条连接都做 RSA。
var (
	proxyCertOnce sync.Once
	proxyCert     tls.Certificate
	proxyCertErr  error
)

func cachedProxySelfSignedCert() (tls.Certificate, error) {
	proxyCertOnce.Do(func() {
		proxyCert, proxyCertErr = proxySelfSignedCert()
	})
	return proxyCert, proxyCertErr
}
