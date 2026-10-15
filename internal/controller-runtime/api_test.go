package controllerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newTestAPI starts the api on a fake cluster behind a filled cache.
func newTestAPI(t *testing.T, objects ...runtime.Object) *httptest.Server {
	t.Helper()

	server, _, _ := apiOverCluster(t, Conf{}, objects...)
	return server
}

// newColdTestAPI starts the api behind a cache that is not filled.
func newColdTestAPI(t *testing.T, objects ...runtime.Object) *httptest.Server {
	t.Helper()

	client := fake.NewClientset(objects...)
	cache := NewCache(KubeConn{k8s: client})
	if cache.Populated() {
		t.Fatal("the cache reports itself as filled before it has been filled")
	}

	return serveAPI(t, Conf{API: Listener{Prefix: "/"}}, KubeConn{k8s: client}, cache)
}

// apiOverCluster starts the api on conf over a fake cluster behind a filled
// cache, and hands the cluster and the cache back with it.
func apiOverCluster(t *testing.T, conf Conf, objects ...runtime.Object) (*httptest.Server, *fake.Clientset, *Cache) {
	t.Helper()

	client := fake.NewClientset(objects...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	return serveAPI(t, conf, KubeConn{k8s: client}, cache), client, cache
}

// serveAPI starts the api on a loopback address and returns where it is.
func serveAPI(t *testing.T, conf Conf, kubeConn KubeConn, cache *Cache) *httptest.Server {
	t.Helper()

	// httptest serves over loopback, so the host may be left out.
	if conf.API.Host == "" {
		conf.API.Host = "127.0.0.1"
	}
	if conf.API.Prefix == "" {
		conf.API.Prefix = "/"
	}
	// httptest serves over plain http, so the keys are of no use here.  They
	// are still handed over, since the api insists on having them.
	conf.TLS = testTLS(t)

	server, err := ListenAPI(conf, kubeConn, cache)
	if err != nil {
		t.Fatalf("ListenAPI() returned an error: %v", err)
	}
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

	request, err := http.NewRequest(http.MethodGet, api.URL+path, nil)
	if err != nil {
		t.Fatalf("cannot build GET %s: %v", path, err)
	}

	return do(t, request)
}

// put asks the api to change something, with the replica count in the query or
// in the body of the form.
func put(t *testing.T, api *httptest.Server, path string, form url.Values) answer {
	t.Helper()

	request, err := http.NewRequest(http.MethodPut, api.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("cannot build PUT %s: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return do(t, request)
}

// do asks the api with the prepared request, and reports what it said back.
func do(t *testing.T, request *http.Request) answer {
	t.Helper()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("closing the body of %s %s: %v", request.Method, request.URL, err)
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("cannot read the body of %s %s: %v", request.Method, request.URL, err)
	}

	return answer{
		status:      response.StatusCode,
		contentType: response.Header.Get("Content-Type"),
		body:        string(body),
	}
}

// clusterReplicas is how many replicas the cluster is holding a deployment at.
func clusterReplicas(t *testing.T, client *fake.Clientset, namespace, name string) int32 {
	t.Helper()

	deploy, err := client.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cannot read deployment %q: %v", name, err)
	}
	if deploy.Spec.Replicas == nil {
		return 0
	}

	return *deploy.Spec.Replicas
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
			path: "/deployment?ns=web&deployment=front",
			want: `{"namespace":"web","name":"front","replicas":3}`,
		},
		{
			name: "one deployment of the whole cluster",
			path: "/deployment?deployment=cdn",
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
			path: "/deployment?deployment=shared",
			want: `{"deployments":[{"namespace":"api","name":"shared","replicas":1},` +
				`{"namespace":"web","name":"shared","replicas":2}]}`,
		},
		{
			name: "the same name in the namespace that was asked about",
			path: "/deployment?ns=web&deployment=shared",
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
			path: "/deployment?namespace=web&deployment=front",
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
			path: "/deployment?ns=nope&deployment=front",
			want: `{"error":"namespace \"nope\" does not exist"}`,
		},
		{
			name: "namespace without the deployment",
			path: "/deployment?ns=web&deployment=nope",
			want: `{"error":"deployment \"nope\" does not exist in namespace \"web\""}`,
		},
		{
			name: "deployment that is not there at all",
			path: "/deployment?deployment=nope",
			want: `{"error":"deployment \"nope\" does not exist"}`,
		},
		{
			name: "deployment of another namespace",
			// The namespace was wrong rather than the deployment gone.
			path: "/deployment?ns=web&deployment=backend",
			want: `{"error":"deployment \"backend\" does not exist in namespace \"web\""}`,
		},
		{
			name: "namespace spelled out that is not there",
			path: "/deployment?namespace=nope&deployment=front",
			want: `{"error":"namespace \"nope\" does not exist"}`,
		},
		{
			name: "namespace spelled out without the deployment",
			path: "/deployment?namespace=web&deployment=nope",
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
		"/deployment?ns=web&deployment=front",
		"/deployment?namespace=web&deployment=front",
		"/deployment?ns=web",
		"/deployment?deployment=front",
		"/deployment",
		"/deployment?ns=nope&deployment=front",
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

func TestAPISetReplicas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		form     url.Values
		replicas int32
	}{
		{
			name:     "scaling up",
			path:     "/deployment?ns=web&deployment=front&replicas=5",
			replicas: 5,
		},
		{
			name:     "scaling to none",
			path:     "/deployment?ns=web&deployment=front&replicas=0",
			replicas: 0,
		},
		{
			name:     "the namespace spelled out",
			path:     "/deployment?namespace=web&deployment=front&replicas=7",
			replicas: 7,
		},
		{
			name:     "the count in the body of a form",
			path:     "/deployment?ns=web&deployment=front",
			form:     url.Values{"replicas": {"4"}},
			replicas: 4,
		},
		{
			name:     "the deployment of another namespace",
			path:     "/deployment?ns=api&deployment=shared&replicas=6",
			replicas: 6,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, client, cache := apiOverCluster(t, Conf{}, testCluster()...)
			got := put(t, api, test.path, test.form)
			if got.status != http.StatusOK {
				t.Fatalf("PUT %s status = %d, want %d (%s)", test.path, got.status, http.StatusOK, got.body)
			}
			if !strings.HasPrefix(got.contentType, "application/json") {
				t.Errorf("PUT %s content type = %q, want json", test.path, got.contentType)
			}
			if body := got.asJSON(t); body != `{"status":"done"}` {
				t.Errorf("PUT %s = %s, want it to say the update is done", test.path, body)
			}

			// Only the deployment the path names is scaled.
			namespace, name := askedOf(t, test.path)
			want := heldAfter(t, testCluster(), namespace, name, test.replicas)
			if got := clusterHolds(t, client); !slices.Equal(got, want) {
				t.Errorf("the cluster holds %+v, want %+v", got, want)
			}

			// The cache holds it without the watch reporting it, which nothing
			// is running to do here.
			if !hasDeployment(cache, namespace, name, test.replicas) {
				t.Errorf("the cache holds %+v, want %s/%s at %d replicas",
					cache.Get(namespace).Deployments, namespace, name, test.replicas)
			}
		})
	}
}

