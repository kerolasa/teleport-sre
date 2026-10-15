package clientgo

import (
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/kerolasa/teleport-sre/internal"
)

// loopback is where a service listening on every address of the host is to a
// caller sharing that host with it.
const loopback = "localhost"

// apiURL is where the controller serves the api, with no route on it yet.  The
// scheme is https, since the api server terminates TLS.
func apiURL(conf internal.Conf) (*url.URL, error) {
	if conf.API.Port == 0 {
		return nil, errors.New("the configuration says nothing about the api port")
	}

	base := &url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(reachableAt(conf), strconv.Itoa(int(conf.API.Port))),
		Path:   "/",
	}

	return base.JoinPath(conf.API.Prefix), nil
}

func reachableAt(conf internal.Conf) string {
	if address := net.ParseIP(conf.API.Host); conf.API.Host == "" ||
		(address != nil && address.IsUnspecified()) {
		return loopback
	}

	return conf.API.Host
}
