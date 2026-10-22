package health

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"calserver/internal/config"
)

// TestCheckServerVerifiesCertName serves the DAV listener with a certificate
// that names only a DNS host, as a real deployment does. The probe dials
// loopback, so it must verify against the certificate's name, not 127.0.0.1.
func TestCheckServerVerifiesCertName(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cal.example.test"},
		DNSNames:     []string{"cal.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	certPath := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DAV", "1, 2")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	defer srv.Close()

	var cfg config.Config
	cfg.Server.Addr = srv.Listener.Addr().String()
	cfg.Server.TLS.Cert = certPath
	o := OptionsFromConfig(cfg)
	if o.ServerName != "cal.example.test" {
		t.Fatalf("ServerName = %q, want the certificate's name", o.ServerName)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	o.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	o.Timeout = 5 * time.Second
	if r := checkServer(context.Background(), o); r.Status != Healthy {
		t.Fatalf("probe = %s %q, want healthy", r.Status, r.Message)
	}
}
