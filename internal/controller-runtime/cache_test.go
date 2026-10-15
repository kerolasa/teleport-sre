package controllerruntime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rs/zerolog"
)

// waitTimeout is how long a test waits for the cache to catch up with the
// cluster, which it does in the background.
const waitTimeout = 5 * time.Second

// waitTick is how often those waits look, and how long the cache waits
// between watches in a test.
const waitTick = 5 * time.Millisecond

// TestMain keeps the logs of the cache out of the test output.  A cache that
// is having a bad time says so at the error level, and a test that provokes
// one should not read as though the service is unwell.
func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

// newTestCache is a cache that watches quickly enough for a test to watch it
// re-establish itself.
func newTestCache(client *fake.Clientset) *Cache {
	cache := NewCache(KubeConn{k8s: client})
	cache.retryInterval = waitTick

	return cache
}

// waitFor waits for cond to hold, and fails the test if it does not soon.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(waitTick)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// counted tells how many times the fake api server was asked for verb on
// resource.
func counted(client *fake.Clientset, verb, resource string) int {
	var count int
	for _, action := range client.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == resource {
			count++
		}
	}
	return count
}

// watchOptions returns the options the fake api server was asked to watch
// resource with, the first time it was asked.
func watchOptions(t *testing.T, client *fake.Clientset, resource string) metav1.ListOptions {
	t.Helper()

	for _, action := range client.Actions() {
		if action.GetVerb() != "watch" || action.GetResource().Resource != resource {
			continue
		}
		watched, ok := action.(k8stesting.WatchActionImpl)
		if !ok {
			t.Fatalf("watch action is a %T, want a WatchActionImpl", action)
		}
		return watched.ListOptions
	}

	t.Fatalf("the fake api server was never asked to watch %s", resource)
	return metav1.ListOptions{}
}

// hasDeployment tells whether the cache holds a deployment with these
// replica count.
func hasDeployment(cache *Cache, namespace, name string, replicas int32) bool {
	want := Deployment{Namespace: namespace, Name: name, Replicas: replicas}

	return slices.Contains(cache.Get(namespace).Deployments, want)
}

// hasNamespace tells whether the cache holds a namespace of this name.
func hasNamespace(cache *Cache, name string) bool {
	return slices.Contains(cache.Get("").Namespaces, Namespace{Name: name})
}

func TestCachePopulate(t *testing.T) {
	t.Parallel()

	denied := errors.New("not allowed to list")
	tests := []struct {
		name    string
		objects []runtime.Object
		// deniedBy makes the api server refuse to list the deployments.
		deniedBy error
		want     Inventory
	}{
		{
			name: "namespaces and deployments",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
				deployment("web", "front", 3),
				deployment("api", "backend", 1),
			},
			want: Inventory{
				Namespaces:  []Namespace{{Name: "api"}, {Name: "web"}},
				Deployments: []Deployment{{Namespace: "api", Name: "backend", Replicas: 1}, {Namespace: "web", Name: "front", Replicas: 3}},
			},
		},
		{
			name: "empty cluster",
			want: Inventory{
				Namespaces:  []Namespace{},
				Deployments: []Deployment{},
			},
		},
		{
			name: "listing is refused",
			objects: []runtime.Object{
				deployment("web", "front", 3),
			},
			deniedBy: denied,
			// The cache is left as it was, which for one that has just been
			// made is empty rather than half filled.
			want: Inventory{
				Namespaces:  []Namespace{},
				Deployments: []Deployment{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := fake.NewClientset(test.objects...)
			if test.deniedBy != nil {
				client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.deniedBy
				})
			}
			cache := newTestCache(client)

			err := cache.Populate(context.Background())
			if test.deniedBy != nil {
				// A cluster that cannot be listed leaves the cache as it was,
				// which for a cache that has just been made is empty.  A
				// caller is told there is nothing rather than being handed
				// a half filled cache.
				if !errors.Is(err, test.deniedBy) {
					t.Fatalf("Populate() error = %v, want it to wrap %v", err, test.deniedBy)
				}
			} else if err != nil {
				t.Fatalf("Populate() returned an error: %v", err)
			}

			// The api server numbers the versions, so what is checked is that
			// each is held, not what it happens to be.
			got := cache.Get("")
			deployments, namespaces := got.DeploymentsResourceVersion, got.NamespacesResourceVersion
			got.DeploymentsResourceVersion, got.NamespacesResourceVersion = "", ""
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("Get() = %+v, want %+v", got, test.want)
			}
			if test.deniedBy == nil && (deployments == "" || namespaces == "") {
				t.Errorf("Get() reported versions %q and %q, want the ones the cache was filled at", deployments, namespaces)
			}
		})
	}
}

