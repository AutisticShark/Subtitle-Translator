package email

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// sesTLSServer is a local HTTPS server that presents a certificate for the
// real SES host, signed by a private CA written to caFile. The SES transport
// is pointed at it by dialing, so the SDK-built TLS configuration (roots from
// AWS_CA_BUNDLE or a profile's ca_bundle) is what verifies it.
type sesTLSServer struct {
	srv    *httptest.Server
	caFile string
	hits   atomic.Int32
}

func newSESTLSServer(t *testing.T, handler http.HandlerFunc) *sesTLSServer {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "email.us-east-1.amazonaws.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"email.us-east-1.amazonaws.com"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &sesTLSServer{caFile: filepath.Join(t.TempDir(), "ca.pem")}
	if err := os.WriteFile(s.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		handler(w, r)
	}))
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

// service returns an SES sender whose SDK-built transport dials this server.
func (s *sesTLSServer) service(t *testing.T) service {
	t.Helper()
	svc := mustNew(t, sesEnv(nil))
	addr := s.srv.Listener.Addr().String()
	svc.t.(*sesTransport).adjustTransport = func(tr *http.Transport) {
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	return svc
}

func TestSESHonoursTheCustomCABundle(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"MessageId":"accepted"}`) }

	t.Run("AWS_CA_BUNDLE", func(t *testing.T) {
		isolateAWS(t)
		s := newSESTLSServer(t, ok)
		t.Setenv("AWS_CA_BUNDLE", s.caFile)
		if err := s.service(t).Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
			t.Fatalf("send through the bundle-trusted server: %v", err)
		}
		if s.hits.Load() != 1 {
			t.Fatalf("hits %d", s.hits.Load())
		}
	})

	t.Run("profile ca_bundle", func(t *testing.T) {
		isolateAWS(t)
		s := newSESTLSServer(t, ok)
		cfg := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(cfg, []byte("[default]\nca_bundle = "+filepath.ToSlash(s.caFile)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_CONFIG_FILE", cfg)
		if err := s.service(t).Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
			t.Fatalf("send through the profile-trusted server: %v", err)
		}
		if s.hits.Load() != 1 {
			t.Fatalf("hits %d", s.hits.Load())
		}
	})

	t.Run("no bundle: the private CA is not trusted", func(t *testing.T) {
		isolateAWS(t)
		s := newSESTLSServer(t, ok)
		assertDelivery(t, s.service(t).Send(context.Background(), "user@example.com", "Subject", "Text", ""))
		if s.hits.Load() != 0 {
			t.Fatalf("request reached an untrusted server: %d", s.hits.Load())
		}
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		isolateAWS(t)
		other := newSESTLSServer(t, ok)
		s := newSESTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.srv.URL+"/v2/email/outbound-emails", http.StatusTemporaryRedirect)
		})
		t.Setenv("AWS_CA_BUNDLE", s.caFile)
		assertDelivery(t, s.service(t).Send(context.Background(), "user@example.com", "Subject", "Text", ""))
		if s.hits.Load() != 1 || other.hits.Load() != 0 {
			t.Fatalf("hits %d, redirect target %d", s.hits.Load(), other.hits.Load())
		}
	})
}
