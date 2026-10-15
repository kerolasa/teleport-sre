package controllerruntime

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kerolasa/teleport-sre/internal"
)

// defaultMetricsPath is the scrape path when the configuration has none.
const defaultMetricsPath = "/metrics"

// heartbeatPath is where a probe asks whether the controller is alive.
const heartbeatPath = "/_heartbeat"

// versionPath is where a person asks which version is running.
const versionPath = "/version"

// unmatchedRoute labels the requests that matched no route.
const unmatchedRoute = "unmatched"

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

func init() {
	prometheus.MustRegister(apiRequests)
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
func ListenMetrics(conf Conf) *http.Server {
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
	mux.HandleFunc(heartbeatPath, heartbeat)
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

// heartbeat prints ok while the controller is alive.  It sits here rather
// than on the api listen because a probe reaches it without the gateway.
func heartbeat(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// version prints the version stamped into the binary.
func version(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(internal.Version))
}
