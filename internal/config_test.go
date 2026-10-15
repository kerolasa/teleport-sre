package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadConfigMaxReplicas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// written is the configuration file.
		written string
		want    int32
	}{
		{
			name:    "the limit the configuration gives",
			written: "max_replicas = 7\n",
			want:    7,
		},
		{
			name:    "a limit that is lifted",
			written: "max_replicas = -1\n",
			want:    -1,
		},
		{
			// Left out, it is left to the default.
			name:    "a configuration that leaves it out",
			written: "loglevel = \"debug\"\n",
			want:    0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			conf := readConfig(t, test.written)
			if conf.MaxReplicas != test.want {
				t.Errorf("ReadConfig() read %d, want %d", conf.MaxReplicas, test.want)
			}
		})
	}
}

func TestReadConfigOfTheSettingsEachBinaryHasAUseFor(t *testing.T) {
	t.Parallel()

	// The api server and the callers of it read one file, and each of them
	// leaves the settings of the other alone.
	conf := readConfig(t, `
loglevel = "info"
max_replicas = 100

[api]
    host = "0.0.0.0"
    port = 8443
    prefix = "/teleport"

[metrics]
    host = "127.0.0.1"
    port = 9443
    path = "/metrics"

[kube]
    kubeconfig = "~/.kube/config"
    context = "kind-kind"

[tls]
    ca = "certs/ca.crt"
    cert = "certs/server.crt"
    key = "certs/server.key"

[client]
    cert = "certs/client.crt"
    key = "certs/client.key"
`)

	if conf.Loglevel != "info" {
		t.Errorf("ReadConfig() read the log level %q, want %q", conf.Loglevel, "info")
	}
	if want := (Listener{Host: "0.0.0.0", Port: 8443, Prefix: "/teleport"}); conf.API != want {
		t.Errorf("ReadConfig() read the api listener %+v, want %+v", conf.API, want)
	}
	if want := (Listener{Host: "127.0.0.1", Port: 9443, Path: "/metrics"}); conf.Metrics != want {
		t.Errorf("ReadConfig() read the metrics listener %+v, want %+v", conf.Metrics, want)
	}
	if want := (KubeConf{Kubeconfig: "~/.kube/config", Context: "kind-kind"}); conf.Kube != want {
		t.Errorf("ReadConfig() read the kubernetes settings %+v, want %+v", conf.Kube, want)
	}
	if want := (TLSConf{CA: "certs/ca.crt", Cert: "certs/server.crt", Key: "certs/server.key"}); conf.TLS != want {
		t.Errorf("ReadConfig() read the tls settings %+v, want %+v", conf.TLS, want)
	}
	if want := (Identity{Cert: "certs/client.crt", Key: "certs/client.key"}); conf.Client != want {
		t.Errorf("ReadConfig() read the client settings %+v, want %+v", conf.Client, want)
	}
}

func TestReadConfigOfAFileThatIsNotThere(t *testing.T) {
	t.Parallel()

	if _, err := ReadConfig(filepath.Join(t.TempDir(), "gone.toml")); err == nil {
		t.Error("ReadConfig() read a configuration that is not there")
	}
}

func TestSampleConfigIsReadable(t *testing.T) {
	t.Parallel()

	// The sample configuration is in the repository, so a setting that does
	// nothing is a setting that is of no use to anyone.
	conf, err := ReadConfig(filepath.Join("..", "sample-config.toml"))
	if err != nil {
		t.Fatalf("ReadConfig() returned an error: %v", err)
	}

	if got, want := conf.MaxReplicas, int32(100); got != want {
		t.Errorf("the sample configuration scales to %d, want the %d it says", got, want)
	}
	if conf.Loglevel == "" {
		t.Error("the sample configuration says nothing about the log level")
	}
	if got, want := conf.API, (Listener{Host: "0.0.0.0", Port: 8443, Prefix: "/"}); got != want {
		t.Errorf("the sample configuration serves the api from %+v, want %+v", got, want)
	}
	if got, want := conf.Metrics, (Listener{Host: "127.0.0.1", Port: 9443, Path: "/metrics"}); got != want {
		t.Errorf("the sample configuration serves metrics from %+v, want %+v", got, want)
	}
	if got, want := conf.TLS, (TLSConf{
		CA:   "certs/ca.crt",
		Cert: "certs/server.crt",
		Key:  "certs/server.key",
	}); got != want {
		t.Errorf("the sample configuration is served with %+v, want %+v", got, want)
	}
	if got, want := conf.Client, (Identity{Cert: "certs/client.crt", Key: "certs/client.key"}); got != want {
		t.Errorf("the sample configuration authenticates with %+v, want %+v", got, want)
	}
}

// readConfig is the configuration at a file of its own holding what is written.
func readConfig(t *testing.T, written string) Conf {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("cannot write the configuration: %v", err)
	}

	conf, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig() returned an error: %v", err)
	}

	return conf
}
