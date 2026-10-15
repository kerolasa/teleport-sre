package clientgo

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kerolasa/teleport-sre/internal"
	"github.com/kerolasa/teleport-sre/internal/testcerts"
)

// asked is a request the api received.
type asked struct {
	method string
	path   string
	query  url.Values
}

// startAPI serves the api over mTLS on a loopback address, answering everything
// with the status and the body given, and hands back the requests it received.
func startAPI(t *testing.T, set testcerts.Set, status int, body string) (*httptest.Server, chan asked) {
	t.Helper()

	received := make(chan asked, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- asked{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("cannot write the answer of the api: %v", err)
		}
	}))
	server.TLS = testcerts.Server(t, set)
	// A caller the test turns away on purpose, so the complaint of the server
	// is of no interest to anyone.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)

	return server, received
}

// callerConf is the configuration of a client reaching the api at address.
func callerConf(t *testing.T, set testcerts.Set, address string) internal.Conf {
	t.Helper()

	served, err := url.Parse(address)
	if err != nil {
		t.Fatalf("cannot make sense of the address %q: %v", address, err)
	}
	port, err := strconv.Atoi(served.Port())
	if err != nil {
		t.Fatalf("the address %q carries no port: %v", address, err)
	}

	return internal.Conf{
		API:    internal.Listener{Host: served.Hostname(), Port: uint16(port), Prefix: "/"},
		TLS:    internal.TLSConf{CA: set.CA},
		Client: internal.Identity{Cert: set.ClientCert, Key: set.ClientKey},
	}
}

func TestClientAsksTheApi(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ask  func(client *client) error
		want asked
	}{
		{
			name: "every namespace and deployment there is",
			ask:  func(client *client) error { return client.list("") },
			want: asked{method: http.MethodGet, path: "/list", query: url.Values{}},
		},
		{
			name: "the deployments of a namespace",
			ask:  func(client *client) error { return client.list("web") },
			want: asked{method: http.MethodGet, path: "/list", query: url.Values{"ns": {"web"}}},
		},
		{
			name: "the replicas of every deployment there is",
			ask:  func(client *client) error { return client.get("", "") },
			want: asked{method: http.MethodGet, path: "/deployment", query: url.Values{}},
		},
		{
			name: "the replicas of a deployment",
			ask:  func(client *client) error { return client.get("web", "front") },
			want: asked{
				method: http.MethodGet,
				path:   "/deployment",
				query:  url.Values{"ns": {"web"}, "deployment": {"front"}},
			},
		},
		{
			name: "the replicas of a deployment in every namespace",
			ask:  func(client *client) error { return client.get("", "front") },
			want: asked{method: http.MethodGet, path: "/deployment", query: url.Values{"deployment": {"front"}}},
		},
		{
			name: "a request to scale a deployment",
			ask:  func(client *client) error { return client.set("web", "front", 3) },
			want: asked{
				method: http.MethodPut,
				path:   "/deployment",
				query:  url.Values{"ns": {"web"}, "deployment": {"front"}, "replicas": {"3"}},
			},
		},
		{
			// No replicas is a number of replicas, rather than a part of the
			// request left out.
			name: "a request to scale a deployment to nothing",
			ask:  func(client *client) error { return client.set("web", "front", 0) },
			want: asked{
				method: http.MethodPut,
				path:   "/deployment",
				query:  url.Values{"ns": {"web"}, "deployment": {"front"}, "replicas": {"0"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			set := testcerts.Write(t, t.TempDir())
			api, received := startAPI(t, set, http.StatusOK, `{"status":"done"}`)

			out := &bytes.Buffer{}
			client, err := newClient(callerConf(t, set, api.URL), out)
			if err != nil {
				t.Fatalf("newClient() returned an error: %v", err)
			}
			if err := test.ask(client); err != nil {
				t.Fatalf("the client was refused: %v", err)
			}

			got := <-received
			if got.method != test.want.method || got.path != test.want.path {
				t.Errorf("the client asked for %s %s, want %s %s",
					got.method, got.path, test.want.method, test.want.path)
			}
			if got.query.Encode() != test.want.query.Encode() {
				t.Errorf("the client asked with the query %q, want %q",
					got.query.Encode(), test.want.query.Encode())
			}
		})
	}
}

func TestClientAsksTheApiUnderItsPrefix(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	api, received := startAPI(t, set, http.StatusOK, `{}`)

	conf := callerConf(t, set, api.URL)
	conf.API.Prefix = "/teleport"
	client, err := newClient(conf, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newClient() returned an error: %v", err)
	}
	if err := client.list(""); err != nil {
		t.Fatalf("the client was refused: %v", err)
	}

	if got, want := (<-received).path, "/teleport/list"; got != want {
		t.Errorf("the client asked for %q, want %q", got, want)
	}
}

func TestClientWritesTheAnswerAsItCame(t *testing.T) {
	t.Parallel()

	const answer = `{"deployments":[{"namespace":"web","name":"front","replicas":3}]}`

	tests := []struct {
		name   string
		status int
		// refused is whether the answer is a refusal.
		refused bool
	}{
		{name: "an answer with a reason of its own", status: http.StatusOK},
		// A refusal is written out as well, since that is where its reason is.
		{name: "an answer the api refused the request with", status: http.StatusNotFound, refused: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			set := testcerts.Write(t, t.TempDir())
			api, _ := startAPI(t, set, test.status, answer)

			out := &bytes.Buffer{}
			client, err := newClient(callerConf(t, set, api.URL), out)
			if err != nil {
				t.Fatalf("newClient() returned an error: %v", err)
			}

			err = client.list("web")
			if test.refused && err == nil {
				t.Fatal("the client took the refusal of the api for an answer")
			}
			if !test.refused && err != nil {
				t.Fatalf("the client was refused: %v", err)
			}

			if got := out.String(); got != answer+"\n" {
				t.Errorf("the client wrote %q, want the answer as the api sent it", got)
			}
		})
	}
}

func TestClientReportsWhatTheApiRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		// complained is what the failure has to mention.
		complained string
	}{
		{
			name:       "a deployment that is not there",
			status:     http.StatusNotFound,
			body:       `{"error":"deployment \"front\" does not exist in namespace \"web\""}`,
			complained: `404 Not Found: deployment "front" does not exist in namespace "web"`,
		},
		{
			name:       "a replica count over the limit",
			status:     http.StatusNotAcceptable,
			body:       `{"error":"replicas 1000 is over the limit of 100"}`,
			complained: "406 Not Acceptable: replicas 1000 is over the limit of 100",
		},
		{
			// A reason the api does not give is still a failure to report, and
			// the status is all there is to go by.
			name:       "an answer that says nothing about why",
			status:     http.StatusBadGateway,
			body:       "<html>a gateway that is having a bad day</html>",
			complained: "502 Bad Gateway",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			set := testcerts.Write(t, t.TempDir())
			api, _ := startAPI(t, set, test.status, test.body)

			client, err := newClient(callerConf(t, set, api.URL), &bytes.Buffer{})
			if err != nil {
				t.Fatalf("newClient() returned an error: %v", err)
			}

			err = client.set("web", "front", 3)
			if err == nil {
				t.Fatal("the client is happy with an answer that refused the request")
			}
			if !strings.Contains(err.Error(), test.complained) {
				t.Errorf("the client said %q, want it to mention %q", err, test.complained)
			}
		})
	}
}

func TestClientWantsACertificateOfItsOwn(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	api, received := startAPI(t, set, http.StatusOK, `{}`)
	// An authority of its own, for a caller the api has never heard of.
	foreign := testcerts.Foreign(t, t.TempDir())

	conf := callerConf(t, set, api.URL)
	conf.Client = internal.Identity{Cert: foreign.ClientCert, Key: foreign.ClientKey}

	client, err := newClient(conf, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newClient() returned an error: %v", err)
	}

	err = client.list("")
	if err == nil {
		t.Fatal("the api answered a caller another authority signed for")
	}
	if !strings.Contains(err.Error(), "cannot reach the api") {
		t.Errorf("the client said %q, want it to mention that the api is out of reach", err)
	}
	if len(received) > 0 {
		t.Error("the api was asked something by a caller it should have turned away")
	}
}

func TestNewClientWithoutUsableKeys(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	keys := internal.Conf{
		API:    internal.Listener{Host: "localhost", Port: 8443},
		TLS:    internal.TLSConf{CA: set.CA},
		Client: internal.Identity{Cert: set.ClientCert, Key: set.ClientKey},
	}

	// A file that is not a certificate, for the authority to be told off over.
	junk := filepath.Join(t.TempDir(), "junk.crt")
	if err := os.WriteFile(junk, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("cannot write %q: %v", junk, err)
	}

	tests := []struct {
		name string
		conf internal.Conf
		// complained is what the failure has to mention.
		complained string
	}{
		{
			// A caller the api server cannot tell apart is turned away before
			// it answers anything, so there is nothing gained by carrying on.
			name:       "no keys at all",
			conf:       internal.Conf{API: keys.API},
			complained: "nothing about the tls ca",
		},
		{
			name:       "no certificate to authenticate with",
			conf:       internal.Conf{API: keys.API, TLS: keys.TLS},
			complained: "nothing about the client certificate",
		},
		{
			name: "no key for the certificate",
			conf: internal.Conf{
				API:    keys.API,
				TLS:    keys.TLS,
				Client: internal.Identity{Cert: keys.Client.Cert},
			},
			complained: "nothing about the client key",
		},
		{
			name: "a certificate that is not there",
			conf: internal.Conf{
				API:    keys.API,
				TLS:    keys.TLS,
				Client: internal.Identity{Cert: filepath.Join(t.TempDir(), "gone.crt"), Key: keys.Client.Key},
			},
			complained: "cannot use the certificate",
		},
		{
			name: "an authority with no certificates in it",
			conf: internal.Conf{
				API:    keys.API,
				TLS:    internal.TLSConf{CA: junk},
				Client: keys.Client,
			},
			complained: "holds no certificates",
		},
		{
			name: "a configuration that names no port",
			conf: internal.Conf{
				TLS:    keys.TLS,
				Client: keys.Client,
			},
			complained: "nothing about the api port",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client, err := newClient(test.conf, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("newClient() is ready to reach the api with %+v, want it to refuse", client.http)
			}
			if !strings.Contains(err.Error(), test.complained) {
				t.Errorf("newClient() said %q, want it to mention %q", err, test.complained)
			}
		})
	}
}

func TestClientCannotReachAnApiThatIsNotThere(t *testing.T) {
	t.Parallel()

	set := testcerts.Write(t, t.TempDir())
	// An address nothing listens on.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	client, err := newClient(callerConf(t, set, closed.URL), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newClient() returned an error: %v", err)
	}

	err = client.list("")
	if err == nil {
		t.Fatal("the client is happy with an api that is not there")
	}
	if !strings.Contains(err.Error(), "cannot reach the api") {
		t.Errorf("the client said %q, want it to mention that the api is out of reach", err)
	}
}
