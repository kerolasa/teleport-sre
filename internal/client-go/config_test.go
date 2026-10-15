package clientgo

import (
	"strings"
	"testing"

	"github.com/kerolasa/teleport-sre/internal"
)

func TestAPIURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		conf internal.Conf
		url  string
		// refused is what the refusal has to mention.
		refused string
	}{
		{
			name: "the host the configuration names",
			conf: internal.Conf{API: internal.Listener{Host: "teleport.example.com", Port: 8443}},
			url:  "https://teleport.example.com:8443/",
		},
		{
			// The api server issues its certificate for a name rather than for
			// an address, so this is not loopback served.
			name: "the host under its address",
			conf: internal.Conf{API: internal.Listener{Host: "127.0.0.1", Port: 8443}},
			url:  "https://127.0.0.1:8443/",
		},
		{
			// The sample configuration serves the api on every address of the
			// host, where a caller on it finds it.
			name: "a listener on every address of the host",
			conf: internal.Conf{API: internal.Listener{Host: "0.0.0.0", Port: 8443}},
			url:  "https://localhost:8443/",
		},
		{
			name: "a listener on every address of the host, in its own spelling",
			conf: internal.Conf{API: internal.Listener{Host: "::", Port: 8443}},
			url:  "https://localhost:8443/",
		},
		{
			name: "a configuration that names no host",
			conf: internal.Conf{API: internal.Listener{Port: 8443}},
			url:  "https://localhost:8443/",
		},
		{
			name: "the prefix the api is mounted at",
			conf: internal.Conf{API: internal.Listener{Host: "localhost", Port: 8443, Prefix: "/teleport"}},
			url:  "https://localhost:8443/teleport",
		},
		{
			name: "a prefix with no leading slash",
			conf: internal.Conf{API: internal.Listener{Host: "localhost", Port: 8443, Prefix: "teleport"}},
			url:  "https://localhost:8443/teleport",
		},
		{
			name:    "a configuration that names no port",
			conf:    internal.Conf{API: internal.Listener{Host: "localhost"}},
			refused: "nothing about the api port",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			url, err := apiURL(test.conf)
			if test.refused != "" {
				if err == nil {
					t.Fatalf("apiURL() is ready to reach %s, want it to refuse a configuration with no port", url)
				}
				if got := err.Error(); !strings.Contains(got, test.refused) {
					t.Errorf("apiURL() said %q, want it to mention %q", got, test.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("apiURL() returned an error: %v", err)
			}
			if got := url.String(); got != test.url {
				t.Errorf("apiURL() is %q, want %q", got, test.url)
			}
		})
	}
}
