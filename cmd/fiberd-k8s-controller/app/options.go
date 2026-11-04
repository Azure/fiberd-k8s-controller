// Package app builds the fiberd-k8s-controller manager from its
// command-line options. It holds the flags, their defaults and the checks
// that run before the first API call.
package app

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/content"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/Azure/fiberd/pkg/endpoint"

	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// Flag defaults that podtemplate leaves to its caller.
const (
	// defaultLease is fiberd's own -jwks-max-stale default. The builder sets
	// -jwks-max-stale to the lease, so a grant Pod keeps the standalone
	// default.
	defaultLease = time.Hour

	// defaultPodCPU is the CPU limit of a grant Pod whose template sets
	// none.
	defaultPodCPU = "500m"

	// defaultProbeAddr is where /healthz and /readyz listen.
	defaultProbeAddr = ":8081"

	// envPodNamespace names the downward API variable that gives the
	// controller its own namespace.
	envPodNamespace = "POD_NAMESPACE"
)

// Options is everything the command line sets.
type Options struct {
	// Template is what the controller decides for every grant Pod.
	Template podtemplate.Options

	// Namespace is the controller's own namespace. The leader-election
	// Lease lives there.
	Namespace string

	// LeaderElect makes the replicas elect one leader to run the
	// reconciler.
	LeaderElect bool

	// ProbeAddr is the host and port that serve /healthz and /readyz.
	ProbeAddr string
}

// NewOptions returns every option at its default. The namespace comes from
// $POD_NAMESPACE as it reads now. The issuer URL has no default.
func NewOptions() *Options {
	return &Options{
		Template: podtemplate.Options{
			Lease:          defaultLease,
			StaleTTL:       podtemplate.DefaultStaleTTL,
			EndpointFamily: podtemplate.DefaultEndpointFamily,
			DefaultCPU:     resource.MustParse(defaultPodCPU),
			Plaintext:      true,
			ServiceAccount: podtemplate.DefaultServiceAccount,
		},
		Namespace:   os.Getenv(envPodNamespace),
		LeaderElect: true,
		ProbeAddr:   defaultProbeAddr,
	}
}

// AddFlags registers every option on fs. Each flag starts at the value o
// holds, so NewOptions gives the shipped defaults.
func (o *Options) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.Template.IssuerURL, "issuer-url", o.Template.IssuerURL,
		"fiberd fetches OIDC discovery and the JSON Web Key Set from this URL. It is required.")
	fs.DurationVar(&o.Template.Lease, "lease", o.Template.Lease,
		"A signed grant stays valid this long, as a duration such as 1h or 90s. It must be a whole number of seconds. fiberd's -jwks-max-stale is set to it.")
	fs.DurationVar(&o.Template.StaleTTL, "stale-ttl", o.Template.StaleTTL,
		"Every grant Pod runs fiberd with this -stale-ttl.")
	fs.Var((*familyValue)(&o.Template.EndpointFamily), "endpoint-family",
		"Every grant Pod runs fiberd with this -endpoint-family. It is one of unix, inet4 or inet6.")
	fs.Var((*quantityValue)(&o.Template.DefaultCPU), "default-pod-cpu",
		"A grant Pod whose template sets no CPU gets this CPU limit.")
	fs.BoolVar(&o.Template.Plaintext, "insecure-plaintext", o.Template.Plaintext,
		"Serve fiberd's Fibers API without TLS. It is on by default, since grant Pods do not serve mutual TLS yet.")
	fs.StringVar(&o.Template.ServiceAccount, "grant-service-account", o.Template.ServiceAccount,
		"Every grant Pod runs as this service account.")
	fs.StringVar(&o.Namespace, "namespace", o.Namespace,
		"The controller runs in this namespace and holds its leader-election Lease there. It defaults to $"+envPodNamespace+".")
	fs.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect,
		"Elect one leader among the replicas to run the reconciler.")
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", o.ProbeAddr,
		"/healthz and /readyz listen on this host and port.")
}

// Validate reports every problem in o before any API call. It runs the
// grant Pod template checks, requires a valid namespace and parses the
// probe address.
func (o *Options) Validate() error {
	var errs []error
	if err := o.Template.Validate(); err != nil {
		errs = append(errs, err)
	}
	if o.Namespace == "" {
		errs = append(errs, fmt.Errorf("--namespace is required, set it or $%s", envPodNamespace))
	} else if msgs := content.IsDNS1123Label(o.Namespace); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("--namespace %q is not a valid namespace name: %s", o.Namespace, strings.Join(msgs, ", ")))
	}
	if err := validProbeAddr(o.ProbeAddr); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.NewAggregate(errs)
}

// validProbeAddr accepts a host and port whose port is a number from 0 to
// 65535. An empty host listens on every address.
func validProbeAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--health-probe-bind-address %q: %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("--health-probe-bind-address %q: port %q is not a number from 0 to 65535", addr, port)
	}
	return nil
}

// familyValue reads --endpoint-family with fiberd's own parser.
type familyValue endpoint.Family

// String returns the family as fiberd spells it.
func (f *familyValue) String() string { return string(*f) }

// Set parses s as fiberd parses its -endpoint-family flag. It refuses an
// empty or blank value, which fiberd's parser would read as unix.
func (f *familyValue) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("the endpoint family is empty, want unix, inet4 or inet6")
	}
	v, err := endpoint.ParseFamily(s)
	if err != nil {
		return err
	}
	*f = familyValue(v)
	return nil
}

// quantityValue reads a resource quantity such as 500m or 2.
type quantityValue resource.Quantity

// String returns the quantity in its canonical form.
func (q *quantityValue) String() string { return (*resource.Quantity)(q).String() }

// Set parses s as a resource quantity. It refuses a zero or negative
// quantity, since a grant Pod needs some CPU.
func (q *quantityValue) Set(s string) error {
	v, err := resource.ParseQuantity(s)
	if err != nil {
		return err
	}
	if v.Sign() <= 0 {
		return fmt.Errorf("%s is not a positive quantity", s)
	}
	*q = quantityValue(v)
	return nil
}
