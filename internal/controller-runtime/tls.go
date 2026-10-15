package controllerruntime

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// tlsConfig is what the api server terminates TLS with: TLS 1.3, a
// certificate for the address the caller reaches, and a client certificate
// signed by the same certificate authority.
func tlsConfig(conf TLSConf) (*tls.Config, error) {
	for _, setting := range []struct{ name, path string }{
		{"ca", conf.CA},
		{"cert", conf.Cert},
		{"key", conf.Key},
	} {
		if setting.path == "" {
			return nil, fmt.Errorf("the configuration says nothing about the tls %s", setting.name)
		}
	}

	certificate, err := tls.LoadX509KeyPair(conf.Cert, conf.Key)
	if err != nil {
		return nil, fmt.Errorf("cannot use the certificate %q with the key %q: %w",
			conf.Cert, conf.Key, err)
	}

	authorities, err := readAuthority(conf.CA)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    authorities,
	}, nil
}

// readAuthority is the pool of certificate authorities a client certificate
// is expected to be signed by.
func readAuthority(path string) (*x509.CertPool, error) {
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
