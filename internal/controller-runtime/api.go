package controllerruntime

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type httpError struct {
	Err string `json:"error" example:"Error message"`
}

// listView is the answer to a list request.
type listView struct {
	Namespaces  []Namespace  `json:"namespaces,omitempty"`
	Deployments []Deployment `json:"deployments"`
}

// deploymentsJSON is the answer to a request that does not resolve to a single
// deployment.
type deploymentsJSON struct {
	Deployments []Deployment `json:"deployments"`
}

// cachePending is the answer to a request that arrives before the cache has
// been filled.
const cachePending = "the cluster is still being read, please try again later"

// init quiets gin down, since its settings are globals.
func init() {
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DisableConsoleColor()
}

// api serves the web ui.

type api struct {
	prefix string
	// kubeConn is the client to the kubernetes.
	kubeConn KubeConn
	// cache is what the read only routes answer from.
	cache *Cache
}

func ListenAPI(conf Conf, kubeConn KubeConn, cache *Cache) *http.Server {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(CountRequests())

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, httpError{Err: "not found"})
	})

	api := &api{
		prefix:   conf.API.Prefix,
		kubeConn: kubeConn,
		cache:    cache,
	}
	prefix := r.Group(api.prefix)
	prefix.GET("/", api.rootPage)
	prefix.GET("/list", api.list)
	prefix.GET("/deployment", api.deployment)
	prefix.GET("/ui/list", api.uiList)
	return &http.Server{

		Addr:              net.JoinHostPort(conf.API.Host, strconv.Itoa(int(conf.API.Port))),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// url joins a route onto the prefix.
func (a *api) url(route string) string {
	return path.Join(a.prefix, route)
}

// rootPage renders the web ui.
func (a *api) rootPage(c *gin.Context) {
	a.render(c, http.StatusOK, "root.html", rootView{
		ListURL: a.url("ui/list"),
	})
}

// list reports the deployments asked about, with the namespaces when the
// request names none.
func (a *api) list(c *gin.Context) {
	namespace := askedNamespace(c)
	inventory, ok := a.inventory(c, namespace)
	if !ok {
		return
	}

	view := listView{Deployments: inventory.Deployments}
	// A named namespace is answered with its deployments alone.
	if namespace == "" {
		view.Namespaces = inventory.Namespaces
	}

	c.JSON(http.StatusOK, view)
}

// deployment reports the deployment the request names.
func (a *api) deployment(c *gin.Context) {
	namespace, name := askedNamespace(c), c.Query("name")
	inventory, ok := a.inventory(c, namespace)
	if !ok {
		return
	}

	matches := named(inventory.Deployments, name)
	if name == "" {
		c.JSON(http.StatusOK, deploymentsJSON{Deployments: matches})
		return
	}
	if len(matches) == 0 {
		if namespace == "" {
			c.JSON(http.StatusNotFound, httpError{Err: fmt.Sprintf("deployment %q does not exist", name)})
			return
		}
		c.JSON(http.StatusNotFound, httpError{
			Err: fmt.Sprintf("deployment %q does not exist in namespace %q", name, namespace),
		})
		return
	}
	if len(matches) == 1 {
		c.JSON(http.StatusOK, matches[0])
		return
	}

	c.JSON(http.StatusOK, deploymentsJSON{Deployments: matches})
}

// uiList renders the table fragment the list button swaps in.
func (a *api) uiList(c *gin.Context) {
	inventory, ok := a.inventory(c, askedNamespace(c))
	if !ok {
		return
	}

	a.renderDeployments(c, inventory.Deployments)
}

// askedNamespace is the namespace asked about.  `ns` and `namespace` are
// both taken.
func askedNamespace(c *gin.Context) string {
	if namespace := c.Query("ns"); namespace != "" {
		return namespace
	}

	return c.Query("namespace")
}

// inventory is the cached cluster, and whether the request can be answered
// from it.  An unfilled cache is not an empty cluster, and a namespace not in
// the cache is missing rather than empty.
func (a *api) inventory(c *gin.Context, namespace string) (Inventory, bool) {
	if !a.cache.Populated() {
		c.JSON(http.StatusServiceUnavailable, httpError{Err: cachePending})
		return Inventory{}, false
	}

	inventory := a.cache.Get(namespace)
	if namespace != "" && !slices.ContainsFunc(inventory.Namespaces, func(cached Namespace) bool {
		return cached.Name == namespace
	}) {
		c.JSON(http.StatusNotFound, httpError{Err: fmt.Sprintf("namespace %q does not exist", namespace)})
		return Inventory{}, false
	}

	return inventory, true
}

// renderDeployments renders the table fragment with the given rows.
func (a *api) renderDeployments(c *gin.Context, deployments []Deployment) {
	a.render(c, http.StatusOK, "deployments.html", deploymentsView{
		DeploymentURL: a.url("deployment"),
		Deployments:   deployments,
	})
}

// named returns the deployments called name, or all of them when name is
// empty.  They are copied rather than narrowed in place, since the cache
// handed them over for this request alone.
func named(deployments []Deployment, name string) []Deployment {
	if name == "" {
		return deployments
	}

	return slices.DeleteFunc(slices.Clone(deployments), func(deploy Deployment) bool {
		return deploy.Name != name
	})
}
