package podtemplate

import (
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ArgPattern is the only shape a tenant arg may take. A flag and its value
// are one item, so no tenant item can be read as the value of another. The
// admission policy in config/policy repeats it, and a test keeps the two
// equal.
const ArgPattern = `^-{1,2}[a-z][a-z0-9-]*(=.*)?$`

var argPattern = regexp.MustCompile(ArgPattern)

// tenantFlags are the fiberd flags a tenant may pass in args. Every other
// flag fiberd binds is the controller's, such as -issuer and -template, or
// the operator's, such as -userns-pool and -audit-key, and a flag that
// does not exist yet is nobody's until it is added here. The admission
// policy in config/policy repeats the list, and a test keeps the two
// equal and every entry a flag fiberd registers.
var tenantFlags = map[string]bool{
	// The repository the grant's zygote artifacts are pulled from.
	"registry": true,
	// Where the grant's parked sessions are published and claimed.
	"delta-registry": true,
	// The key that signs the grant's deltas, shared by the grant's Pods so
	// one can claim what another parked.
	"delta-key": true,
	// The key that seals the grant's deltas and opens its sealed templates.
	"delta-seal-key": true,
	// Further keys whose deltas the grant's home imports.
	"delta-trust": true,
	// The devices the grant's engine may drive, which the container's
	// resources put in the Pod.
	"devices": true,
	// The device engine is the tenant's own program, which fiberd runs as
	// the engine uid with the engine capability set.
	"engine": true,
	// The engine's memory, carved from the container's limit.
	"engine-memory-max": true,
	// The block ceiling, within the container's memory limit.
	"grant-ceiling": true,
	// Trades RAM for disk on the tenant's own share of the node.
	"gvisor-templates-on-disk": true,
	// The guest the tenant's helper loads.
	"hyperlight-guest": true,
	// How strictly the grant's own parks must match the host.
	"parity": true,
}

// tenantBoolFlags are the boolean flags among tenantFlags. Only these may
// appear without "=value". Go's flag package reads the next item as the
// value of any other flag written bare, which would swallow the
// controller's first flag. A test keeps this list equal to what fiberd
// registers.
var tenantBoolFlags = map[string]bool{
	"gvisor-templates-on-disk": true,
}

// TenantFlags returns, sorted, the flags a tenant may pass in args. Each
// call returns a new slice.
func TenantFlags() []string { return sortedNames(tenantFlags) }

// TenantBoolFlags returns, sorted, the tenant flags that may be written
// bare. Each call returns a new slice.
func TenantBoolFlags() []string { return sortedNames(tenantBoolFlags) }

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// CheckArgs refuses tenant args that name a flag outside TenantFlags. Each
// arg must be one flag in the one- or two-dash form with its value after
// "=", such as -registry=example.com/zygotes. Only a boolean flag may omit
// the value.
func CheckArgs(args []string) error {
	return checkArgs(field.NewPath("args"), args).ToAggregate()
}

// checkArgs is CheckArgs with errors rooted at fld.
func checkArgs(fld *field.Path, args []string) field.ErrorList {
	var errs field.ErrorList
	for i, arg := range args {
		p := fld.Index(i)
		if !argPattern.MatchString(arg) {
			errs = append(errs, field.Invalid(p, arg, "must be one flag such as -name=value, matching "+argPattern.String()))
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch {
		case !tenantFlags[name]:
			errs = append(errs, field.Forbidden(p, "-"+name+" is not a flag a tenant may set"))
		case !hasValue && !tenantBoolFlags[name]:
			errs = append(errs, field.Invalid(p, arg, "must carry its value as -"+name+"=value"))
		}
	}
	return errs
}
