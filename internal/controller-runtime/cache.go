package controllerruntime

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/rs/zerolog/log"
)

// watchRetryInterval is how long the cache waits before watching again.
const watchRetryInterval = 5 * time.Second

// Cache is the cluster as the service remembers it.  The read only routes
// answer from here, and Watch keeps it in step with the cluster.
type Cache struct {
	k8s KubeConn

	// retryInterval is how long to wait before watching again.
	retryInterval time.Duration

	// mu guards everything below it, and Get takes it shared.
	mu                 sync.RWMutex
	namespaces         map[string]Namespace
	deployments        map[string]Deployment
	namespacesVersion  string
	deploymentsVersion string
	populated          bool
}

// NewCache returns an empty cache.  Populate fills it before Watch starts.
func NewCache(k8s KubeConn) *Cache {
	return &Cache{
		k8s:           k8s,
		retryInterval: watchRetryInterval,
		namespaces:    make(map[string]Namespace),
		deployments:   make(map[string]Deployment),
	}
}

// Populate fills the cache from the cluster, replacing what was there.
func (c *Cache) Populate(ctx context.Context) error {
	inventory, err := c.k8s.List(ctx, metav1.NamespaceAll)
	if err != nil {
		return fmt.Errorf("cannot list cluster: %w", err)
	}

	deployments := make(map[string]Deployment, len(inventory.Deployments))
	for _, deploy := range inventory.Deployments {
		deployments[cacheKey(deploy.Namespace, deploy.Name)] = deploy
	}
	namespaces := make(map[string]Namespace, len(inventory.Namespaces))
	for _, namespace := range inventory.Namespaces {
		namespaces[namespace.Name] = namespace
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.namespaces = namespaces
	c.deployments = deployments
	c.namespacesVersion = inventory.NamespacesResourceVersion
	c.deploymentsVersion = inventory.DeploymentsResourceVersion
	// A listing refreshes both halves.
	refresh(namespaceResource)
	refresh(deploymentResource)
	c.populated = true
	c.measure()

	return nil
}

// Populated tells whether the cache has been filled, which the api asks before
// answering from it.
func (c *Cache) Populated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.populated
}

// Set records what the cluster has just confirmed for a deployment.  The
// resource version is left to the watch, which has not seen the events since.
func (c *Cache) Set(deploy Deployment) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.deployments[cacheKey(deploy.Namespace, deploy.Name)] = deploy
	refresh(deploymentResource)
	c.measure()
}

// Get returns the cached namespaces and the cached deployments of namespace.
// An empty namespace means every namespace, as it does in List.
func (c *Cache) Get(namespace string) Inventory {
	c.mu.RLock()
	defer c.mu.RUnlock()

	inventory := Inventory{
		Namespaces:                 make([]Namespace, 0, len(c.namespaces)),
		Deployments:                make([]Deployment, 0, len(c.deployments)),
		DeploymentsResourceVersion: c.deploymentsVersion,
		NamespacesResourceVersion:  c.namespacesVersion,
	}
	// Both are built here, so an empty cache answers with empty slices.
	for _, cached := range c.namespaces {
		inventory.Namespaces = append(inventory.Namespaces, cached)
	}
	for _, cached := range c.deployments {
		if namespace != metav1.NamespaceAll && cached.Namespace != namespace {
			continue
		}
		inventory.Deployments = append(inventory.Deployments, cached)
	}
	// Both are sorted on the way out rather than on the way in.
	slices.SortFunc(inventory.Namespaces, byNamespace)
	slices.SortFunc(inventory.Deployments, byDeployment)

	return inventory
}

// Watch keeps the cache in step with the cluster until ctx is cancelled.  A
// watch the api server ends is expected, and is started again from the
// version the cache reached; one it refuses is recovered from with a
// listing.
func (c *Cache) Watch(ctx context.Context) {
	for ctx.Err() == nil {
		err := c.follow(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("cluster watch ended, resynchronising")
			if err := c.Populate(ctx); err != nil {
				log.Error().Err(err).Msg("cannot repopulate cache")
			}
		}

		// An unwell cluster is given a moment before it is asked again.
		select {
		case <-ctx.Done():
		case <-time.After(c.retryInterval):
		}
	}
}

// follow watches until one of the watches fails or ctx is cancelled, and
// returns once both have stopped.
func (c *Cache) follow(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ended := make(chan error, 2)
	for _, follower := range []func(context.Context) error{c.followNamespaces, c.followDeployments} {
		go func() {
			ended <- follower(ctx)
		}()
	}

	err := <-ended
	cancel()
	<-ended

	return err
}

// followNamespaces watches the namespaces until the watch ends.
func (c *Cache) followNamespaces(ctx context.Context) error {
	watcher, err := c.k8s.WatchNamespaces(ctx, c.namespaceVersion())
	if err != nil {
		return fmt.Errorf("cannot watch namespaces: %w", err)
	}

	return c.fold(ctx, watcher, c.applyNamespace)
}

