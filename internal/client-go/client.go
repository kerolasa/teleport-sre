package clientgo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kerolasa/teleport-sre/internal"
)

// requestTimeout bounds a request.  The api server gives the cluster less time
// than this before it calls an update a failure.
const requestTimeout = 30 * time.Second

// client is a caller of the controller api, writing what the api answers to
// out the way the api sent it.
type client struct {
	// base is where the api is served, without a route on it.
	base *url.URL
	// out is where the answers of the api are written.
	out io.Writer
	// http is the connection the requests are made over, which is mTLS.
	http *http.Client
}

func newClient(conf internal.Conf, out io.Writer) (*client, error) {
	base, err := apiURL(conf)
	if err != nil {
		return nil, err
	}

	// The api server turns a caller it cannot identify away before it answers,
	// so the certificate is asked for here rather than after the first request.
	if err := internal.Configured(
		internal.Setting{Name: "tls ca", Path: conf.TLS.CA},
		internal.Setting{Name: "client certificate", Path: conf.Client.Cert},
		internal.Setting{Name: "client key", Path: conf.Client.Key},
	); err != nil {
		return nil, err
	}

	secure, err := internal.MutualTLS(conf.Client.Cert, conf.Client.Key, conf.TLS.CA)
	if err != nil {
		return nil, err
	}

	return &client{
		base: base,
		out:  out,
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				TLSClientConfig: secure,
				// TODO: consider retry logic for the read only requests, which
				// the api answers from the cache it keeps.
			},
		},
	}, nil
}

func (c *client) list(namespace string) error {
	query := url.Values{}
	given(query, "ns", namespace)

	return c.ask(http.MethodGet, "list", query)
}

func (c *client) get(namespace, deployment string) error {
	query := url.Values{}
	given(query, "ns", namespace)
	given(query, "deployment", deployment)

	return c.ask(http.MethodGet, "deployment", query)
}

func (c *client) set(namespace, deployment string, replicas int) error {
	query := url.Values{"replicas": {strconv.Itoa(replicas)}}
	given(query, "ns", namespace)
	given(query, "deployment", deployment)

	return c.ask(http.MethodPut, "deployment", query)
}

// ask performs the request and writes the answer out.  A refusal is an error,
// and is written out all the same, since that is where its reason is.
func (c *client) ask(method, route string, query url.Values) error {
	asked := c.base.JoinPath(route)
	asked.RawQuery = query.Encode()

	request, err := http.NewRequest(method, asked.String(), nil)
	if err != nil {
		return fmt.Errorf("cannot ask for %s %s: %w", method, asked, err)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("cannot reach the api at %s: %w", c.base, err)
	}
	defer func() {
		// Read in full below, so a failed close leaves nothing to report.
		_ = response.Body.Close()
	}()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("cannot read what the api answered: %w", err)
	}
	if err := c.write(answer); err != nil {
		return err
	}

	return refused(response, answer)
}

func (c *client) write(answer []byte) error {
	if _, err := c.out.Write(answer); err != nil {
		return fmt.Errorf("cannot write what the api answered: %w", err)
	}

	// The api ends nothing with a newline, and a shell prompt on the same line
	// as the answer is no fun.
	if _, err := io.WriteString(c.out, "\n"); err != nil {
		return fmt.Errorf("cannot write what the api answered: %w", err)
	}

	return nil
}

// refused is why the api turned the request down, or nothing when it did not.
// The status on its own says little, so the reason the api carries is reported.
func refused(response *http.Response, answer []byte) error {
	if response.StatusCode < http.StatusBadRequest {
		return nil
	}

	var failure struct {
		Err string `json:"error"`
	}
	if err := json.Unmarshal(answer, &failure); err != nil || failure.Err == "" {
		return fmt.Errorf("the api answered %s", response.Status)
	}

	return fmt.Errorf("the api answered %s: %s", response.Status, failure.Err)
}

// given adds a part of the query to narrow an operation down with.  A part that
// was not given is left out, since the api reads a missing part as "any".
func given(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}
