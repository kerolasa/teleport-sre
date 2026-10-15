package clientgo

import (
	"bytes"
	"strings"
	"testing"
)

func TestWanted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mentioned map[string]bool
		operation operation
		// complained is what the failure has to mention.
		complained string
	}{
		{
			name:      "a listing",
			mentioned: map[string]bool{"list": true},
			operation: list,
		},
		{
			name:      "a read",
			mentioned: map[string]bool{"get": true},
			operation: get,
		},
		{
			// The count of replicas says nothing on its own; it is the flag
			// being there that makes it an update.
			name:      "an update of no replicas",
			mentioned: map[string]bool{"set": true},
			operation: set,
		},
		{
			// The other flags narrow an operation down rather than say which
			// one it is.
			name:      "an operation narrowed down",
			mentioned: map[string]bool{"config": true, "namespace": true, "deployment": true, "get": true},
			operation: get,
		},
		{
			// The version is stamped into the binary, so it is asked for on its
			// own rather than along with anything else.
			name:      "the version",
			mentioned: map[string]bool{"version": true},
			operation: version,
		},
		{
			name:       "a command line that asks for nothing",
			mentioned:  map[string]bool{"config": true, "namespace": true},
			complained: "nothing to do, use -list, -get, -set, -version",
		},
		{
			name:       "a command line that asks for two things",
			mentioned:  map[string]bool{"list": true, "set": true},
			complained: "only one of -list, -set can be asked for at a time",
		},
		{
			// The version is not a request to the api, so asking for it along
			// with one is as much of a question as asking for two.
			name:       "a command line that asks for the version and an operation",
			mentioned:  map[string]bool{"list": true, "version": true},
			complained: "only one of -list, -version can be asked for at a time",
		},
		{
			name:       "a command line that asks for all three",
			mentioned:  map[string]bool{"list": true, "get": true, "set": true},
			complained: "only one of -list, -get, -set can be asked for at a time",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			command := options{}
			err := command.wanted(test.mentioned)
			if test.complained != "" {
				if err == nil {
					t.Fatalf("wanted() settled on %q, want it to complain", command.operation)
				}
				if !strings.Contains(err.Error(), test.complained) {
					t.Errorf("wanted() said %q, want it to mention %q", err, test.complained)
				}
				return
			}
			if err != nil {
				t.Fatalf("wanted() returned an error: %v", err)
			}
			if command.operation != test.operation {
				t.Errorf("wanted() settled on %q, want %q", command.operation, test.operation)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		operation  operation
		namespace  string
		deployment string
		replicas   int
		// complained is what the failure has to mention.
		complained string
	}{
		{
			name:      "a listing of every namespace",
			operation: list,
		},
		{
			name:       "a read of a deployment in every namespace",
			operation:  get,
			deployment: "front",
		},
		{
			name:       "an update told everything it needs",
			operation:  set,
			namespace:  "web",
			deployment: "front",
			replicas:   3,
		},
		{
			// No replicas is a number of replicas, and the api server will
			// scale a deployment out of existence.
			name:       "an update of no replicas",
			operation:  set,
			namespace:  "web",
			deployment: "front",
		},
		{
			name:       "an update with no namespace",
			operation:  set,
			deployment: "front",
			replicas:   3,
			complained: "-set needs a namespace",
		},
		{
			name:       "an update with no deployment",
			operation:  set,
			namespace:  "web",
			replicas:   3,
			complained: "-set needs the name of the deployment",
		},
		{
			name:       "an update of fewer than no replicas",
			operation:  set,
			namespace:  "web",
			deployment: "front",
			replicas:   -1,
			complained: "a whole number of zero or more, got -1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			command := options{
				operation:  test.operation,
				namespace:  test.namespace,
				deployment: test.deployment,
				replicas:   test.replicas,
			}
			err := command.check()
			if test.complained != "" {
				if err == nil {
					t.Fatal("check() is happy with a request the api cannot act on")
				}
				if !strings.Contains(err.Error(), test.complained) {
					t.Errorf("check() said %q, want it to mention %q", err, test.complained)
				}
				return
			}
			if err != nil {
				t.Errorf("check() returned an error: %v", err)
			}
		})
	}
}

func TestOperateWithNothingToDo(t *testing.T) {
	t.Parallel()

	// wanted() settles on one of the three, so anything else is a bug rather
	// than something a user can bring about.
	if err := operate(nil, options{operation: "count"}); err == nil {
		t.Error("operate() is happy to perform an operation that does not exist")
	}
}

func TestUsage(t *testing.T) {
	// The flags are set on the flag set of the process, which knows only one
	// set of them, so this is the one test that asks for them.
	command := options{}
	flags := commandLine(&command)

	printed := &bytes.Buffer{}
	flags.SetOutput(printed)
	flags.Usage()
	usage := printed.String()

	// The design doc spells out the usage this tool is expected to print.
	for _, want := range []string{
		"Usage: ",
		"[options]",
		"  -list\n",
		"List namespaces and deployments. Use -namespace to narrow down listing",
		"  -namespace ns\n",
		"Narrow down operation to specific ns",
		"  -deployment name\n",
		"Narrow down operation to specific name",
		"  -get\n",
		"Get number of replicas",
		"  -set int\n",
		"Set number of replicas. Requires use of -namespace and -deployment",
		"  -version\n",
		"Print the version of this tool and exit",
		"  -config string\n",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage does not tell of %q:\n%s", want, usage)
		}
	}
}
