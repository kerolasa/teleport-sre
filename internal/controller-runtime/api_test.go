package controllerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestAPI starts the api on a fake cluster behind a filled cache.
func newTestAPI(t *testing.T, objects ...runtime.Object) *httptest.Server {
	t.Helper()

	client := fake.NewClientset(objects...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	return serveAPI(t, KubeConn{k8s: client}, cache)
}

// newColdTestAPI starts the api behind a cache that is not filled.
func newColdTestAPI(t *testing.T, objects ...runtime.Object) *httptest.Server {
	t.Helper()

	client := fake.NewClientset(objects...)
	cache := NewCache(KubeConn{k8s: client})
	if cache.Populated() {
		t.Fatal("the cache reports itself as filled before it has been filled")
	}

	return serveAPI(t, KubeConn{k8s: client}, cache)
}

// serveAPI starts the api on a loopback address and returns where it is.
func serveAPI(t *testing.T, kubeConn KubeConn, cache *Cache) *httptest.Server {
	t.Helper()

	conf := Conf{API: Listener{Host: "127.0.0.1", Prefix: "/"}}
	server := ListenAPI(conf, kubeConn, cache)
	api := httptest.NewServer(server.Handler)
	t.Cleanup(api.Close)

	return api
}

// testCluster is the fixture: three namespaces, two with deployments, and a
// deployment name used in two namespaces.
func testCluster() []runtime.Object {
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty"}},
		deployment("web", "front", 3),
		deployment("web", "cdn", 2),
		deployment("api", "backend", 1),
		deployment("web", "shared", 2),
		deployment("api", "shared", 1),
	}
}

// answer is what the api was asked with, and what it said back.
type answer struct {
	status      int
	contentType string
	body        string
}

// get asks the api for the path.
func get(t *testing.T, api *httptest.Server, path string) answer {
	t.Helper()

	response, err := http.Get(api.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("closing the body of GET %s: %v", path, err)
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("cannot read the body of GET %s: %v", path, err)
	}

	return answer{
		status:      response.StatusCode,
		contentType: response.Header.Get("Content-Type"),
		body:        string(body),
	}
}

// asJSON is the body with the insignificant space taken out.
func (a answer) asJSON(t *testing.T) string {
	t.Helper()

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(a.body)); err != nil {
		t.Fatalf("the body %q is not json: %v", a.body, err)
	}

	return compact.String()
}

