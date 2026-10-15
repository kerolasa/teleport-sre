package controllerruntime

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/kerolasa/teleport-sre/internal"
)

const (
	EXIT_SUCCESS = 0
	EXIT_FAILURE = 1
)

// shutdownTimeout is how long a drain is given.
const shutdownTimeout = 42 * time.Second

// populateTimeout bounds the listing that fills the cache during startup.
const populateTimeout = 30 * time.Second

func Run() int {
	configPath := flag.String("config", "./sample-config.toml", "path to configuration")
	flag.Parse()

	config, err := ReadConfig(*configPath)
	if err != nil {
		log.Error().Err(err).Msg("cannot read configuration")
		return EXIT_FAILURE
	}
	internal.SetupLogging(config.Loglevel)

	kubeConn, err := NewKube(config.Kube)
	if err != nil {
		log.Error().Err(err).Msg("cannot find kubernetes")
		return EXIT_FAILURE
	}

	// The cache is filled before anything is served.
	cache := NewCache(kubeConn)
	populateCtx, cancelPopulate := context.WithTimeout(context.Background(), populateTimeout)
	defer cancelPopulate()
	if err := cache.Populate(populateCtx); err != nil {
		log.Error().Err(err).Msg("cannot populate cache")
		return EXIT_FAILURE
	}
	populated := cache.Get("")
	log.Info().
		Int("namespaces", len(populated.Namespaces)).
		Int("deployments", len(populated.Deployments)).
		Msg("cache populated")

	// The watch gets a context of its own, so the shutdown can stop it.
	cacheCtx, stopCache := context.WithCancel(context.Background())
	cacheStopped := make(chan struct{})
	go func() {
		defer close(cacheStopped)
		cache.Watch(cacheCtx)
	}()
	defer stopCache()

	apiServer := ListenAPI(config, kubeConn, cache)
	metricsServer := ListenMetrics(config, kubeConn)

	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	var listeners sync.WaitGroup
	serve := func(name string, server *http.Server) {
		listeners.Add(1)
		go func() {
			defer listeners.Done()
			log.Info().Str("server", name).Str("address", server.Addr).Msg("listening")
			err := server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			results <- result{name: name, err: err}
		}()
	}
	serve("api", apiServer)
	serve("metrics", metricsServer)

	// Signals are buffered to avoid losing them.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	exit := EXIT_SUCCESS
	select {
	case sig := <-signals:
		log.Info().Str("signal", sig.String()).Msg("shutdown signal received")
	case res := <-results:
		if res.err != nil {
			log.Error().Err(res.err).Str("server", res.name).Msg("server stopped unexpectedly")
			exit = EXIT_FAILURE
		}
	}

	// Stop intercepting signals, so that the runtime handles any later one.
	signal.Stop(signals)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// The watch stops before the drain, so nothing writes to the cache as the api goes away.
	stopCache()
	select {
	case <-cacheStopped:
		log.Info().Msg("cache watch stopped")
	case <-ctx.Done():
		log.Warn().Msg("cache watch did not stop before the shutdown deadline")
	}

	shutdownServer(ctx, "api", apiServer)
	shutdownServer(ctx, "metrics", metricsServer)

	listeners.Wait()
	close(results)
	for res := range results {
		if res.err != nil {
			log.Error().Err(res.err).Str("server", res.name).Msg("server exited with error")
			exit = EXIT_FAILURE
		}
	}
	if exit == EXIT_SUCCESS {
		log.Info().Msg("shutdown complete")
	}
	return exit
}

// shutdownServer drains a listener.
func shutdownServer(ctx context.Context, name string, server *http.Server) {
	log.Info().Str("server", name).Msg("draining")
	if err := server.Shutdown(ctx); err != nil {
		log.Error().Err(err).Str("server", name).Msg("server did not drain cleanly")
		if err := server.Close(); err != nil {
			log.Error().Err(err).Str("server", name).Msg("cannot close server")
		}
		return
	}
	log.Info().Str("server", name).Msg("drained")
}