// askedOf is the deployment a put path names.
func askedOf(t *testing.T, path string) (namespace, name string) {
	t.Helper()

	asked, err := url.Parse(path)
	if err != nil {
		t.Fatalf("cannot read %q: %v", path, err)
	}
	query := asked.Query()

	namespace = query.Get("ns")
	if namespace == "" {
		namespace = query.Get("namespace")
	}

	return namespace, query.Get("deployment")
}

// clusterHolds is every deployment of the cluster, in the order the service
// reports them.
func clusterHolds(t *testing.T, client *fake.Clientset) []Deployment {
	t.Helper()

	list, err := client.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("cannot list the deployments: %v", err)
	}

	deployments := make([]Deployment, 0, len(list.Items))
	for _, item := range list.Items {
		deployments = append(deployments, newDeployment(&item))
	}
	slices.SortFunc(deployments, byDeployment)

	return deployments
}

// heldAfter is the cluster as it was, with one deployment scaled.
func heldAfter(t *testing.T, objects []runtime.Object, namespace, name string, replicas int32) []Deployment {
	t.Helper()

	deployments := clusterHolds(t, fake.NewClientset(objects...))
	for i, deploy := range deployments {
		if deploy.Namespace == namespace && deploy.Name == name {
			deployments[i].Replicas = replicas
		}
	}

	return deployments
}