func TestCacheGetNamespace(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
		deployment("web", "front", 3),
		deployment("web", "cdn", 2),
		deployment("api", "backend", 1),
	)
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	tests := []struct {
		name      string
		namespace string
		want      []Deployment
	}{
		{
			name:      "every namespace",
			namespace: "",
			want: []Deployment{
				{Namespace: "api", Name: "backend", Replicas: 1},
				{Namespace: "web", Name: "cdn", Replicas: 2},
				{Namespace: "web", Name: "front", Replicas: 3},
			},
		},
		{
			name:      "one namespace",
			namespace: "web",
			// Ordered by name, which is the order the cache would not
			// otherwise produce, since it holds them in a map.
			want: []Deployment{
				{Namespace: "web", Name: "cdn", Replicas: 2},
				{Namespace: "web", Name: "front", Replicas: 3},
			},
		},
		{
			name:      "namespace without deployments",
			namespace: "empty",
			want:      []Deployment{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := cache.Get(test.namespace)
			if !reflect.DeepEqual(got.Deployments, test.want) {
				t.Errorf("Get(%q).Deployments = %+v, want %+v", test.namespace, got.Deployments, test.want)
			}
			// The namespaces are not narrowed by the request, since the
			// full set is what a namespace filter offers as its choices.
			want := []Namespace{{Name: "api"}, {Name: "web"}}
			if !reflect.DeepEqual(got.Namespaces, want) {
				t.Errorf("Get(%q).Namespaces = %+v, want %+v", test.namespace, got.Namespaces, want)
			}
		})
	}
}

// watching starts Watch in the background and returns a function that stops
// the cache and waits for it to be done.
func watching(t *testing.T, cache *Cache) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cache.Watch(ctx)
	}()

	return func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(waitTimeout):
			t.Error("Watch did not return after its context was cancelled")
		}
	}
}

// watchStarted hands out the watches the api server is given, for the tests
// that need to end one themselves.  A test that wants the real events of the
// fake api server must not use this, since it replaces them.
func watchStarted(t *testing.T, client *fake.Clientset) <-chan *watch.RaceFreeFakeWatcher {
	t.Helper()

	started := make(chan *watch.RaceFreeFakeWatcher, 8)
	client.PrependWatchReactor("deployments", func(k8stesting.Action) (bool, watch.Interface, error) {
		watcher := watch.NewRaceFreeFake()
		select {
		case started <- watcher:
		default:
		}
		return true, watcher, nil
	})

	return started
}

// nextWatch waits for the cache to be handed a watch and returns it.
func nextWatch(t *testing.T, started <-chan *watch.RaceFreeFakeWatcher) *watch.RaceFreeFakeWatcher {
	t.Helper()

	select {
	case watcher := <-started:
		return watcher
	case <-time.After(waitTimeout):
		t.Fatal("the cache did not start watching the cluster")
		return nil
	}
}

func TestCacheWatchAppliesChanges(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	ctx := context.Background()

	// A deployment created after the cache was filled is picked up.
	_, err := client.AppsV1().Deployments("web").Create(ctx, deployment("web", "cdn", 2), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("cannot create deployment: %v", err)
	}
	waitFor(t, "the added deployment to be cached", func() bool {
		return hasDeployment(cache, "web", "cdn", 2)
	})

	// Scaling it is picked up as well.
	_, err = client.AppsV1().Deployments("web").Update(ctx, deployment("web", "cdn", 5), metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("cannot update deployment: %v", err)
	}
	waitFor(t, "the scaled deployment to be cached", func() bool {
		return hasDeployment(cache, "web", "cdn", 5)
	})

	// And deleting it takes it back out again.
	if err := client.AppsV1().Deployments("web").Delete(ctx, "cdn", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("cannot delete deployment: %v", err)
	}
	waitFor(t, "the deleted deployment to be dropped", func() bool {
		return !hasDeployment(cache, "web", "cdn", 5)
	})
}

