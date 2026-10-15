// Package testcerts makes the keys and the certificates the tests of this
// repository need: a certificate authority, the certificate the api server
// serves, and the certificate a caller authenticates with.  These are the
// three `make certs` leaves in the certs directory, made in a directory of the
// test rather than in the working tree, so that the tests need neither openssl
// nor a `make certs` of their own.
package testcerts

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Set is where the keys and the certificates of a test were written down,
// named after the files `make certs` leaves behind.
type Set struct {
	// CA is the authority that signed the other two, and the one a client
	// certificate is expected to be signed by.
	CA string
	// ServerCert and ServerKey are what the api server serves TLS with.
	ServerCert string
	ServerKey  string
	// ClientCert and ClientKey are what a caller authenticates with.
	ClientCert string
	ClientKey  string
}

// certificate is a certificate and the private key it belongs to, in PEM.
type certificate struct {
	cert, key []byte
}

// certificates is a certificate authority along with the server and the client
// certificates it has signed.
type certificates struct {
	authority certificate
	server    certificate
	client    certificate
}

// fixture is the certificate set of the whole test binary.  Every test wants
// the same three keys, so they are made once, and each test writes them into a
// directory of its own.
var fixture = sync.OnceValues(newCertificates)

// Write is the certificate set of the test binary, written into dir.
func Write(t *testing.T, dir string) Set {
	t.Helper()

	set, err := fixture()
	if err != nil {
		t.Fatalf("cannot generate the test certificates: %v", err)
	}

	return set.write(t, dir)
}

// Foreign is a certificate set of an authority of its own, for a caller the
// api server has never heard of.  It is written into dir like Write is.
func Foreign(t *testing.T, dir string) Set {
	t.Helper()

	set, err := newCertificates()
	if err != nil {
		t.Fatalf("cannot generate the foreign certificates: %v", err)
	}

	return set.write(t, dir)
}

// write puts the certificates down as files, and tells where to find them.
func (c certificates) write(t *testing.T, dir string) Set {
	t.Helper()

	for name, cert := range map[string]certificate{
		"ca":     c.authority,
		"server": c.server,
		"client": c.client,
	} {
		writeFile(t, filepath.Join(dir, name+".crt"), cert.cert)
		writeFile(t, filepath.Join(dir, name+".key"), cert.key)
	}

	return Set{
		CA:         filepath.Join(dir, "ca.crt"),
		ServerCert: filepath.Join(dir, "server.crt"),
		ServerKey:  filepath.Join(dir, "server.key"),
		ClientCert: filepath.Join(dir, "client.crt"),
		ClientKey:  filepath.Join(dir, "client.key"),
	}
}

// Pool is the certificate authority in the file at path, which is what a
// certificate is verified against.
func Pool(t *testing.T, path string) *x509.CertPool {
	t.Helper()

	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the certificate authority %q: %v", path, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("the certificate authority %q holds no certificates", path)
	}

	return pool
}

// KeyPair is the certificate at cert with the key at key.
func KeyPair(t *testing.T, cert, key string) tls.Certificate {
	t.Helper()

	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("cannot read the certificate %q with the key %q: %v", cert, key, err)
	}

	return pair
}

// Server is the api server's half of the set: what it serves TLS with, and the
// authorities a caller is expected to bring a certificate of.
func Server(t *testing.T, set Set) *tls.Config {
	t.Helper()

	return &tls.Config{
		Certificates: []tls.Certificate{KeyPair(t, set.ServerCert, set.ServerKey)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    Pool(t, set.CA),
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
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

// issued is a certificate with the key it was issued for, before either of them
// is written out.
type issued struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
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
