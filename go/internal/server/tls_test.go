package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"sekiro-go/internal/config"
)

// ---- TLS 监听（wss）集成测试：自签证书 + HTTP API + WS 注册 ----

// writeTLSCertPair 生成临时自签证书对（SAN 含 127.0.0.1/localhost），返回文件路径。
func writeTLSCertPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	return certPath, keyPath
}

// startTLSTestServer 启动带 TLS 监听的测试服务端，返回 Server 与 TLS 地址。
// 复用 Run 的 TLS 代码路径（非手工拼接），覆盖真实装配逻辑。
func startTLSTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	certPath, keyPath := writeTLSCertPair(t)
	cfg := &config.Config{
		Port:       0,
		TLSCert:    certPath,
		TLSCertKey: keyPath,
		TLSPort:    0,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = s.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.TLSAddr(); a != nil {
			return s, a.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tls listener not started in time")
	return nil, ""
}

func tlsInsecureDialer(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // 测试自签：跳过校验
		Timeout:   5 * time.Second,
	}
}

func TestTLSHTTPAPI(t *testing.T) {
	s, tlsAddr := startTLSTestServer(t)
	_ = s
	resp, err := tlsInsecureDialer(t).Get("https://" + tlsAddr + "/business/groupList")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `"status":0`) {
		t.Errorf("body = %q, want status 0", string(buf[:n]))
	}
}

func TestTLSWSRegister(t *testing.T) {
	s, tlsAddr := startTLSTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "wss://" + tlsAddr + "/register?group=tls-group&clientId=tls-client-1"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 测试自签：跳过校验
		}},
	})
	if err != nil {
		t.Fatalf("wss dial: %v", err)
	}
	defer conn.Close(websocket.StatusInternalError, "")

	// 注册是否生效：clientQueue 应能看到该 client
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		time.Sleep(50 * time.Millisecond)
		resp, err := tlsInsecureDialer(t).Get("https://" + tlsAddr + "/business/clientQueue?group=tls-group")
		if err != nil {
			t.Fatalf("clientQueue over TLS: %v", err)
		}
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if strings.Contains(string(buf[:n]), "tls-client-1") {
			found = true
		}
	}
	if !found {
		t.Error("wss client not found in clientQueue over TLS")
	}
	_ = s
}
