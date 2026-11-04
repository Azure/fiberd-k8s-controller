package podtemplate

import (
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// argPattern is the only shape a tenant arg may take. A flag and its value
// are one item, so no tenant item can be read as the value of another.
var argPattern = regexp.MustCompile(`^-{1,2}[a-z][a-z0-9-]*(=.*)?$`)

// ownedFiberdFlags are the fiberd flags the controller sets or that only
// the controller may decide. admin-unsafe exists only in fiberd builds with
// the fiberd_testhooks tag. -template is owned whole. A grant Pod runs the
// grant's one zygote, and a tenant entry for another digest, or for the
// default key, would run a command the grant never named.
var ownedFiberdFlags = []string{
	"admin-unsafe",
	"all-caps",
	"cgroup-root",
	"client-ca",
	"endpoint-family",
	"endpoint-host",
	"engine-uid",
	"fiber-hide",
	"grants-dir",
	"gvisor-rootfs",
	"handoff-key",
	"handoff-listen",
	"http",
	"insecure-plaintext",
	"issuer",
	"jwks-max-stale",
	"listen",
	"max-lease",
	"node-id",
	"runc-rootfs",
	"runtime",
	"stale-ttl",
	"state",
	"template",
	"tls-cert",
	"tls-key",
	"verifier",
}

// binaryFlags are the flags the fiberd binary binds beside fiberd's
// Config.Bind. -home selects the home, and the controller always sets it to
// kubernetes. The Kubernetes home's flags carry the Pod's identity, which
// the downward API sets. The controller owns all of them. A test in fiberd
// core's cmd/fiberd checks that every flag the binary binds beyond
// Config.Bind is in OwnedFlags, which keeps the two lists in step.
var binaryFlags = []string{
	"home",
	"pod-name",
	"pod-namespace",
	"pod-uid",
	"version",
}

// ownedFlags is every flag a tenant arg may not name.
var ownedFlags = func() map[string]bool {
	m := make(map[string]bool, len(ownedFiberdFlags)+len(binaryFlags))
	for _, name := range slices.Concat(ownedFiberdFlags, binaryFlags) {
		m[name] = true
	}
	return m
}()

// tenantBoolFlags are the boolean fiberd flags a tenant may set. Only these
// may appear without "=value". Go's flag package reads the next item as the
// value of any other flag written bare, which would swallow the
// controller's first flag. A test keeps this list equal to what fiberd
// registers.
var tenantBoolFlags = map[string]bool{
	"gvisor-templates-on-disk": true,
}

// OwnedFlags returns, sorted, the flags a tenant may not pass in args.
// That is fiberd's owned flags and the fiberd binary's own. Each call
// returns a new slice.
func OwnedFlags() []string {
	out := make([]string, 0, len(ownedFlags))
	for name := range ownedFlags {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// CheckArgs refuses tenant args that name a flag the controller owns. Each
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
		case ownedFlags[name]:
			errs = append(errs, field.Forbidden(p, "-"+name+" is set by the controller"))
		case !hasValue && !tenantBoolFlags[name]:
			errs = append(errs, field.Invalid(p, arg, "must carry its value as -"+name+"=value"))
		}
	}
	return errs
}
