package controllerruntime

import (
	"crypto/tls"

	"github.com/kerolasa/teleport-sre/internal"
)

// tlsConfig is what the api server terminates TLS with: TLS 1.3, a certificate
// for the address the caller reaches, and a client certificate signed by the
// same certificate authority.
func tlsConfig(conf TLSConf) (*tls.Config, error) {
	if err := internal.Configured(
		internal.Setting{Name: "tls ca", Path: conf.CA},
		internal.Setting{Name: "tls cert", Path: conf.Cert},
		internal.Setting{Name: "tls key", Path: conf.Key},
	); err != nil {
		return nil, err
	}

	secure, err := internal.MutualTLS(conf.Cert, conf.Key, conf.CA)
	if err != nil {
		return nil, err
	}

	// A caller the api server cannot tell from anyone else has no business
	// being served at all.
	secure.ClientAuth = tls.RequireAndVerifyClientCert

	return secure, nil
}
