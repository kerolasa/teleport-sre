package controllerruntime

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"

	"github.com/kerolasa/teleport-sre/internal"
)

// defaultMetricsPath is the scrape path when the configuration has none.
const defaultMetricsPath = "/metrics"

// heartbeatPath is where a probe asks whether the controller is alive.
const heartbeatPath = "/_heartbeat"

// versionPath is where a person asks which version is running.
const versionPath = "/version"

// aliveBody is what the heartbeat answers while the api server answers.
const aliveBody = "ok"

// notAliveBody is what the heartbeat answers while the api server does not.
const notAliveBody = "not alive"

// heartbeatTimeout is how long the heartbeat waits for the api server.
const heartbeatTimeout = 3 * time.Second

// unmatchedRoute labels the requests that matched no route.
const unmatchedRoute = "unmatched"

// namespaceResource and deploymentResource are the cacheRefreshes label
// values, naming the two halves of the cache.
const (
	namespaceResource  = "namespaces"
	deploymentResource = "deployments"
)

// apiRequests counts the requests the api served.
var apiRequests = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "teleport",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total number of requests served, by uri.",
	},
	[]string{"uri"},
)

// cacheNamespaces reports how many namespaces the cache is holding.
var cacheNamespaces = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "teleport",
		Subsystem: "cache",
		Name:      "namespaces",
		Help:      "Number of namespaces the cache is holding.",
	},
)

// cacheDeployments reports how many deployments the cache is holding.
var cacheDeployments = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "teleport",
		Subsystem: "cache",
		Name:      "deployments",
		Help:      "Number of deployments the cache is holding.",
	},
)

// cacheRefreshes counts the refreshes of the cache, by resource.
//
// A refresh is a change folded into the cache from a watch, or the listing
// that fills it.  A bookmark folds no change in and is not counted, since a
// quiet cluster sends those only to say it is still there.
var cacheRefreshes = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "teleport",
		Subsystem: "cache",
		Name:      "refreshes_total",
		Help:      "Total number of times the cache has been refreshed, by resource.",
	},
	[]string{"resource"},
)

func init() {
	prometheus.MustRegister(apiRequests, cacheNamespaces, cacheDeployments, cacheRefreshes)
	// Both label values are given up front, so that each series is reported
	// from the start rather than appearing once its first change is seen.
	cacheRefreshes.WithLabelValues(namespaceResource)
	cacheRefreshes.WithLabelValues(deploymentResource)
}

// CountRequests records how often each uri is requested.
func CountRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		uri := c.FullPath()
		if uri == "" {
			uri = unmatchedRoute
		}
		apiRequests.WithLabelValues(uri).Inc()
		c.Next()
	}
}

// ListenMetrics builds the metrics, heartbeat and version server.  It listens
// on its own address so that it is not exposed with the api.
func ListenMetrics(conf Conf, kubeConn KubeConn) *http.Server {
	path := conf.Metrics.Path
	if path == "" {
		path = defaultMetricsPath
	}
	// A pattern without a leading slash panics when registered.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	mux := http.NewServeMux()
	mux.Handle(path, promhttp.Handler())
	mux.HandleFunc(heartbeatPath, heartbeat(kubeConn))
	mux.HandleFunc(versionPath, version)

	return &http.Server{
		Addr:              net.JoinHostPort(conf.Metrics.Host, strconv.Itoa(int(conf.Metrics.Port))),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// heartbeat answers whether the api server answers, since a controller that
// cannot reach the cluster has nothing to serve.  It sits here rather than on
// the api listen because a probe reaches it without the gateway.
func heartbeat(kubeConn KubeConn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), heartbeatTimeout)
		defer cancel()

		if err := kubeConn.Reachable(ctx); err != nil {
			log.Error().Err(err).Msg("the heartbeat could not reach the api server")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(notAliveBody))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(aliveBody))
	}
}

// version prints the version stamped into the binary.
func version(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(internal.Version))
}
