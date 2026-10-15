package clientgo

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/kerolasa/teleport-sre/internal"
)

// defaultLoglevel is how much there is to report until the configuration says.
const defaultLoglevel = "info"

// operation is what a single run of the tool does, of which the command line
// asks for at most one.
type operation string

const (
	list operation = "list"
	get  operation = "get"
	set  operation = "set"
	// version is answered without reaching the api, and so it is an operation
	// of the tool rather than a request to the api.
	version operation = "version"
)

var operations = []operation{list, get, set, version}

type options struct {
	// configPath is where the api address and the keys are read from.
	configPath string
	namespace  string
	deployment string
	operation  operation
	// replicas is what -set scales a deployment to.  No replicas is a number
	// of replicas all by itself, so the flag is asked about rather than the
	// number it holds.
	replicas int
}

func Run() int {
	// The configuration says what to report, and it is read after the flags.
	internal.SetupConsoleLogging(defaultLoglevel)

	command := options{}
	flags := commandLine(&command)
	flag.Parse()

	if err := command.wanted(mentioned(flags)); err != nil {
		log.Error().Err(err).Msg("cannot tell what to do")
		flags.Usage()
		return internal.ExitFailure
	}

	// The version is stamped into the binary, so it is answered without the
	// configuration and the keys a request needs.
	if command.operation == version {
		_, _ = fmt.Println(internal.Version)
		return internal.ExitSuccess
	}

	if err := command.check(); err != nil {
		log.Error().Err(err).Msg("cannot tell what to do")
		return internal.ExitFailure
	}

	conf, err := internal.ReadConfig(command.configPath)
	if err != nil {
		log.Error().Err(err).Msg("cannot read configuration")
		return internal.ExitFailure
	}
	internal.SetupConsoleLogging(conf.Loglevel)

	client, err := newClient(conf, os.Stdout)
	if err != nil {
		log.Error().Err(err).Msg("cannot talk to the api")
		return internal.ExitFailure
	}

	if err := operate(client, command); err != nil {
		log.Error().Err(err).Str("operation", string(command.operation)).Msg("the operation did not happen")
		return internal.ExitFailure
	}

	return internal.ExitSuccess
}

// commandLine is the flags of the tool.  The flags that only say which
// operation was asked for are left unbound, since the flag set answers whether
// they were mentioned.
func commandLine(options *options) *flag.FlagSet {
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.StringVar(&options.configPath, "config", internal.DefaultConfigPath,
		"Read the api address and the keys from this file")
	flag.StringVar(&options.namespace, "namespace", "",
		"Narrow down operation to specific `ns`")
	flag.StringVar(&options.deployment, "deployment", "",
		"Narrow down operation to specific `name`")
	flag.Bool("list", false,
		"List namespaces and deployments. Use -namespace to narrow down listing")
	flag.Bool("get", false,
		"Get number of replicas")
	flag.IntVar(&options.replicas, "set", 0,
		"Set number of replicas. Requires use of -namespace and -deployment")
	flag.Bool("version", false,
		"Print the version of this tool and exit")

	return flag.CommandLine
}

// wanted is the operation the command line asks for; none at all, or two of
// them, is a question with no answer to it.
func (o *options) wanted(given map[string]bool) error {
	asked := []operation{}
	for _, operation := range operations {
		if given[string(operation)] {
			asked = append(asked, operation)
		}
	}

	switch len(asked) {
	case 0:
		return fmt.Errorf("nothing to do, use %s", flagNames(operations))
	case 1:
		o.operation = asked[0]
		return nil
	default:
		return fmt.Errorf("only one of %s can be asked for at a time", flagNames(asked))
	}
}

// check is what the operation needs told alongside it, since a request the api
// cannot act on has changed nothing.
func (o options) check() error {
	if o.operation != set {
		return nil
	}

	switch {
	case o.namespace == "":
		return errors.New("-set needs a namespace, since a deployment name can repeat in another")
	case o.deployment == "":
		return errors.New("-set needs the name of the deployment to scale")
	case o.replicas < 0:
		return fmt.Errorf("the replica count is a whole number of zero or more, got %d", o.replicas)
	}

	return nil
}

func operate(client *client, command options) error {
	switch command.operation {
	case list:
		return client.list(command.namespace)
	case get:
		return client.get(command.namespace, command.deployment)
	case set:
		return client.set(command.namespace, command.deployment, command.replicas)
	}

	return fmt.Errorf("the command line asked for %q, which is nothing this tool does", command.operation)
}

// mentioned is which of the flags the command line mentioned, since a flag left
// out has nothing to say however telling the value it holds looks.
func mentioned(flags *flag.FlagSet) map[string]bool {
	asked := map[string]bool{}
	flags.Visit(func(f *flag.Flag) {
		asked[f.Name] = true
	})

	return asked
}

func flagNames(operations []operation) string {
	flags := make([]string, 0, len(operations))
	for _, operation := range operations {
		flags = append(flags, "-"+string(operation))
	}

	return strings.Join(flags, ", ")
}
