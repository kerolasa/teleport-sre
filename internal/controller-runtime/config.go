package controllerruntime

import (
	"github.com/BurntSushi/toml"
)

type Conf struct {
	API      Listener
	Metrics  Listener
	Kube     KubeConf
	TLS      TLSConf
	Loglevel string
	// MaxReplicas is the largest number of replicas a request is allowed to
	// scale a deployment to.  An empty setting gets the default, and a negative
	// one lifts the limit.
	MaxReplicas int32 `toml:"max_replicas"`
}

type Listener struct {
	Host   string
	Port   uint16
	Prefix string // where the api is mounted
	Path   string // where the metrics are
}

// TLSConf tells where the keys and the certificates the api server terminates
// TLS with are.  All three are needed, since a service that cannot tell its
// callers apart has no business serving them.
type TLSConf struct {
	// CA is the certificate authority that signed the server certificate,
	// and the one a client certificate is expected to be signed by.
	CA string
	// Cert is the certificate the api server serves.
	Cert string
	// Key is the private key of the server certificate.
	Key string
}

// KubeConf tells how to reach the kubernetes api server.  An empty one leaves
// the discovery to the application, which then behaves like kubectl does.
type KubeConf struct {
	// Kubeconfig is the path of the kubeconfig to authenticate with.
	Kubeconfig string
	// Context is the kubeconfig context to use, or its current one.
	Context string
}

func ReadConfig(path string) (Conf, error) {
	var conf Conf

	_, err := toml.DecodeFile(path, &conf)
	if err != nil {
		return conf, err
	}

	return conf, nil
}