func TestCacheWatchResumesFromTheListedVersion(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	version := cache.Get("").DeploymentsResourceVersion

	stop := watching(t, cache)
	defer stop()
	waitFor(t, "the watch to start", func() bool {
		return counted(client, "watch", "deployments") == 1
	})

	// The watch takes up from the version the cache was filled at, so that a
	// deployment changed in between is delivered rather than skipped.
	got := watchOptions(t, client, "deployments")
	if got.ResourceVersion != version {
		t.Errorf("watch resumed from %q, want the version the cache was filled at %q", got.ResourceVersion, version)
	}
	if !got.AllowWatchBookmarks {
		t.Error("watch did not allow bookmarks, want them so that the version moves on a quiet cluster")
	}
}

func TestCacheWatchResumesTheNamespaceWatchFromTheListedVersion(t *testing.T) {
	// The fixture holds three namespaces and one deployment, which the fake
	// numbers separately, so that the two listings are read at different
	// versions and the namespace watch cannot pass for the deployment one.
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cdn"}},
		deployment("web", "front", 1),
	)
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}
	filled := cache.Get("")
	version, deploymentsVersion := filled.NamespacesResourceVersion, filled.DeploymentsResourceVersion
	if version == deploymentsVersion {
		t.Fatalf("the fixture was filled at one version %q for both listings, so it cannot tell the watches apart", version)
	}

	stop := watching(t, cache)
	defer stop()
	waitFor(t, "the watch to start", func() bool {
		return counted(client, "watch", "namespaces") == 1
	})

	// The namespace watch takes up from the version the namespace listing was
	// read at, since a namespace created in between would otherwise be
	// stepped over.
	got := watchOptions(t, client, "namespaces")
	if got.ResourceVersion != version {
		t.Errorf("watch resumed from %q, want the version the namespaces were listed at %q", got.ResourceVersion, version)
	}
	if !got.AllowWatchBookmarks {
		t.Error("watch did not allow bookmarks, want them so that the version moves on a quiet cluster")
	}
}

func TestCacheWatchAppliesNamespaceChanges(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}})
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	ctx := context.Background()

	// A namespace created after the cache was filled is offered as a choice,
	// rather than waiting for a deployment change to notice it.
	for _, name := range []string{"api", "cdn"} {
		created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if _, err := client.CoreV1().Namespaces().Create(ctx, created, metav1.CreateOptions{}); err != nil {
			t.Fatalf("cannot create namespace: %v", err)
		}
	}
	waitFor(t, "the added namespaces to be cached", func() bool {
		return hasNamespace(cache, "api") && hasNamespace(cache, "cdn")
	})

	// The cache holds the namespaces in a map, so they are ordered on the way
	// out rather than in the order they were added.
	want := []Namespace{{Name: "api"}, {Name: "cdn"}, {Name: "web"}}
	if got := cache.Get("").Namespaces; !reflect.DeepEqual(got, want) {
		t.Errorf("Get().Namespaces = %+v, want %+v", got, want)
	}

	// A deleted one is taken back out again, rather than lingering until a
	// deployment change happens to resynchronise the cache.
	if err := client.CoreV1().Namespaces().Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("cannot delete namespace: %v", err)
	}
	waitFor(t, "the deleted namespace to be dropped", func() bool {
		return !hasNamespace(cache, "web")
	})
}

