package podtemplate

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/content"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/Azure/fiberd/pkg/endpoint"
)

// Defaults Options.withDefaults fills in. The stale TTL is the
// controller's own default for the -stale-ttl flag it always passes, so
// the home's standalone default never applies to a grant Pod.
const (
	DefaultStaleTTL       = 30 * time.Second
	DefaultEndpointFamily = endpoint.Inet4
	DefaultServiceAccount = "fiberd-grant"

	// DefaultPodCPU is the CPU limit a grant Pod gets when its template
	// sets none.
	DefaultPodCPU = "500m"
)

// defaultCPU is DefaultPodCPU as a quantity.
var defaultCPU = resource.MustParse(DefaultPodCPU)

// Options is what the controller decides for every grant Pod. The tenant
// sets none of it.
type Options struct {
	// IssuerURL is fiberd's -issuer. fiberd also fetches the JSON Web Key
	// Set from it, since the builder always sets -verifier=jwks.
	IssuerURL string

	// Lease is how long a grant the controller signs stays valid. fiberd's
	// -jwks-max-stale is set to it, so a home stops verifying once its key
	// set is older than one lease. It is a whole number of seconds, since
	// the grant token's exp claim holds whole seconds and the issuer
	// refuses any other lease.
	Lease time.Duration

	// StaleTTL is fiberd's -stale-ttl. The default is DefaultStaleTTL.
	StaleTTL time.Duration

	// EndpointFamily is fiberd's -endpoint-family. The default is inet4.
	EndpointFamily endpoint.Family

	// DefaultCPU is the CPU limit used when the template sets none. The
	// default is 500m.
	DefaultCPU resource.Quantity

	// ServiceAccount is the grant Pod's service account. The default is
	// fiberd-grant.
	ServiceAccount string
}

// withDefaults returns o with every unset field given its default.
func (o Options) withDefaults() Options {
	if o.StaleTTL == 0 {
		o.StaleTTL = DefaultStaleTTL
	}
	if o.EndpointFamily == "" {
		o.EndpointFamily = DefaultEndpointFamily
	}
	if o.DefaultCPU.IsZero() {
		o.DefaultCPU = defaultCPU.DeepCopy()
	}
	if o.ServiceAccount == "" {
		o.ServiceAccount = DefaultServiceAccount
	}
	return o
}

// Validate reports every problem that keeps a grant Pod from being built
// with o. These are mistakes in the controller's own configuration, not in
// a grant, so a controller calls Validate once at startup. Unset fields are
// checked as the default Build would give them. Build calls Validate too.
func (o Options) Validate() error {
	o = o.withDefaults()
	var errs []error
	if err := validIssuer(o.IssuerURL); err != nil {
		errs = append(errs, err)
	}
	switch {
	case o.Lease <= 0:
		errs = append(errs, fmt.Errorf("lease must be positive, got %s", o.Lease))
	case o.Lease%time.Second != 0:
		errs = append(errs, fmt.Errorf("lease must be a whole number of seconds, got %s", o.Lease))
	}
	if o.StaleTTL <= 0 {
		errs = append(errs, fmt.Errorf("stale TTL must be positive, got %s", o.StaleTTL))
	}
	switch o.EndpointFamily {
	case endpoint.Unix, endpoint.Inet4, endpoint.Inet6:
	default:
		errs = append(errs, fmt.Errorf("endpoint family %q is not unix, inet4 or inet6", o.EndpointFamily))
	}
	if o.DefaultCPU.Sign() <= 0 {
		errs = append(errs, fmt.Errorf("default CPU must be positive, got %s", o.DefaultCPU.String()))
	}
	if msgs := content.IsDNS1123Subdomain(o.ServiceAccount); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("service account %q is not a valid name: %s", o.ServiceAccount, strings.Join(msgs, "; ")))
	}
	return utilerrors.NewAggregate(errs)
}

// validIssuer accepts an absolute https URL with a host and nothing after
// it. fiberd fetches OIDC discovery from it, the controller serves it over
// TLS only and core mounts discovery at the URL's root, so anything else
// fails at runtime.
func validIssuer(s string) error {
	if s == "" {
		return errors.New("issuer URL is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("issuer URL %q: %w", s, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("issuer URL %q must be an absolute https URL, since the controller serves it over TLS only", s)
	}
	if strings.ContainsFunc(s, unicode.IsSpace) {
		return fmt.Errorf("issuer URL %q must not contain whitespace", s)
	}
	if u.Path != "" || strings.ContainsAny(s, "?#") {
		return fmt.Errorf("issuer URL %q must have no path, query or fragment, since discovery is served at its root", s)
	}
	return nil
}