// followDeployments watches the deployments until the watch ends.
func (c *Cache) followDeployments(ctx context.Context) error {
	watcher, err := c.k8s.WatchDeployments(ctx, c.deploymentVersion())
	if err != nil {
		return fmt.Errorf("cannot watch deployments: %w", err)
	}

	return c.fold(ctx, watcher, c.applyDeployment)
}

// fold applies the events of one watch until it ends.  A watch that ends on
// its own is not a failure, and the connection is stopped here rather than
// left to the api server.
func (c *Cache) fold(ctx context.Context, watcher watch.Interface, apply func(watch.Event) error) error {
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}
			if err := apply(event); err != nil {
				return err
			}
		}
	}
}

// applyDeployment folds one deployment watch event into the cache.  The
// write lock is held for the event, so a reader never sees a deployment
// counted but not yet there.
func (c *Cache) applyDeployment(event watch.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch event.Type {
	case watch.Added, watch.Modified:
		deploy, err := deploymentOf(event)
		if err != nil {
			return err
		}
		c.deployments[cacheKey(deploy.Namespace, deploy.Name)] = newDeployment(deploy)
		c.advanceDeployment(deploy.ResourceVersion)
		refresh(deploymentResource)
	case watch.Deleted:
		deploy, err := deploymentOf(event)
		if err != nil {
			return err
		}
		delete(c.deployments, cacheKey(deploy.Namespace, deploy.Name))
		c.advanceDeployment(deploy.ResourceVersion)
		refresh(deploymentResource)
	case watch.Bookmark:
		// A bookmark moves the version forward and is not a refresh.
		c.advanceDeployment(bookmarkVersion(event))
	case watch.Error:
		return watchError(deploymentResource, event)
	}
	c.measure()

	return nil
}

// applyNamespace folds one namespace watch event into the cache.
func (c *Cache) applyNamespace(event watch.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch event.Type {
	case watch.Added, watch.Modified:
		namespace, err := namespaceOf(event)
		if err != nil {
			return err
		}
		c.namespaces[namespace.Name] = newNamespace(namespace)
		c.advanceNamespace(namespace.ResourceVersion)
		refresh(namespaceResource)
	case watch.Deleted:
		namespace, err := namespaceOf(event)
		if err != nil {
			return err
		}
		delete(c.namespaces, namespace.Name)
		c.advanceNamespace(namespace.ResourceVersion)
		refresh(namespaceResource)
	case watch.Bookmark:
		c.advanceNamespace(bookmarkVersion(event))
	case watch.Error:
		return watchError(namespaceResource, event)
	}
	c.measure()

	return nil
}

// measure tells the metrics how much of the cluster the cache holds.
func (c *Cache) measure() {
	cacheNamespaces.Set(float64(len(c.namespaces)))
	cacheDeployments.Set(float64(len(c.deployments)))
}

// refresh counts one refresh of resource.
func refresh(resource string) {
	cacheRefreshes.WithLabelValues(resource).Inc()
}

// advanceDeployment moves the deployment resource version forward.  An
// absent version is skipped, since watching from an empty one starts the api
// server from wherever it is now.
func (c *Cache) advanceDeployment(resourceVersion string) {
	if resourceVersion == "" {
		return
	}
	c.deploymentsVersion = resourceVersion
}

// advanceNamespace moves the namespace resource version forward.
func (c *Cache) advanceNamespace(resourceVersion string) {
	if resourceVersion == "" {
		return
	}
	c.namespacesVersion = resourceVersion
}

// deploymentVersion is the version the cached deployments reached.
func (c *Cache) deploymentVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.deploymentsVersion
}

// namespaceVersion is the version the cached namespaces reached, and the one
// the namespace watch takes up from.
func (c *Cache) namespaceVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.namespacesVersion
}

// deploymentOf reads the deployment out of a watch event.
func deploymentOf(event watch.Event) (*appsv1.Deployment, error) {
	deploy, ok := event.Object.(*appsv1.Deployment)
	if !ok {
		return nil, fmt.Errorf("cannot read deployment from %s event: %T", event.Type, event.Object)
	}
	return deploy, nil
}

// namespaceOf reads the namespace out of a watch event.
func namespaceOf(event watch.Event) (*corev1.Namespace, error) {
	namespace, ok := event.Object.(*corev1.Namespace)
	if !ok {
		return nil, fmt.Errorf("cannot read namespace from %s event: %T", event.Type, event.Object)
	}
	return namespace, nil
}

// bookmarkVersion is the version a bookmark carries.  A bookmark arrives as
// partial metadata, and one that is not moves nothing.
func bookmarkVersion(event watch.Event) string {
	meta, ok := event.Object.(*metav1.PartialObjectMetadata)
	if !ok {
		return ""
	}
	return meta.ResourceVersion
}

// watchError is the error an Error event is turned into.  FromObject always
// answers with an error, even for an object that is not a Status.
func watchError(resource string, event watch.Event) error {
	return fmt.Errorf("%s watch failed: %w", resource, apierrors.FromObject(event.Object))
}

// cacheKey finds a deployment, whose name cannot repeat within a namespace.
func cacheKey(namespace, name string) string {
	return namespace + "/" + name
}
