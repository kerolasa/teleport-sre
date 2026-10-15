package controllerruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type httpStatus struct {
	Status string `json:"status" example:"done"`
}

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

// defaultMaxReplicas is the replica limit when the configuration has none.
const defaultMaxReplicas = 100

// scaleTimeout bounds how long an update waits for the cluster.
const scaleTimeout = 10 * time.Second

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
	// cache is what the read only routes answer from, and what an update is
	// recorded in.
	cache *Cache
	// maxReplicas is the replica limit, negative when it is lifted.
	maxReplicas int32
}

func ListenAPI(conf Conf, kubeConn KubeConn, cache *Cache) (*http.Server, error) {
	// The keys are read before anything is served, so a service that cannot
	// authenticate its callers never starts.
	secure, err := tlsConfig(conf.TLS)
	if err != nil {
		return nil, err
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(CountRequests())

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, httpError{Err: "not found"})
	})

	api := &api{
		prefix:      conf.API.Prefix,
		kubeConn:    kubeConn,
		cache:       cache,
		maxReplicas: configuredMaxReplicas(conf),
	}
	prefix := r.Group(api.prefix)
	prefix.GET("/", api.rootPage)
	prefix.GET("/list", api.list)
	prefix.GET("/deployment", api.deployment)
	prefix.PUT("/deployment", api.setReplicas)
	prefix.GET("/ui/list", api.uiList)
	prefix.GET("/ui/deployment", api.uiReplicas)
	prefix.GET("/ui/deployment/edit", api.uiEditReplicas)
	prefix.PUT("/ui/deployment", api.uiSetReplicas)
	return &http.Server{

		Addr:              net.JoinHostPort(conf.API.Host, strconv.Itoa(int(conf.API.Port))),
		Handler:           r,
		TLSConfig:         secure,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

// url joins a route onto the prefix.
func (a *api) url(route string) string {
	return path.Join(a.prefix, route)
}

// urlOf is a route under the api prefix, asked about one deployment.  The
// query is encoded, since a name the cluster allows can carry characters a url
// does not take as they are.
func (a *api) urlOf(route string, deploy Deployment) string {
	return a.url(route) + "?" + url.Values{
		"ns":         {deploy.Namespace},
		"deployment": {deploy.Name},
	}.Encode()
}

// configuredMaxReplicas is the configured limit, or the default when it is
// left out.
func configuredMaxReplicas(conf Conf) int32 {
	if conf.MaxReplicas == 0 {
		return defaultMaxReplicas
	}

	return conf.MaxReplicas
}

// replicaLimit is the limit as a number, with a lifted limit as far as a count
// reaches.
func (a *api) replicaLimit() int32 {
	if a.maxReplicas < 0 {
		return math.MaxInt32
	}

	return a.maxReplicas
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
	inventory, err := a.inventory(namespace)
	if err != nil {
		refuse(c, err)
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
	namespace, name := askedNamespace(c), c.Query("deployment")
	inventory, err := a.inventory(namespace)
	if err != nil {
		refuse(c, err)
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

// setReplicas sets the number of replicas of the deployment the request names.
func (a *api) setReplicas(c *gin.Context) {
	deploy, replicas, err := a.askedUpdate(c)
	if err != nil {
		refuse(c, err)
		return
	}

	if err := a.scale(c, deploy, replicas); err != nil {
		refuse(c, err)
		return
	}

	c.JSON(http.StatusOK, httpStatus{Status: "done"})
}

// scale sets the replicas of a deployment and caches what the cluster said.
func (a *api) scale(c *gin.Context, deploy Deployment, replicas int32) error {
	ctx, cancel := context.WithTimeout(c.Request.Context(), scaleTimeout)
	defer cancel()

	if err := a.kubeConn.SetReplicas(ctx, deploy.Namespace, deploy.Name, replicas); err != nil {
		return err
	}

	deploy.Replicas = replicas
	a.cache.Set(deploy)

	return nil
}

// uiList renders the table fragment the list button swaps in.
func (a *api) uiList(c *gin.Context) {
	inventory, err := a.inventory(askedNamespace(c))
	if err != nil {
		refuse(c, err)
		return
	}

	a.renderDeployments(c, inventory.Deployments)
}

// uiReplicas renders the replicas cell the way the table shows it.
func (a *api) uiReplicas(c *gin.Context) {
	deploy, err := a.askedDeployment(c)
	if err != nil {
		a.refuseCell(c, Deployment{}, err)
		return
	}

	a.render(c, http.StatusOK, "replicas.html", a.cell(deploy, ""))
}

// uiEditReplicas renders the replicas cell as the form it is edited with.
func (a *api) uiEditReplicas(c *gin.Context) {
	deploy, err := a.askedDeployment(c)
	if err != nil {
		a.refuseCell(c, Deployment{}, err)
		return
	}

	cell := a.cell(deploy, "")
	cell.Editing = true
	a.render(c, http.StatusOK, "replicas.html", cell)
}

// uiSetReplicas applies an update and answers with the cell the table shows
// afterwards, carrying the reason when it did not happen.
func (a *api) uiSetReplicas(c *gin.Context) {
	deploy, replicas, err := a.askedUpdate(c)
	if err != nil {
		a.refuseCell(c, deploy, err)
		return
	}

	if err := a.scale(c, deploy, replicas); err != nil {
		a.refuseCell(c, deploy, err)
		return
	}

	deploy.Replicas = replicas
	a.render(c, http.StatusOK, "replicas.html", a.cell(deploy, ""))
}

// refuseCell puts the reason a request failed where the cell of the row was.
func (a *api) refuseCell(c *gin.Context, deploy Deployment, err error) {
	if deploy.Name == "" {
		a.render(c, statusOf(err), "replicas-error.html", errorView{Message: err.Error()})
		return
	}

	a.render(c, statusOf(err), "replicas.html", a.cell(deploy, err.Error()))
}

// cell is the replicas cell of a deployment, carrying a failure reason.
func (a *api) cell(deploy Deployment, message string) replicasView {
	return replicasView{
		ReplicasURL: a.urlOf("ui/deployment", deploy),
		EditURL:     a.urlOf("ui/deployment/edit", deploy),
		Replicas:    deploy.Replicas,
		Limit:       a.replicaLimit(),
		Message:     message,
	}
}

// askedUpdate is the deployment to change, and the replicas asked for.
func (a *api) askedUpdate(c *gin.Context) (Deployment, int32, error) {
	deploy, err := a.askedDeployment(c)
	if err != nil {
		return deploy, 0, err
	}

	replicas, err := a.askedReplicas(c)
	if err != nil {
		return deploy, 0, err
	}

	return deploy, replicas, nil
}

// askedDeployment is the deployment the request names, which needs a
// namespace, as a name on its own can repeat in another.
func (a *api) askedDeployment(c *gin.Context) (Deployment, error) {
	name := c.Query("deployment")
	if name == "" {
		return Deployment{}, newRefusal(http.StatusBadRequest, "no deployment name was given")
	}

	namespace := askedNamespace(c)
	if namespace == "" {
		return Deployment{}, newRefusal(http.StatusBadRequest,
			"no namespace was given, and a deployment name can repeat in another namespace")
	}

	inventory, err := a.inventory(namespace)
	if err != nil {
		return Deployment{}, err
	}

	// The namespace is settled, so the name matches at most one.
	matches := named(inventory.Deployments, name)
	if len(matches) == 0 {
		return Deployment{}, newRefusal(http.StatusNotFound,
			"deployment %q does not exist in namespace %q", name, namespace)
	}

	return matches[0], nil
}

// askedReplicas is the replicas asked for, from the query or the form body.
func (a *api) askedReplicas(c *gin.Context) (int32, error) {
	asked := c.Request.FormValue("replicas")
	if asked == "" {
		return 0, newRefusal(http.StatusBadRequest,
			"no replicas were given, and they must be a whole number of zero or more")
	}

	// The page is told the limit, so the form refuses the rest.
	limit := int64(a.replicaLimit())

	count, err := strconv.ParseInt(asked, 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		// A count too large to count here is over any limit.
		return 0, newRefusal(http.StatusNotAcceptable, "replicas %s is over the limit of %d", asked, limit)
	}
	if err != nil {
		return 0, newRefusal(http.StatusBadRequest, "replicas %q is not a whole number", asked)
	}
	if count < 0 {
		return 0, newRefusal(http.StatusBadRequest,
			"replicas %d is fewer than zero, and must be a whole number of zero or more", count)
	}
	if count > limit {
		return 0, newRefusal(http.StatusNotAcceptable,
			"replicas %d is over the limit of %d", count, limit)
	}

	return int32(count), nil
}

// askedNamespace is the namespace asked about.  `ns` and `namespace` are
// both taken.
func askedNamespace(c *gin.Context) string {
	if namespace := c.Query("ns"); namespace != "" {
		return namespace
	}

	return c.Query("namespace")
}

// inventory is the cached cluster, or the reason the request cannot be
// answered from it.
func (a *api) inventory(namespace string) (Inventory, error) {
	if !a.cache.Populated() {
		return Inventory{}, newRefusal(http.StatusServiceUnavailable, cachePending)
	}

	inventory := a.cache.Get(namespace)
	if namespace != "" && !slices.ContainsFunc(inventory.Namespaces, func(cached Namespace) bool {
		return cached.Name == namespace
	}) {
		return Inventory{}, newRefusal(http.StatusNotFound, "namespace %q does not exist", namespace)
	}

	return inventory, nil
}

// renderDeployments renders the table fragment with the given rows.
func (a *api) renderDeployments(c *gin.Context, deployments []Deployment) {
	rows := make([]deploymentRow, 0, len(deployments))
	for _, deploy := range deployments {
		rows = append(rows, deploymentRow{
			Namespace: deploy.Namespace,
			Name:      deploy.Name,
			Cell:      a.cell(deploy, ""),
		})
	}

	a.render(c, http.StatusOK, "deployments.html", deploymentsView{Rows: rows})
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

// refusal is a request the service will not act on, with the status the caller
// is answered with and the reason they are given for it.
type refusal struct {
	status int
	reason string
}

func (r *refusal) Error() string { return r.reason }

// newRefusal is a refusal with a formatted reason.
func newRefusal(status int, format string, args ...any) error {
	return &refusal{status: status, reason: fmt.Sprintf(format, args...)}
}

// refuse answers a request the service will not act on.
func refuse(c *gin.Context, err error) {
	c.JSON(statusOf(err), httpError{Err: err.Error()})
}

// statusOf is the status a failed request is answered with.  A cluster failure
// is reported as the upstream failure it is.
func statusOf(err error) int {
	var turned *refusal
	if errors.As(err, &turned) {
		return turned.status
	}

	switch {
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return http.StatusForbidden
	case apierrors.IsConflict(err):
		return http.StatusConflict
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		return http.StatusBadRequest
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