func TestAPIList(t *testing.T) {
	t.Parallel()

	all := `{"namespaces":[{"name":"api"},{"name":"empty"},{"name":"web"}],` +
		`"deployments":[{"namespace":"api","name":"backend","replicas":1},` +
		`{"namespace":"api","name":"shared","replicas":1},` +
		`{"namespace":"web","name":"cdn","replicas":2},` +
		`{"namespace":"web","name":"front","replicas":3},` +
		`{"namespace":"web","name":"shared","replicas":2}]}`

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "every namespace",
			path: "/list",
			want: all,
		},
		{
			name: "one namespace",
			path: "/list?ns=web",
			want: `{"deployments":[{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "namespace without deployments",
			path: "/list?ns=empty",
			want: `{"deployments":[]}`,
		},
		{
			name: "namespace spelled out",
			path: "/list?namespace=web",
			want: `{"deployments":[{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "both spellings of the namespace",
			path: "/list?ns=web&namespace=api",
			want: `{"deployments":[{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
	}

	api := newTestAPI(t, testCluster()...)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := get(t, api, test.path)
			if got.status != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d (%s)", test.path, got.status, http.StatusOK, got.body)
			}
			if !strings.HasPrefix(got.contentType, "application/json") {
				t.Errorf("GET %s content type = %q, want json", test.path, got.contentType)
			}
			if body := got.asJSON(t); body != test.want {
				t.Errorf("GET %s = %s, want %s", test.path, body, test.want)
			}
		})
	}
}

func TestAPIListWithoutTheNamespace(t *testing.T) {
	t.Parallel()

	// A namespace not in the cluster is missing rather than empty.
	got := get(t, newTestAPI(t, testCluster()...), "/list?ns=nope")
	if got.status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusNotFound, got.body)
	}
	want := `{"error":"namespace \"nope\" does not exist"}`
	if body := got.asJSON(t); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestAPIListWithAColdCache(t *testing.T) {
	t.Parallel()

	// An unfilled cache says nothing, so the caller is told to come back.
	got := get(t, newColdTestAPI(t, testCluster()...), "/list")
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusServiceUnavailable, got.body)
	}
	want := `{"error":"` + cachePending + `"}`
	if body := got.asJSON(t); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	if !strings.Contains(got.body, "later") {
		t.Errorf("body = %q, want it to tell the caller to come back later", got.body)
	}
}

func TestAPIDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "one deployment",
			path: "/deployment?ns=web&name=front",
			want: `{"namespace":"web","name":"front","replicas":3}`,
		},
		{
			name: "one deployment of the whole cluster",
			path: "/deployment?name=cdn",
			want: `{"namespace":"web","name":"cdn","replicas":2}`,
		},
		{
			name: "the deployments of a namespace",
			path: "/deployment?ns=web",
			want: `{"deployments":[{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "every deployment",
			path: "/deployment",
			want: `{"deployments":[{"namespace":"api","name":"backend","replicas":1},` +
				`{"namespace":"api","name":"shared","replicas":1},` +
				`{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "namespace without deployments",
			path: "/deployment?ns=empty",
			want: `{"deployments":[]}`,
		},
		{
			name: "the same name in two namespaces",
			path: "/deployment?name=shared",
			want: `{"deployments":[{"namespace":"api","name":"shared","replicas":1},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "the same name in the namespace that was asked about",
			path: "/deployment?ns=web&name=shared",
			want: `{"namespace":"web","name":"shared","replicas":2}`,
		},
		{
			name: "namespace spelled out",
			path: "/deployment?namespace=web",
			want: `{"deployments":[{"namespace":"web","name":"cdn","replicas":2},` +
				`{"namespace":"web","name":"front","replicas":3},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "namespace spelled out with a name",
			path: "/deployment?namespace=web&name=front",
			want: `{"namespace":"web","name":"front","replicas":3}`,
		},
	}

	api := newTestAPI(t, testCluster()...)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := get(t, api, test.path)
			if got.status != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d (%s)", test.path, got.status, http.StatusOK, got.body)
			}
			if !strings.HasPrefix(got.contentType, "application/json") {
				t.Errorf("GET %s content type = %q, want json", test.path, got.contentType)
			}
			if body := got.asJSON(t); body != test.want {
				t.Errorf("GET %s = %s, want %s", test.path, body, test.want)
			}
		})
	}
}

func TestAPIDeploymentNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "namespace that is not there",
			path: "/deployment?ns=nope&name=front",
			want: `{"error":"namespace \"nope\" does not exist"}`,
		},
		{
			name: "namespace without the deployment",
			path: "/deployment?ns=web&name=nope",
			want: `{"error":"deployment \"nope\" does not exist in namespace \"web\""}`,
		},
		{
			name: "deployment that is not there at all",
			path: "/deployment?name=nope",
			want: `{"error":"deployment \"nope\" does not exist"}`,
		},
		{
			name: "deployment of another namespace",
			// The namespace was wrong rather than the deployment gone.
			path: "/deployment?ns=web&name=backend",
			want: `{"error":"deployment \"backend\" does not exist in namespace \"web\""}`,
		},
		{
			name: "namespace spelled out that is not there",
			path: "/deployment?namespace=nope&name=front",
			want: `{"error":"namespace \"nope\" does not exist"}`,
		},
		{
			name: "namespace spelled out without the deployment",
			path: "/deployment?namespace=web&name=nope",
			want: `{"error":"deployment \"nope\" does not exist in namespace \"web\""}`,
		},
	}

	api := newTestAPI(t, testCluster()...)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := get(t, api, test.path)
			if got.status != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want %d (%s)", test.path, got.status, http.StatusNotFound, got.body)
			}
			if body := got.asJSON(t); body != test.want {
				t.Errorf("GET %s = %s, want %s", test.path, body, test.want)
			}
		})
	}
}

func TestAPIDeploymentWithAColdCache(t *testing.T) {
	t.Parallel()

	// The one answer that must not claim the deployment is gone.
	api := newColdTestAPI(t, testCluster()...)
	for _, path := range []string{
		"/deployment?ns=web&name=front",
		"/deployment?namespace=web&name=front",
		"/deployment?ns=web",
		"/deployment?name=front",
		"/deployment",
		"/deployment?ns=nope&name=front",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			got := get(t, api, path)
			if got.status != http.StatusServiceUnavailable {
				t.Fatalf("GET %s status = %d, want %d (%s)", path, got.status, http.StatusServiceUnavailable, got.body)
			}
			if body := got.asJSON(t); body != `{"error":"`+cachePending+`"}` {
				t.Errorf("GET %s = %s, want the cache to be reported as not ready", path, body)
			}
		})
	}
}

func TestAPIUIList(t *testing.T) {
	t.Parallel()

	// The web ui swaps a fragment in, so it has a route of its own.
	api := newTestAPI(t, testCluster()...)
	got := get(t, api, "/ui/list")
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if !strings.HasPrefix(got.contentType, "text/html") {
		t.Errorf("content type = %q, want html", got.contentType)
	}
	for _, want := range []string{`<table id="deployments">`, "web", "front", "3"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the fragment does not contain %q:\n%s", want, got.body)
		}
	}
}

func TestAPIRootPageAsksTheFragmentRoute(t *testing.T) {
	t.Parallel()

	// The list button asks the fragment route.
	got := get(t, newTestAPI(t, testCluster()...), "/")
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if !strings.Contains(got.body, `hx-get="/ui/list"`) {
		t.Errorf("the root page does not ask the fragment route:\n%s", got.body)
	}
}

func TestAPIRoutesFollowThePrefix(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(testCluster()...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	conf := Conf{API: Listener{Host: "127.0.0.1", Prefix: "/teleport"}}
	api := httptest.NewServer(ListenAPI(conf, KubeConn{k8s: client}, cache).Handler)
	t.Cleanup(api.Close)

	// The routes answer under the configured prefix, and nowhere else.
	got := get(t, api, "/teleport/list")
	if got.status != http.StatusOK {
		t.Errorf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if status := get(t, api, "/list").status; status != http.StatusNotFound {
		t.Errorf("the unprefixed route answered with %d, want it not to be served", status)
	}
}
