package podtemplate

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd/pkg/runtime/host"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

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
