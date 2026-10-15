package controllerruntime

import (
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type httpError struct {
	Err string `json:"error" example:"Error message"`
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
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DisableConsoleColor()

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
	prefix.GET("/list", api.deployments)
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
		ListURL: a.url("list"),
	})
}

// deployments renders the table fragment the list button swaps in.  The rows
// come from the cache, so asking costs the cluster nothing.
func (a *api) deployments(c *gin.Context) {
	a.renderDeployments(c, a.cache.Get(c.Query("ns")).Deployments)
}

// renderDeployments renders the table fragment with the given rows.
func (a *api) renderDeployments(c *gin.Context, deployments []Deployment) {
	a.render(c, http.StatusOK, "deployments.html", deploymentsView{
		DeploymentURL: a.url("deployment"),
		Deployments:   deployments,
	})
}
