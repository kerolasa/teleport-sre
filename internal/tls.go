package internal

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Setting is a file the configuration names, and what the tool reading it calls
// the setting.
type Setting struct {
	// Name is what the setting is called in a complaint about it going missing.
	Name string
	// Path is where the file is, empty when the configuration is silent on it.
	Path string
}

// Configured is a complaint about the configuration naming none of what is
// needed of it.
func Configured(settings ...Setting) error {
	for _, setting := range settings {
		if setting.Path == "" {
			return fmt.Errorf("the configuration says nothing about the %s", setting.Name)
		}
	}

	return nil
}

// MutualTLS is what the api server and one of its callers secure the connection
// between them with: TLS 1.3, a certificate of their own, and the authority
// the other end is verified against.  Both ends are given the same settings, and
// a server asks for a certificate of the caller's own on top of it.
func MutualTLS(cert, key, ca string) (*tls.Config, error) {
	pair, err := certificate(cert, key)
	if err != nil {
		return nil, err
	}

	authorities, err := ReadAuthority(ca)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      authorities,
		ClientCAs:    authorities,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}, nil
}

func certificate(cert, key string) (tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot use the certificate %q with the key %q: %w",
			cert, key, err)
	}

	return pair, nil
}

// ReadAuthority is the pool of certificate authorities a certificate is
// expected to be signed by.
func ReadAuthority(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the certificate authority %q: %w", path, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the certificate authority %q holds no certificates", path)
	}

	return pool, nil
}
