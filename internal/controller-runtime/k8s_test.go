package controllerruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func TestSetReplicas(t *testing.T) {
	t.Parallel()

	// A deployment carries more than its replica count, and the rest of it is
	// left as it was.
	front := deployment("web", "front", 3)
	front.Labels = map[string]string{"app": "front"}
	front.Spec.Template.Spec.Containers = []corev1.Container{{Name: "front", Image: "front:1"}}

	conn := cluster(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web"}}, front)
	if err := conn.SetReplicas(context.Background(), "web", "front", 7); err != nil {
		t.Fatalf("SetReplicas() returned an error: %v", err)
	}

	got, err := conn.k8s.AppsV1().Deployments("web").Get(context.Background(), "front", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cannot read the deployment: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 7 {
		t.Errorf("the deployment asks for %v replicas, want 7", got.Spec.Replicas)
	}
	if !reflect.DeepEqual(got.Labels, front.Labels) {
		t.Errorf("the deployment carries the labels %v, want %v", got.Labels, front.Labels)
	}
	if !reflect.DeepEqual(got.Spec.Template.Spec.Containers, front.Spec.Template.Spec.Containers) {
		t.Errorf("the deployment carries the containers %v, want %v",
			got.Spec.Template.Spec.Containers, front.Spec.Template.Spec.Containers)
	}

	// Scaling it down to none is a whole number of replicas like any other.
	if err := conn.SetReplicas(context.Background(), "web", "front", 0); err != nil {
		t.Fatalf("SetReplicas() returned an error: %v", err)
	}
	got, err = conn.k8s.AppsV1().Deployments("web").Get(context.Background(), "front", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cannot read the deployment: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Errorf("the deployment asks for %v replicas, want none", got.Spec.Replicas)
	}
}

func TestSetReplicasErrors(t *testing.T) {
	t.Parallel()

	deployments := schema.GroupResource{Group: "apps", Resource: "deployments"}

	t.Run("a deployment that is not there", func(t *testing.T) {
		t.Parallel()

		err := cluster().SetReplicas(context.Background(), "web", "front", 3)
		if !apierrors.IsNotFound(err) {
			t.Errorf("SetReplicas() error = %v, want it to wrap a not found", err)
		}
		if got := err.Error(); !strings.Contains(got, `"front"`) {
			t.Errorf("SetReplicas() error = %q, want it to name the deployment it could not read", got)
		}
	})

	t.Run("a cluster that refuses the update", func(t *testing.T) {
		t.Parallel()

		denied := apierrors.NewForbidden(deployments, "front", errors.New("not allowed to scale"))
		client := fake.NewClientset(deployment("web", "front", 3))
		client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, denied
		})

		err := KubeConn{k8s: client}.SetReplicas(context.Background(), "web", "front", 5)
		if !errors.Is(err, denied) {
			t.Errorf("SetReplicas() error = %v, want it to wrap %v", err, denied)
		}
		// The reason the api server gave is kept.
		if got := err.Error(); !strings.Contains(got, "not allowed to scale") {
			t.Errorf("SetReplicas() error = %q, want it to carry what the api server said", got)
		}
	})
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
