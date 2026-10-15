package controllerruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// certificate is a certificate and the private key it belongs to, in PEM.
type certificate struct {
	cert, key []byte
}

// issued is a certificate with the key it was issued for, before either of them
// is written out.
type issued struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
}

// certificates is a certificate authority along with the server and the client
// certificates it has signed, which is what `make certs` leaves behind.
type certificates struct {
	authority certificate
	server    certificate
	client    certificate
}

// generated is the fixture of the whole test binary.  Every test wants the same
// three keys, so they are made once, and each test writes them into a directory
// of its own.
var generated = sync.OnceValues(newCertificates)

// testTLS is the tls section of a configuration, with the keys and the
// certificates of the test binary written into the directory of the test.
func testTLS(t *testing.T) TLSConf {
	t.Helper()

	set, err := generated()
	if err != nil {
		t.Fatalf("cannot generate the test certificates: %v", err)
	}

	return set.write(t, t.TempDir())
}

// write puts the certificates down as files, and tells the configuration where
// to find them.
func (c certificates) write(t *testing.T, dir string) TLSConf {
	t.Helper()

	for name, cert := range map[string]certificate{
		"ca":     c.authority,
		"server": c.server,
		"client": c.client,
	} {
		writeFile(t, filepath.Join(dir, name+".crt"), cert.cert)
		writeFile(t, filepath.Join(dir, name+".key"), cert.key)
	}

	return TLSConf{
		CA:   filepath.Join(dir, "ca.crt"),
		Cert: filepath.Join(dir, "server.crt"),
		Key:  filepath.Join(dir, "server.key"),
	}
}

// newCertificates makes a certificate authority, a server certificate issued
// for loopback, and a client certificate.  Every key is Ed25519, as the keys
// the service is served with are.
func newCertificates() (certificates, error) {
	// The certificates are valid from an hour ago, since a clock that is a
	// little off is not worth a test failing over.
	from := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)

	authority, err := newAuthority(from, until)
	if err != nil {
		return certificates{}, err
	}

	server, err := newCertificate(&x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    from,
		NotAfter:     until,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}, authority)
	if err != nil {
		return certificates{}, err
	}

	client, err := newCertificate(&x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "teleport-sre caller"},
		NotBefore:    from,
		NotAfter:     until,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, authority)
	if err != nil {
		return certificates{}, err
	}

	root, err := authority.asPEM()
	if err != nil {
		return certificates{}, err
	}

	return certificates{authority: root, server: server, client: client}, nil
}

// newAuthority makes a certificate authority that signs nothing but the two
// certificates the service needs.
func newAuthority(from, until time.Time) (issued, error) {
	public, key, err := newKey()
	if err != nil {
		return issued{}, err
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "teleport-sre CA"},
		NotBefore:             from,
		NotAfter:              until,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// An authority that signs another authority would be able to widen
		// the trust, which nothing here asks for.
		MaxPathLenZero: true,
	}

	certificate, err := create(template, template, public, key)
	if err != nil {
		return issued{}, err
	}

	return issued{certificate: certificate, key: key}, nil
}

// newCertificate makes a certificate for the template, signed by the authority,
// in the form it is written down in.
func newCertificate(template *x509.Certificate, authority issued) (certificate, error) {
	public, key, err := newKey()
	if err != nil {
		return certificate{}, err
	}

	signed, err := create(template, authority.certificate, public, authority.key)
	if err != nil {
		return certificate{}, err
	}

	return issued{certificate: signed, key: key}.asPEM()
}

// newKey is a key of the kind the service is served with.
func newKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate an ed25519 key: %w", err)
	}

	return public, key, nil
}

// create signs the template, which says what the certificate is for.  The
// public key is the one the certificate is issued for, and the key is the one
// of the certificate authority doing the signing.
func create(template, authority *x509.Certificate, public ed25519.PublicKey, key ed25519.PrivateKey) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, template, authority, public, key)
	if err != nil {
		return nil, fmt.Errorf("cannot sign a certificate: %w", err)
	}

	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("cannot read back a signed certificate: %w", err)
	}

	return certificate, nil
}

// asPEM is the certificate and its key, as they are written down.
func (i issued) asPEM() (certificate, error) {
	key, err := x509.MarshalPKCS8PrivateKey(i.key)
	if err != nil {
		return certificate{}, fmt.Errorf("cannot write out an ed25519 key: %w", err)
	}

	return certificate{
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.certificate.Raw}),
		key:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	}, nil
}

// writeFile puts a file down for the service under test to read.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("cannot write %q: %v", path, err)
	}
}

// keyPair is a certificate with the key it belongs to.
func (c certificate) keyPair(t *testing.T) tls.Certificate {
	t.Helper()

	pair, err := tls.X509KeyPair(c.cert, c.key)
	if err != nil {
		t.Fatalf("cannot read a certificate with its key: %v", err)
	}

	return pair
}

// pool is the certificate authority to verify the other end with.
func (c certificate) pool(t *testing.T) *x509.CertPool {
	t.Helper()

	hold := x509.NewCertPool()
	if !hold.AppendCertsFromPEM(c.cert) {
		t.Fatal("a certificate authority with no certificates in it is no authority")
	}

	return hold
}

// caller reaches the api trusting the given authority, and presenting the
// certificate it is given.  A caller with nothing to present says nothing about
// who it is.
func caller(t *testing.T, trust certificates, presenting *certificate) *http.Client {
	t.Helper()

	secure := &tls.Config{RootCAs: trust.authority.pool(t), MinVersion: tls.VersionTLS13}
	if presenting != nil {
		secure.Certificates = []tls.Certificate{presenting.keyPair(t)}
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: secure},
		Timeout:   10 * time.Second,
	}
}

func TestAPIWantsAClientCertificate(t *testing.T) {
	t.Parallel()

	set, err := generated()
	if err != nil {
		t.Fatalf("cannot generate the test certificates: %v", err)
	}
	// An authority of its own, for a caller the service has never heard of.
	foreign, err := newCertificates()
	if err != nil {
		t.Fatalf("cannot generate the foreign certificates: %v", err)
	}

	client := fake.NewClientset(testCluster()...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	address := secureAPI(t, Conf{
		API: Listener{Host: "127.0.0.1", Prefix: "/"},
		TLS: set.write(t, t.TempDir()),
	}, KubeConn{k8s: client}, cache)

	tests := []struct {
		name string
		// presenting is the certificate the caller brings, and a nil one is a
		// caller with nothing to say about who it is.
		presenting *certificate
		// served says whether the request is answered at all.
		served bool
	}{
		{
			name:       "a caller the authority signed for",
			presenting: &set.client,
			served:     true,
		},
		{
			name:       "a caller with no certificate",
			presenting: nil,
			served:     false,
		},
		{
			name:       "a caller another authority signed for",
			presenting: &foreign.client,
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

	set, err := generated()
	if err != nil {
		t.Fatalf("cannot generate the test certificates: %v", err)
	}
	keys := set.write(t, t.TempDir())

	// A file that is not a certificate, for the authority to be told off over.
	junk := filepath.Join(t.TempDir(), "junk.crt")
	writeFile(t, junk, []byte("this is not a certificate\n"))

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
