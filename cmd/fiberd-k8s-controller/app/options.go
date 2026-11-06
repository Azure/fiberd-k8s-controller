// Package app builds the fiberd-k8s-controller manager from its
// command-line options. It holds the flags, their defaults and the checks
// that run before the first API call.
package app

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/content"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/Azure/fiberd/pkg/endpoint"

	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// Flag defaults that podtemplate leaves to its caller.
const (
	// defaultLease is fiberd's own -jwks-max-stale default. The builder sets
	// -jwks-max-stale to the lease, so a grant Pod keeps the standalone
	// default.
	defaultLease = time.Hour

	// defaultProbeAddr is where /healthz and /readyz listen.
	defaultProbeAddr = ":8081"

	// defaultIssuerAddr is where discovery and the key set are served over
	// HTTPS.
	defaultIssuerAddr = ":8443"

	// defaultRatePerIP and defaultRate are requests per second. A Pod asks
	// once per half lease, so the global limit carries far more Pods than
	// one cluster runs and only a first start of many Pods comes near it.
	defaultRatePerIP = 50
	defaultRate      = 100

	// defaultPodInterval is the time one Pod earns one more grant in. It is
	// long against a renewal and short against a crash loop.
	defaultPodInterval = 10 * time.Second

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

	// IssuerAddr is the host and port that serve discovery and the key set
	// over HTTPS.
	IssuerAddr string

	// TLSCertFile is the PEM certificate the issuer listener serves. It is
	// reread when it changes, so a renewed certificate needs no restart.
	TLSCertFile string

	// TLSKeyFile is the PEM private key of TLSCertFile.
	TLSKeyFile string

	// ClientCAFile is the PEM fiberd CA that a caller's client certificate
	// must chain to. It is read once at start, since the CA does not rotate.
	ClientCAFile string

	// Limits bounds how often the grant route answers.
	Limits issuer.RateLimits
}

// NewOptions returns every option at its default. The namespace comes from
// $POD_NAMESPACE as it reads now. The issuer URL, the serving certificate
// and key files and the client CA file have no default.
func NewOptions() *Options {
	return &Options{
		Template: podtemplate.Options{
			Lease:          defaultLease,
			StaleTTL:       podtemplate.DefaultStaleTTL,
			EndpointFamily: podtemplate.DefaultEndpointFamily,
			DefaultCPU:     resource.MustParse(podtemplate.DefaultPodCPU),
			ServiceAccount: podtemplate.DefaultServiceAccount,
		},
		Namespace:   os.Getenv(envPodNamespace),
		LeaderElect: true,
		ProbeAddr:   defaultProbeAddr,
		IssuerAddr:  defaultIssuerAddr,
		Limits: issuer.RateLimits{
			PerIP:       defaultRatePerIP,
			Global:      defaultRate,
			PodInterval: defaultPodInterval,
		},
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
		"A grant Pod whose template sets no CPU gets this CPU request and no CPU limit.")
	fs.StringVar(&o.Template.ServiceAccount, "grant-service-account", o.Template.ServiceAccount,
		"Every grant Pod runs as this service account.")
	fs.StringVar(&o.Namespace, "namespace", o.Namespace,
		"The controller runs in this namespace and holds its leader-election Lease there. It defaults to $"+envPodNamespace+".")
	fs.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect,
		"Elect one leader among the replicas to run the reconciler.")
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", o.ProbeAddr,
		"/healthz and /readyz listen on this host and port.")
	fs.StringVar(&o.IssuerAddr, "issuer-bind-address", o.IssuerAddr,
		"fiberd fetches OIDC discovery, the JSON Web Key Set and its grant from the HTTPS listener on this host and port.")
	fs.StringVar(&o.TLSCertFile, "tls-cert-file", o.TLSCertFile,
		"The issuer listener serves this PEM certificate for OIDC discovery and the JSON Web Key Set. It is required.")
	fs.StringVar(&o.TLSKeyFile, "tls-private-key-file", o.TLSKeyFile,
		"The PEM private key of --tls-cert-file. It is required.")
	fs.StringVar(&o.ClientCAFile, "client-ca-file", o.ClientCAFile,
		"A caller fetching its grants presents a client certificate that chains to this PEM CA bundle, the fiberd CA. It is read once at start and is required.")
	fs.Float64Var(&o.Limits.PerIP, "grant-rate-per-ip", o.Limits.PerIP,
		"Each replica answers one client address this many grant requests per second.")
	fs.Float64Var(&o.Limits.Global, "grant-rate", o.Limits.Global,
		"Each replica answers this many grant requests per second from all clients together.")
	fs.DurationVar(&o.Limits.PodInterval, "grant-pod-interval", o.Limits.PodInterval,
		"Each replica lets one Pod, and one caller, earn one more grant request in this time, after a small burst.")
}

// Validate reports every problem in o before any API call. It does not
// read the certificate, key and CA files, since NewManager reports a missing or
// bad one with the real error.
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
	if err := validBindAddr("--health-probe-bind-address", o.ProbeAddr); err != nil {
		errs = append(errs, err)
	}
	if err := validBindAddr("--issuer-bind-address", o.IssuerAddr); err != nil {
		errs = append(errs, err)
	}
	if o.TLSCertFile == "" {
		errs = append(errs, errors.New("--tls-cert-file is required"))
	}
	if o.TLSKeyFile == "" {
		errs = append(errs, errors.New("--tls-private-key-file is required"))
	}
	if o.ClientCAFile == "" {
		errs = append(errs, errors.New("--client-ca-file is required"))
	}
	if err := validRate("--grant-rate-per-ip", o.Limits.PerIP); err != nil {
		errs = append(errs, err)
	}
	if err := validRate("--grant-rate", o.Limits.Global); err != nil {
		errs = append(errs, err)
	}
	if o.Limits.PodInterval <= 0 {
		errs = append(errs, fmt.Errorf("--grant-pod-interval must be positive, got %s", o.Limits.PodInterval))
	}
	return utilerrors.NewAggregate(errs)
}

// validRate accepts a positive, finite number of requests per second. The
// flag package parses NaN and Inf, and a limit of either bounds nothing.
func validRate(name string, perSecond float64) error {
	if !(perSecond > 0) || math.IsInf(perSecond, 1) {
		return fmt.Errorf("%s must be a positive number, got %v", name, perSecond)
	}
	return nil
}

// validBindAddr accepts a host and port whose port is a number from 0 to
// 65535. An empty host listens on every address. name is the flag addr
// came from.
func validBindAddr(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%s %q: port %q is not a number from 0 to 65535", name, addr, port)
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
