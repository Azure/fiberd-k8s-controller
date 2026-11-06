package podtemplate

import (
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd/pkg/runtime/host"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// The seal key volume holds the Secret spec.zygote.sealKeySecretName names.
// It sits outside mountRoots, so no tenant mount can replace it or hide it.
const (
	sealKeyVolume = "fiberd-seal-key"
	sealKeyDir    = "/var/run/secrets/fiberd.io/seal-key"
	sealKeyFile   = sealKeyDir + "/" + v1alpha1.SealKeySecretKey
)

var registryPattern = regexp.MustCompile(v1alpha1.RegistryPattern)

// TemplateFlag renders fiberd's -template value from the zygote spec with
// host.FormatTemplateFlag, so fiberd's own rules decide what a digest and
// a command may hold. A refusal is reported as a field error on
// spec.zygote that carries fiberd's reason.
func TemplateFlag(z v1alpha1.ZygoteSpec) (string, error) {
	v, errs := templateFlag(field.NewPath("spec", "zygote"), z)
	if len(errs) > 0 {
		return "", errs.ToAggregate()
	}
	return v, nil
}

// templateFlag is TemplateFlag with its error rooted at fld.
func templateFlag(fld *field.Path, z v1alpha1.ZygoteSpec) (string, field.ErrorList) {
	v, err := host.FormatTemplateFlag(z.Digest, z.Command)
	if err != nil {
		return "", field.ErrorList{field.Invalid(fld, z, err.Error())}
	}
	return v, nil
}

// checkRegistryRuntime refuses a registry zygote on hyperlight, as the CRD
// rule does. That backend hands its helper only the template's arguments, so
// the pulled executable would never run.
func checkRegistryRuntime(fld *field.Path, rt v1alpha1.Runtime, registry string) field.ErrorList {
	if registry != "" && rt == v1alpha1.RuntimeHyperlight {
		return field.ErrorList{field.Forbidden(fld, "not allowed for runtime hyperlight, which does not run a pulled zygote yet")}
	}
	return nil
}

// zygoteArgs are the fiberd flags that say where the zygote comes from. A
// command becomes a -template mapping. A registry becomes -registry and no
// -template, since fiberd looks a digest up in its -template map first and
// pulls from the registry only when the map lacks it. The checks repeat
// the CRD's rules, so a grant written past them cannot reach the Pod.
func zygoteArgs(fld *field.Path, z v1alpha1.ZygoteSpec) ([]string, field.ErrorList) {
	var args []string
	switch {
	case len(z.Command) > 0 && z.Registry != "":
		return nil, field.ErrorList{field.Invalid(fld, z, "exactly one of zygote.command and zygote.registry must be set")}
	case z.Registry == "":
		v, errs := templateFlag(fld, z)
		if len(errs) > 0 {
			return nil, errs
		}
		args = []string{"-template=" + escapeExpansion(v)}
	case !registryPattern.MatchString(z.Registry):
		return nil, field.ErrorList{field.Invalid(fld.Child("registry"), z.Registry, "must match "+v1alpha1.RegistryPattern)}
	default:
		args = []string{"-registry=" + escapeExpansion(z.Registry)}
	}
	if z.SealKeySecretName != "" {
		// proc fibers run as fiberd's uid, so only hiding the directory
		// keeps the tenant's key from them.
		args = append(args, "-delta-seal-key="+sealKeyFile, "-fiber-hide="+sealKeyDir)
	}
	return args, nil
}
