package controllerruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// deployment builds a deployment fixture, replica count as a pointer.
func deployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// unsetReplicas is a deployment left with its replica count unset.
func unsetReplicas(namespace, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

// cluster is a fake api server holding the given namespaces and deployments.
func cluster(objects ...runtime.Object) KubeConn {
	return KubeConn{k8s: fake.NewClientset(objects...)}
}

func TestList(t *testing.T) {
	t.Parallel()

	ns := func(names ...string) []Namespace {
		var namespaces []Namespace
		for _, name := range names {
			namespaces = append(namespaces, Namespace{Name: name})
		}
		return namespaces
	}

	tests := []struct {
		name      string
		namespace string
		objects   []runtime.Object
		want      Inventory
	}{
		{
			name: "every namespace",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
				deployment("web", "front", 3),
				deployment("api", "backend", 1),
			},
			// Deployments come back ordered by namespace and then name.
			want: Inventory{
				Namespaces: ns("api", "web"),
				Deployments: []Deployment{
					{Namespace: "api", Name: "backend", Replicas: 1},
					{Namespace: "web", Name: "front", Replicas: 3},
				},
			},
		},
		{
			name:      "one namespace",
			namespace: "web",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api"}},
				deployment("web", "front", 3),
				deployment("web", "cdn", 2),
				deployment("api", "backend", 1),
			},
			// The namespaces stay whole, to offer as the filter choices.
			want: Inventory{
				Namespaces: ns("api", "web"),
				Deployments: []Deployment{
					{Namespace: "web", Name: "cdn", Replicas: 2},
					{Namespace: "web", Name: "front", Replicas: 3},
				},
			},
		},
		{
			name:      "namespace without deployments",
			namespace: "empty",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty"}},
				deployment("web", "front", 3),
			},
			// An empty result renders as the "nothing to show" row.
			want: Inventory{
				Namespaces:  ns("empty"),
				Deployments: []Deployment{},
			},
		},
		{
			name: "unset replicas",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
				unsetReplicas("web", "front"),
			},
			want: Inventory{
				Namespaces:  ns("web"),
				Deployments: []Deployment{{Namespace: "web", Name: "front"}},
			},
		},
		{
			name: "empty cluster",
			// Both listings come back empty rather than nil.
			want: Inventory{
				Namespaces:  []Namespace{},
				Deployments: []Deployment{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := cluster(test.objects...).List(context.Background(), test.namespace)
			if err != nil {
				t.Fatalf("List() returned an error: %v", err)
			}

			// The versions are checked for being handed over, not for value.
			deployments, namespaces := got.DeploymentsResourceVersion, got.NamespacesResourceVersion
			got.DeploymentsResourceVersion, got.NamespacesResourceVersion = "", ""
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("List() = %+v, want %+v", got, test.want)
			}
			if deployments == "" {
				t.Error("List() reported an empty deployment resource version, want the one the listing was read at")
			}
			if namespaces == "" {
				t.Error("List() reported an empty namespace resource version, want the one the listing was read at")
			}
		})
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// resource is the api resource the fake fails on.
		resource string
	}{
		{name: "namespaces", resource: "namespaces"},
		{name: "deployments", resource: "deployments"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			denied := errors.New("not allowed to list")
			client := fake.NewClientset()
			client.PrependReactor("list", test.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, denied
			})

			_, err := KubeConn{k8s: client}.List(context.Background(), "")
			if !errors.Is(err, denied) {
				t.Errorf("List() error = %v, want it to wrap %v", err, denied)
			}
			// The error names the listing that failed.
			if got := err.Error(); got == denied.Error() {
				t.Errorf("List() error = %q, want it to name the failed listing", got)
			}
		})
	}
}
