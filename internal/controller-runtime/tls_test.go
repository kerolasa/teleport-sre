package controllerruntime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/kerolasa/teleport-sre/internal/testcerts"
)

// testTLS is the tls section of a configuration, with the keys and the
// certificates of the test binary written into the directory of the test.
func testTLS(t *testing.T) TLSConf {
	t.Helper()

	set := testcerts.Write(t, t.TempDir())

	return TLSConf{
		CA:   set.CA,
		Cert: set.ServerCert,
		Key:  set.ServerKey,
	}
}

// caller reaches the api trusting the authority of the given set, and
// presenting the certificate it is given.  A caller with nothing to present
// says nothing about who it is.
func caller(t *testing.T, trust testcerts.Set, presenting *testcerts.Set) *http.Client {
	t.Helper()

	secure := &tls.Config{RootCAs: testcerts.Pool(t, trust.CA), MinVersion: tls.VersionTLS13}
	if presenting != nil {
		secure.Certificates = []tls.Certificate{testcerts.KeyPair(t, presenting.ClientCert, presenting.ClientKey)}
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: secure},
		Timeout:   10 * time.Second,
	}
}

func TestAPIWantsAClientCertificate(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	// An authority of its own, for a caller the service has never heard of.
	foreign := testcerts.Foreign(t, t.TempDir())

	client := fake.NewClientset(testCluster()...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	address := secureAPI(t, Conf{
		API: Listener{Host: "127.0.0.1", Prefix: "/"},
		TLS: TLSConf{CA: set.CA, Cert: set.ServerCert, Key: set.ServerKey},
	}, KubeConn{k8s: client}, cache)

	tests := []struct {
		name string
		// presenting is the certificate the caller brings, and a nil one is a
		// caller with nothing to say about who it is.
		presenting *testcerts.Set
		// served says whether the request is answered at all.
		served bool
	}{
		{
			name:       "a caller the authority signed for",
			presenting: &set,
			served:     true,
		},
		{
			name:       "a caller with no certificate",
			presenting: nil,
			served:     false,
		},
		{
			name:       "a caller another authority signed for",
			presenting: &foreign,
			served:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := caller(t, set, test.presenting)
			response, err := client.Get(address + "/list")
			if !test.served {
				if err == nil {
					body, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					t.Fatalf("the api answered a caller it should have turned away: %s", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("the api refused a caller with a certificate: %v", err)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("cannot read the body of the answer: %v", err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatalf("cannot close the body of the answer: %v", err)
			}

			if response.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want %d (%s)", response.StatusCode, http.StatusOK, body)
			}
			if !strings.Contains(string(body), `{"namespace":"web","name":"front","replicas":3}`) {
				t.Errorf("the answer does not carry the deployments: %s", body)
			}
			// The design doc asks for TLS 1.3 with ed25519 keys and the first
			// of the cipher suites go offers.
			if version := response.TLS.Version; version != tls.VersionTLS13 {
				t.Errorf("the connection was secured with %x, want TLS 1.3 (%x)", version, tls.VersionTLS13)
			}
			if suite := response.TLS.CipherSuite; suite != tls.TLS_AES_128_GCM_SHA256 {
				t.Errorf("the connection was secured with suite %x, want TLS_AES_128_GCM_SHA256 (%x)",
					suite, tls.TLS_AES_128_GCM_SHA256)
			}
			if got := response.TLS.PeerCertificates; len(got) == 0 || got[0].PublicKeyAlgorithm != x509.Ed25519 {
				t.Error("the api served a certificate that is not ed25519")
			}
		})
	}
}

func TestListenAPIWithoutUsableKeys(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	keys := TLSConf{CA: set.CA, Cert: set.ServerCert, Key: set.ServerKey}

	// A file that is not a certificate, for the authority to be told off over.
	junk := filepath.Join(t.TempDir(), "junk.crt")
	if err := os.WriteFile(junk, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("cannot write %q: %v", junk, err)
	}

	tests := []struct {
		name string
		conf TLSConf
		// complained is a piece of what the failure has to say, so that the
		// reason for it is not lost along the way.
		complained string
	}{
		{
			name:       "no keys at all",
			conf:       TLSConf{},
			complained: "nothing about the tls ca",
		},
		{
			name: "no key for the certificate",
			// A service that cannot authenticate its callers has no business
			// serving them, so it does not start without these.
			conf:       TLSConf{CA: keys.CA, Cert: keys.Cert},
			complained: "nothing about the tls key",
		},
		{
			name:       "a certificate that is not there",
			conf:       TLSConf{CA: keys.CA, Cert: filepath.Join(t.TempDir(), "gone.crt"), Key: keys.Key},
			complained: "cannot use the certificate",
		},
		{
			name:       "an authority with no certificates in it",
			conf:       TLSConf{CA: junk, Cert: keys.Cert, Key: keys.Key},
			complained: "holds no certificates",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := ListenAPI(Conf{TLS: test.conf}, KubeConn{}, NewCache(KubeConn{}))
			if err == nil {
				t.Fatalf("ListenAPI() is ready to serve %s, want it to refuse without usable keys", server.Addr)
			}
			if !strings.Contains(err.Error(), test.complained) {
				t.Errorf("ListenAPI() said %q, want it to mention %q", err, test.complained)
			}
		})
	}
}

// secureAPI starts the api over TLS on a loopback address, and tells where it
// is.  This is how the service is served for real, rather than over the plain
// http of the other tests.
func secureAPI(t *testing.T, conf Conf, kubeConn KubeConn, cache *Cache) string {
	t.Helper()

	server, err := ListenAPI(conf, kubeConn, cache)
	if err != nil {
		t.Fatalf("ListenAPI() returned an error: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen on loopback: %v", err)
	}

	served := make(chan error, 1)
	go func() {
		// The certificates are in the configuration already, so none is
		// named here, which is what the startup does.
		served <- server.ServeTLS(listener, "", "")
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("cannot close the api server: %v", err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the api server stopped with %v, want the close it was asked for", err)
		}
	})

	return "https://" + listener.Addr().String()
}