func TestAPISetReplicasRefusesWhatItWillNotActOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{
			name:   "no count at all",
			path:   "/deployment?ns=web&deployment=front",
			status: http.StatusBadRequest,
			want:   `{"error":"no replicas were given, and they must be a whole number of zero or more"}`,
		},
		{
			name:   "a count that is not a number",
			path:   "/deployment?ns=web&deployment=front&replicas=three",
			status: http.StatusBadRequest,
			want:   `{"error":"replicas \"three\" is not a whole number"}`,
		},
		{
			name:   "a count that is not whole",
			path:   "/deployment?ns=web&deployment=front&replicas=2.5",
			status: http.StatusBadRequest,
			want:   `{"error":"replicas \"2.5\" is not a whole number"}`,
		},
		{
			name:   "a count with room in it",
			path:   "/deployment?ns=web&deployment=front&replicas=%203",
			status: http.StatusBadRequest,
			want:   `{"error":"replicas \" 3\" is not a whole number"}`,
		},
		{
			name:   "a count that is too large to count",
			path:   "/deployment?ns=web&deployment=front&replicas=99999999999999999999",
			status: http.StatusNotAcceptable,
			want:   `{"error":"replicas 99999999999999999999 is over the limit of 100"}`,
		},
		{
			name:   "a negative count",
			path:   "/deployment?ns=web&deployment=front&replicas=-1",
			status: http.StatusBadRequest,
			want:   `{"error":"replicas -1 is fewer than zero, and must be a whole number of zero or more"}`,
		},
		{
			name:   "no deployment name",
			path:   "/deployment?ns=web&replicas=5",
			status: http.StatusBadRequest,
			want:   `{"error":"no deployment name was given"}`,
		},
		{
			name:   "no namespace",
			path:   "/deployment?deployment=front&replicas=5",
			status: http.StatusBadRequest,
			want:   `{"error":"no namespace was given, and a deployment name can repeat in another namespace"}`,
		},
		{
			name:   "a namespace the cluster does not have",
			path:   "/deployment?ns=nope&deployment=front&replicas=5",
			status: http.StatusNotFound,
			want:   `{"error":"namespace \"nope\" does not exist"}`,
		},
		{
			name:   "a deployment the cluster does not have",
			path:   "/deployment?ns=web&deployment=nope&replicas=5",
			status: http.StatusNotFound,
			want:   `{"error":"deployment \"nope\" does not exist in namespace \"web\""}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, client, cache := apiOverCluster(t, Conf{}, testCluster()...)
			got := put(t, api, test.path, nil)
			if got.status != test.status {
				t.Fatalf("PUT %s status = %d, want %d (%s)", test.path, got.status, test.status, got.body)
			}
			if body := got.asJSON(t); body != test.want {
				t.Errorf("PUT %s = %s, want %s", test.path, body, test.want)
			}

			// A refused request has changed nothing.
			if written := counted(client, "update", "deployments"); written != 0 {
				t.Errorf("the cluster was asked to update a deployment %d times, want none", written)
			}
			if held := clusterReplicas(t, client, "web", "front"); held != 3 {
				t.Errorf("the cluster holds front at %d replicas, want it left at 3", held)
			}
			if !hasDeployment(cache, "web", "front", 3) {
				t.Errorf("the cache holds %+v, want front left at 3", cache.Get("web").Deployments)
			}
		})
	}
}

func TestAPISetReplicasOverTheLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		limit    int32
		replicas string
		status   int
	}{
		// An unset limit is the default rather than none at all.
		{name: "over the default limit", limit: 0, replicas: "101", status: http.StatusNotAcceptable},
		{name: "at the default limit", limit: 0, replicas: "100", status: http.StatusOK},
		{name: "over the configured limit", limit: 5, replicas: "6", status: http.StatusNotAcceptable},
		{name: "at the configured limit", limit: 5, replicas: "5", status: http.StatusOK},
		// A lifted limit is as far as a count reaches.
		{name: "with the limit lifted", limit: -1, replicas: "100000", status: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, _, _ := apiOverCluster(t, Conf{MaxReplicas: test.limit}, testCluster()...)
			path := "/deployment?ns=web&deployment=front&replicas=" + test.replicas
			got := put(t, api, path, nil)
			if got.status != test.status {
				t.Fatalf("PUT %s status = %d, want %d (%s)", path, got.status, test.status, got.body)
			}

			if test.status == http.StatusNotAcceptable {
				if body := got.asJSON(t); !strings.Contains(body, "is over the limit of") {
					t.Errorf("PUT %s = %s, want it to say what the limit is", path, body)
				}
				return
			}
			if body := got.asJSON(t); body != `{"status":"done"}` {
				t.Errorf("PUT %s = %s, want it to say the update is done", path, body)
			}
		})
	}
}

func TestAPISetReplicasWithAColdCache(t *testing.T) {
	t.Parallel()

	// The cache cannot say which deployment this is about, so the cluster is
	// not asked on a guess.
	client := fake.NewClientset(testCluster()...)
	cache := NewCache(KubeConn{k8s: client})
	api := serveAPI(t, Conf{API: Listener{Prefix: "/"}}, KubeConn{k8s: client}, cache)

	got := put(t, api, "/deployment?ns=web&deployment=front&replicas=5", nil)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusServiceUnavailable, got.body)
	}
	if body := got.asJSON(t); body != `{"error":"`+cachePending+`"}` {
		t.Errorf("body = %s, want the cache to be reported as not ready", body)
	}
	if written := counted(client, "update", "deployments"); written != 0 {
		t.Errorf("the cluster was asked to update a deployment %d times, want none", written)
	}
}

func TestAPISetReplicasWhenTheClusterWillNotDoIt(t *testing.T) {
	t.Parallel()

	deployments := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		name string
		// refused is what the cluster answers an update with.
		refused error
		status  int
		// says is a piece of what the cluster said, which the caller is told.
		says string
	}{
		{
			name:    "the deployment is gone",
			refused: apierrors.NewNotFound(deployments, "front"),
			status:  http.StatusNotFound,
			says:    `not found`,
		},
		{
			name:    "this service may not scale it",
			refused: apierrors.NewForbidden(deployments, "front", errors.New("not allowed")),
			status:  http.StatusForbidden,
			says:    "forbidden",
		},
		{
			// The deployment is written whole, so a change made in between is
			// reported rather than written over.
			name:    "the deployment changed in between",
			refused: apierrors.NewConflict(deployments, "front", errors.New("the object has been modified")),
			status:  http.StatusConflict,
			says:    "cannot be fulfilled",
		},
		{
			name:    "the cluster will not take the value",
			refused: apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "front", nil),
			status:  http.StatusBadRequest,
			says:    "is invalid",
		},
		{
			name:    "the cluster is too busy to answer",
			refused: apierrors.NewTimeoutError("the cluster is busy", 1),
			status:  http.StatusGatewayTimeout,
			says:    "the cluster is busy",
		},
		{
			// A failure this service has no status of its own for is reported
			// as the upstream failure it is.
			name:    "the cluster said something else",
			refused: errors.New("the connection was refused"),
			status:  http.StatusBadGateway,
			says:    "the connection was refused",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := fake.NewClientset(testCluster()...)
			client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.refused
			})
			cache := NewCache(KubeConn{k8s: client})
			if err := cache.Populate(context.Background()); err != nil {
				t.Fatalf("Populate() returned an error: %v", err)
			}
			api := serveAPI(t, Conf{API: Listener{Prefix: "/"}}, KubeConn{k8s: client}, cache)

			got := put(t, api, "/deployment?ns=web&deployment=front&replicas=5", nil)
			if got.status != test.status {
				t.Fatalf("status = %d, want %d (%s)", got.status, test.status, got.body)
			}
			if !strings.Contains(got.body, test.says) {
				t.Errorf("body = %q, want it to carry what the cluster said about it", got.body)
			}

			// The cache does not hold an update the cluster refused.
			if !hasDeployment(cache, "web", "front", 3) {
				t.Errorf("the cache holds %+v, want front left at 3", cache.Get("web").Deployments)
			}
		})
	}
}

func TestAPIUIReplicas(t *testing.T) {
	t.Parallel()

	got := get(t, newTestAPI(t, testCluster()...), "/ui/deployment?ns=web&deployment=front")
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if !strings.HasPrefix(got.contentType, "text/html") {
		t.Errorf("content type = %q, want html", got.contentType)
	}
	for _, want := range []string{
		`hx-get="/ui/deployment/edit?deployment=front&amp;ns=web"`,
		">3</button>",
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the cell does not contain %q:\n%s", want, got.body)
		}
	}
	if strings.Contains(got.body, "<form") {
		t.Errorf("the cell is a form, want the number:\n%s", got.body)
	}
}

func TestAPIUIEditReplicas(t *testing.T) {
	t.Parallel()

	got := get(t, newTestAPI(t, testCluster()...), "/ui/deployment/edit?ns=web&deployment=front")
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if !strings.HasPrefix(got.contentType, "text/html") {
		t.Errorf("content type = %q, want html", got.contentType)
	}
	for _, want := range []string{
		"<form",
		`hx-put="/ui/deployment?deployment=front&amp;ns=web"`,
		`name="replicas"`,
		`value="3"`,
		`type="submit"`,
		">Submit<",
		// The browser is told the same limit the api enforces.
		`max="100"`,
		`min="0"`,
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the form does not contain %q:\n%s", want, got.body)
		}
	}
}

func TestAPIUIEditReplicasTellsTheConfiguredLimit(t *testing.T) {
	t.Parallel()

	api, _, _ := apiOverCluster(t, Conf{MaxReplicas: 7}, testCluster()...)
	got := get(t, api, "/ui/deployment/edit?ns=web&deployment=front")
	if !strings.Contains(got.body, `max="7"`) {
		t.Errorf("the form does not carry the configured limit:\n%s", got.body)
	}
}

func TestAPIUIEditReplicasTheTableOffersIt(t *testing.T) {
	t.Parallel()

	got := get(t, newTestAPI(t, testCluster()...), "/ui/list")
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	for _, want := range []string{
		`hx-get="/ui/deployment/edit?deployment=cdn&amp;ns=web"`,
		`hx-get="/ui/deployment/edit?deployment=backend&amp;ns=api"`,
		">2</button>",
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the table does not contain %q:\n%s", want, got.body)
		}
	}
}

func TestAPIUISetReplicas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// form is what the page sends.
		form   url.Values
		status int
		// wants are pieces of the cell the page is left with.
		wants []string
		// lacks are pieces that would mean the row is still a form to fill in.
		lacks []string
		// held is how many replicas the cluster is left with.
		held int32
	}{
		{
			name:   "a count the service accepts",
			form:   url.Values{"replicas": {"5"}},
			status: http.StatusOK,
			wants:  []string{">5</button>", `hx-get="/ui/deployment/edit?deployment=front&amp;ns=web"`},
			lacks:  []string{"<form"},
			held:   5,
		},
		{
			name:   "a count of none",
			form:   url.Values{"replicas": {"0"}},
			status: http.StatusOK,
			wants:  []string{">0</button>"},
			lacks:  []string{"<form"},
			held:   0,
		},
		{
			name:   "a count that is not a whole number",
			form:   url.Values{"replicas": {"two"}},
			status: http.StatusBadRequest,
			wants:  []string{`is not a whole number`, ">3</button>"},
			lacks:  []string{"<form"},
			held:   3,
		},
		{
			name:   "a count over the limit",
			form:   url.Values{"replicas": {"500"}},
			status: http.StatusNotAcceptable,
			wants:  []string{"is over the limit of 100", ">3</button>"},
			lacks:  []string{"<form"},
			held:   3,
		},
		{
			name:   "a count that is fewer than zero",
			form:   url.Values{"replicas": {"-2"}},
			status: http.StatusBadRequest,
			wants:  []string{"is fewer than zero", ">3</button>"},
			lacks:  []string{"<form"},
			held:   3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, client, cache := apiOverCluster(t, Conf{}, testCluster()...)
			got := put(t, api, "/ui/deployment?ns=web&deployment=front", test.form)
			if got.status != test.status {
				t.Fatalf("PUT /ui/deployment status = %d, want %d (%s)", got.status, test.status, got.body)
			}
			if !strings.HasPrefix(got.contentType, "text/html") {
				t.Errorf("content type = %q, want html", got.contentType)
			}
			for _, want := range test.wants {
				if !strings.Contains(got.body, want) {
					t.Errorf("the cell does not contain %q:\n%s", want, got.body)
				}
			}
			for _, lacks := range test.lacks {
				if strings.Contains(got.body, lacks) {
					t.Errorf("the cell contains %q, want it to be a row again:\n%s", lacks, got.body)
				}
			}

			// Only a count the service accepted reaches the cluster.
			if held := clusterReplicas(t, client, "web", "front"); held != test.held {
				t.Errorf("the cluster holds front at %d replicas, want %d", held, test.held)
			}
			if !hasDeployment(cache, "web", "front", test.held) {
				t.Errorf("the cache holds %+v, want front at %d", cache.Get("web").Deployments, test.held)
			}
		})
	}
}

func TestAPIUISetReplicasUpdatesTheCluster(t *testing.T) {
	t.Parallel()

	api, client, cache := apiOverCluster(t, Conf{}, testCluster()...)
	got := put(t, api, "/ui/deployment?ns=web&deployment=front", url.Values{"replicas": {"5"}})
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}

	if held := clusterReplicas(t, client, "web", "front"); held != 5 {
		t.Errorf("the cluster holds front at %d replicas, want 5", held)
	}
	if !hasDeployment(cache, "web", "front", 5) {
		t.Errorf("the cache holds %+v, want front at 5", cache.Get("web").Deployments)
	}
}

func TestAPIUISetReplicasTellsWhyItDidNot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{
			name:   "a deployment the cluster does not have",
			path:   "/ui/deployment?ns=web&deployment=nope",
			status: http.StatusNotFound,
			want:   "does not exist in namespace",
		},
		{
			name:   "a namespace the cluster does not have",
			path:   "/ui/deployment?ns=nope&deployment=front",
			status: http.StatusNotFound,
			want:   "does not exist",
		},
		{
			name:   "no deployment named",
			path:   "/ui/deployment?ns=web",
			status: http.StatusBadRequest,
			want:   "no deployment name was given",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, client, _ := apiOverCluster(t, Conf{}, testCluster()...)
			got := put(t, api, test.path, url.Values{"replicas": {"5"}})
			if got.status != test.status {
				t.Fatalf("PUT %s status = %d, want %d (%s)", test.path, got.status, test.status, got.body)
			}
			// The page reads a failure as html rather than as json.
			if !strings.HasPrefix(got.contentType, "text/html") {
				t.Errorf("content type = %q, want html", got.contentType)
			}
			if !strings.Contains(got.body, test.want) {
				t.Errorf("the cell does not say %q:\n%s", test.want, got.body)
			}

			if written := counted(client, "update", "deployments"); written != 0 {
				t.Errorf("the cluster was asked to update a deployment %d times, want none", written)
			}
		})
	}
}

func TestAPIUISetReplicasTheClusterWillNotDoIt(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(testCluster()...)
	client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "front", errors.New("not allowed"))
	})
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	api := serveAPI(t, Conf{API: Listener{Prefix: "/"}}, KubeConn{k8s: client}, cache)

	got := put(t, api, "/ui/deployment?ns=web&deployment=front", url.Values{"replicas": {"5"}})
	if got.status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (%s)", got.status, http.StatusForbidden, got.body)
	}
	if !strings.Contains(got.body, "forbidden") {
		t.Errorf("the cell does not say what the cluster said:\n%s", got.body)
	}
	if strings.Contains(got.body, "<form") {
		t.Errorf("the cell is still a form, want the number carrying the reason:\n%s", got.body)
	}
}

func TestAPISetReplicasIsReachable(t *testing.T) {
	t.Parallel()

	// The api and the page are the same write, under the same prefix.
	client := fake.NewClientset(testCluster()...)
	cache := NewCache(KubeConn{k8s: client})
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	conf := Conf{API: Listener{Host: "127.0.0.1", Prefix: "/teleport"}}
	api := serveAPI(t, conf, KubeConn{k8s: client}, cache)

	if got := put(t, api, "/teleport/deployment?ns=web&deployment=front&replicas=2", nil); got.status != http.StatusOK {
		t.Errorf("PUT /teleport/deployment status = %d, want %d (%s)", got.status, http.StatusOK, got.body)
	}
	if held := clusterReplicas(t, client, "web", "front"); held != 2 {
		t.Errorf("the cluster holds front at %d replicas, want 2", held)
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
	conf.TLS = testTLS(t)
	server, err := ListenAPI(conf, KubeConn{k8s: client}, cache)
	if err != nil {
		t.Fatalf("ListenAPI() returned an error: %v", err)
	}
	api := httptest.NewServer(server.Handler)
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
