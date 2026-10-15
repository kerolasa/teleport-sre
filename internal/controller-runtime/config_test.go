package controllerruntime

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

			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.written), 0o600); err != nil {
				t.Fatalf("cannot write the configuration: %v", err)
			}

			conf, err := ReadConfig(path)
			if err != nil {
				t.Fatalf("ReadConfig() returned an error: %v", err)
			}
			if conf.MaxReplicas != test.want {
				t.Errorf("ReadConfig() read %d, want %d", conf.MaxReplicas, test.want)
			}

			if test.want == 0 {
				if got := configuredMaxReplicas(conf); got != defaultMaxReplicas {
					t.Errorf("the service would scale to %d, want the default of %d", got, defaultMaxReplicas)
				}
			}
		})
	}
}

func TestSampleConfigIsReadable(t *testing.T) {
	t.Parallel()

	// The sample configuration is in the repository, so a setting the
	// service does not read is a setting that does nothing.
	conf, err := ReadConfig(filepath.Join("..", "..", "sample-config.toml"))
	if err != nil {
		t.Fatalf("ReadConfig() returned an error: %v", err)
	}

	if got, want := configuredMaxReplicas(conf), int32(100); got != want {
		t.Errorf("the sample configuration scales to %d, want the %d it says", got, want)
	}
	if got, want := conf.API.Host, "0.0.0.0"; got != want {
		t.Errorf("the sample configuration listens on %q, want %q", got, want)
	}
	if got, want := conf.API.Port, uint16(8443); got != want {
		t.Errorf("the sample configuration listens on port %d, want %d", got, want)
	}
	if got, want := conf.Metrics.Path, "/metrics"; got != want {
		t.Errorf("the sample configuration serves metrics at %q, want %q", got, want)
	}
	if got, want := conf.TLS.CA, "certs/ca.crt"; got != want {
		t.Errorf("the sample configuration verifies callers with %q, want %q", got, want)
	}
	if got, want := conf.TLS.Cert, "certs/server.crt"; got != want {
		t.Errorf("the sample configuration serves the certificate %q, want %q", got, want)
	}
	if got, want := conf.TLS.Key, "certs/server.key"; got != want {
		t.Errorf("the sample configuration serves the key %q, want %q", got, want)
	}
	if conf.Loglevel == "" {
		t.Error("the sample configuration says nothing about the log level")
	}
}
