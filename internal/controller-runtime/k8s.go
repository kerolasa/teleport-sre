package controllerruntime

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeConn is a connection to the kubernetes api server.
type KubeConn struct {
	// k8s is an interface, so a fake api server can stand in for a cluster.
	k8s kubernetes.Interface
}

// NewKube opens a connection to the kubernetes api server the way conf says.
func NewKube(conf KubeConf) (KubeConn, error) {
	config, err := kubeConfig(conf)
	if err != nil {
		return KubeConn{}, err
	}

	k8s, err := kubernetes.NewForConfig(config)
	if err != nil {
		return KubeConn{}, fmt.Errorf("cannot create kubernetes client: %w", err)
	}

	return KubeConn{k8s: k8s}, nil
}

// kubeConfig works out the address and the credentials of the api server
func kubeConfig(conf KubeConf) (*rest.Config, error) {
	return kubeConfigWith(conf, rest.InClusterConfig)
}

// kubeConfigWith is kubeConfig with the in-cluster lookup handed in.
func kubeConfigWith(conf KubeConf, inCluster func() (*rest.Config, error)) (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	if conf.Context != "" {
		overrides.CurrentContext = conf.Context
	}

	if conf.Kubeconfig != "" {
		path := expandHome(conf.Kubeconfig)
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = path

		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot use kubeconfig %q: %w", path, err)
		}
		return config, nil
	}

	if config, err := inCluster(); err == nil {
		return config, nil
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot find kubernetes: %w", err)
	}

	return config, nil
}

// expandHome replaces a leading tilde without help of a shell.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Namespace is a namespace of the cluster as the service reports it.
type Namespace struct {
	Name string `json:"name" example:"default"`
}

// Deployment is a deployment of the cluster as the service reports it.
type Deployment struct {
	Namespace string `json:"namespace" example:"default"`
	Name      string `json:"name" example:"nginx"`
	// Replicas is the number of replicas the deployment asks for.
	Replicas int32 `json:"replicas" example:"3"`
}

// Inventory contains information about the cluster as a whole.
type Inventory struct {
	Namespaces  []Namespace
	Deployments []Deployment
	// DeploymentsResourceVersion is where the deployment watch takes up.
	DeploymentsResourceVersion string
	// NamespacesResourceVersion is where the namespace watch takes up.  The
	// two are held apart, since the listings are served one after the other.
	NamespacesResourceVersion string
}

// List reports the namespaces of the cluster and the deployments within
// namespace.
//
// TODO: A cluster with thousands of namespaces needs better storage than
// this, which is left broken in this toy implementation.
func (k KubeConn) List(ctx context.Context, namespace string) (Inventory, error) {
	namespaces, namespacesVersion, err := k.listNamespaces(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("cannot list namespaces: %w", err)
	}

	deployments, deploymentsVersion, err := k.listDeployments(ctx, namespace)
	if err != nil {
		return Inventory{}, fmt.Errorf("cannot list deployments: %w", err)
	}

	return Inventory{
		Namespaces:                 namespaces,
		Deployments:                deployments,
		DeploymentsResourceVersion: deploymentsVersion,
		NamespacesResourceVersion:  namespacesVersion,
	}, nil
}

// Reachable reports whether the api server answers.  It asks for one
// namespace, since a heartbeat needs to know the server is there.
func (k KubeConn) Reachable(ctx context.Context) error {
	_, err := k.k8s.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("cannot reach the kubernetes api server: %w", err)
	}
	return nil
}

// SetReplicas sets the number of replicas of the deployment the namespace and
// the name name.
//
// The deployment is read before it is written, since the update carries the
// whole of it.  The read version goes with the write, so that the api server
// turns down an update to a deployment something else has changed in between.
func (k KubeConn) SetReplicas(ctx context.Context, namespace, name string, replicas int32) error {
	deployments := k.k8s.AppsV1().Deployments(namespace)

	current, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot read deployment %q in namespace %q: %w", name, namespace, err)
	}

	updated := current.DeepCopy()
	updated.Spec.Replicas = &replicas

	if _, err := deployments.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("cannot set deployment %q in namespace %q to %d replicas: %w",
			name, namespace, replicas, err)
	}

	return nil
}

// WatchDeployments streams the changes made to the deployments of every
// namespace, starting from resourceVersion.  An empty one starts from
// wherever the api server is now.  Bookmarks are allowed, so that a quiet
// cluster still moves the version forward.
func (k KubeConn) WatchDeployments(ctx context.Context, resourceVersion string) (watch.Interface, error) {
	return k.k8s.AppsV1().Deployments(metav1.NamespaceAll).Watch(ctx, metav1.ListOptions{
		ResourceVersion:     resourceVersion,
		AllowWatchBookmarks: true,
	})
}

// WatchNamespaces streams the namespace changes, and answers
// WatchDeployments.
func (k KubeConn) WatchNamespaces(ctx context.Context, resourceVersion string) (watch.Interface, error) {
	return k.k8s.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{
		ResourceVersion:     resourceVersion,
		AllowWatchBookmarks: true,
	})
}

// listNamespaces reports the namespaces and the version they were read at.
func (k KubeConn) listNamespaces(ctx context.Context) ([]Namespace, string, error) {
	list, err := k.k8s.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", err
	}

	namespaces := make([]Namespace, 0, len(list.Items))
	for _, item := range list.Items {
		namespaces = append(namespaces, newNamespace(&item))
	}
	slices.SortFunc(namespaces, byNamespace)

	return namespaces, list.ResourceVersion, nil
}

// listDeployments reports the deployments of namespace, or of every
// namespace when it is empty, and the version they were read at.
func (k KubeConn) listDeployments(ctx context.Context, namespace string) ([]Deployment, string, error) {
	list, err := k.k8s.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", err
	}

	deployments := make([]Deployment, 0, len(list.Items))
	for _, item := range list.Items {
		deployments = append(deployments, newDeployment(&item))
	}
	slices.SortFunc(deployments, byDeployment)

	return deployments, list.ResourceVersion, nil
}

// newNamespace reads a namespace the way the service reports it.
func newNamespace(item *corev1.Namespace) Namespace {
	return Namespace{Name: item.Name}
}

// newDeployment reads a deployment the way the service reports it.
func newDeployment(item *appsv1.Deployment) Deployment {
	var replicas int32
	// The api server defaults this to one, but an unset field is a nil pointer.
	if item.Spec.Replicas != nil {
		replicas = *item.Spec.Replicas
	}

	return Deployment{
		Namespace: item.Namespace,
		Name:      item.Name,
		Replicas:  replicas,
	}
}

// byDeployment orders by namespace and then name, as the table renders.
func byDeployment(a, b Deployment) int {
	return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
}

// byNamespace orders by name, as the filter choices are offered.
func byNamespace(a, b Namespace) int {
	return cmp.Compare(a.Name, b.Name)
}
