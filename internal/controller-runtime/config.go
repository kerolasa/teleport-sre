package controllerruntime

import (
	"github.com/BurntSushi/toml"
)

type Conf struct {
	API      Listener
	Metrics  Listener
	Kube     KubeConf
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