func TestCacheWatchIsReestablishedWhenItEnds(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	started := watchStarted(t, client)
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	// An ended watch is the normal case rather than a failure, since the api
	// server closes one on its own schedule.  Another is started in its
	// place, and the namespace watch goes with it rather than carrying on
	// against a listing that is yet to be written.
	nextWatch(t, started).Stop()
	waitFor(t, "the watches to be started again", func() bool {
		return counted(client, "watch", "deployments") == 2 && counted(client, "watch", "namespaces") == 2
	})

	// An ended watch is not a reason to list the cluster again.
	if listed := counted(client, "list", "deployments"); listed != 1 {
		t.Errorf("the cluster was listed %d times, want the one listing that filled the cache", listed)
	}
}

func TestCacheWatchResynchronisesAfterAnError(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	// Every watch is refused straight away, the way the api server answers a
	// resource version that has gone too far back to be served from.
	client.PrependWatchReactor("deployments", func(k8stesting.Action) (bool, watch.Interface, error) {
		// The channel is given room for the one event, since a fake watcher
		// that nobody is reading yet would otherwise block here rather than
		// hand the event over.
		refused := watch.NewFakeWithChanSize(1, false)
		refused.Error(&metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusGone,
			Reason:  metav1.StatusReasonExpired,
			Message: "too old resource version: 1 (5)",
		})
		return true, refused, nil
	})
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	// A watch the api server will not serve is recovered from with a fresh
	// listing, which also moves the version back to something it will serve.
	waitFor(t, "the cache to be listed again", func() bool {
		return counted(client, "list", "deployments") >= 2
	})

	// The cache still answers with what it was last given.
	if !hasDeployment(cache, "web", "front", 1) {
		t.Errorf("Get() = %+v, want the deployment the cache was populated with", cache.Get("").Deployments)
	}
}

func TestCacheWatchResynchronisesAfterANamespaceError(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	// Every namespace watch is refused straight away, the way the api server
	// answers a resource version that has gone too far back to be served from.
	client.PrependWatchReactor("namespaces", func(k8stesting.Action) (bool, watch.Interface, error) {
		// The channel is given room for the one event, since a fake watcher
		// that nobody is reading yet would otherwise block here rather than
		// hand the event over.
		refused := watch.NewFakeWithChanSize(1, false)
		refused.Error(&metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusGone,
			Reason:  metav1.StatusReasonExpired,
			Message: "too old resource version: 1 (5)",
		})
		return true, refused, nil
	})
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	// A namespace watch the api server will not serve is recovered from with
	// the same fresh listing the deployments are, since that listing is what
	// fills both halves of the cache.
	waitFor(t, "the cache to be listed again", func() bool {
		return counted(client, "list", "namespaces") >= 2
	})

	// The cache still answers with what it was last given.
	if !hasDeployment(cache, "web", "front", 1) {
		t.Errorf("Get() = %+v, want the deployment the cache was populated with", cache.Get("").Deployments)
	}
}

func TestCacheWatchStopsWithItsContext(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	started := watchStarted(t, client)
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	watcher := nextWatch(t, started)
	stop()

	// The connection the watch was holding is given up as Watch returns,
	// rather than left for the api server to time out on its own.
	if !watcher.IsStopped() {
		t.Error("the watch was left running after Watch returned")
	}
}

func TestCacheReadWhileWatching(t *testing.T) {
	client := fake.NewClientset(deployment("web", "front", 1))
	cache := newTestCache(client)
	if err := cache.Populate(context.Background()); err != nil {
		t.Fatalf("Populate() returned an error: %v", err)
	}

	stop := watching(t, cache)
	defer stop()

	// The readers keep asking until the writer is done, so that the two are
	// certainly running at the same time.  This is the test that only says
	// something under the race detector, and what it says is whether the
	// cache is locked against itself.
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
					cache.Get("")
					cache.Get("web")
				}
			}
		}()
	}

	ctx := context.Background()
	for replicas := range 50 {
		_, err := client.AppsV1().Deployments("web").Update(ctx, deployment("web", "front", int32(replicas)), metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("cannot update deployment: %v", err)
		}
	}
	// Waiting for the last of them to be cached keeps the readers running
	// over the whole of the watch writing to the cache.
	waitFor(t, "the last update to be cached", func() bool {
		return hasDeployment(cache, "web", "front", 49)
	})

	close(done)
	readers.Wait()
}
