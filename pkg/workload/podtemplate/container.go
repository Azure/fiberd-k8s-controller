package podtemplate

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

const (
	// grantFetchTimeout is how long fiberd retries its first grant before it
	// refuses to start. It is fiberd's own default, set here because the
	// startup probe has to outlast it.
	grantFetchTimeout = 60 * time.Second

	// startupGrace is what the startup probe allows beyond the first fetch
	// for fiberd to open its port.
	startupGrace = 30 * time.Second

	// startupPeriodSeconds is how often the startup probe dials.
	startupPeriodSeconds = 2
)

// controllerArgs are the fiberd flags the controller sets. Build appends
// them after the tenant's args. checkArgs refuses every one of them in the
// tenant's args, -home and the zygote's -template or -registry included. -home=kubernetes comes
// first and selects the Kubernetes home inside fiberd.
//
// The grant fetch sets no -grant-ca-file. It trusts the same root pool as
// the key set fetch, which SSL_CERT_FILE points at the fiberd CA, so the two
// cannot disagree.
//
// The kubelet expands $(VAR) in args. Values that come from the grant or
// the options are escaped so fiberd sees them as written. -node-id is the
// one value meant to expand.
func controllerArgs(rt v1alpha1.Runtime, zygoteArgs []string, opts Options) []string {
	args := []string{
		"-home=" + fiberdHome,
		"-verifier=jwks",
		"-issuer=" + escapeExpansion(opts.IssuerURL),
		"-jwks-max-stale=" + opts.Lease.String(),
		"-stale-ttl=" + opts.StaleTTL.String(),
		"-listen=:" + strconv.Itoa(int(PortGRPC)),
		"-runtime=" + string(rt),
		"-node-id=$(" + envPodName + ")",
		"-state=" + stateDir,
		"-grants-dir=" + grantsDir,
		"-grant-token-file=" + grantTokenFile,
		"-grant-fetch-timeout=" + grantFetchTimeout.String(),
		"-endpoint-family=" + string(opts.EndpointFamily),
	}
	args = append(args, zygoteArgs...)
	switch rt {
	case v1alpha1.RuntimeRunc:
		args = append(args, "-runc-rootfs="+rootfsDir)
	case v1alpha1.RuntimeGvisor:
		args = append(args, "-gvisor-rootfs="+rootfsDir)
	case v1alpha1.RuntimeHyperlight:
		args = append(args, "-hyperlight-helper="+helperFile)
	}
	if opts.Plaintext {
		args = append(args, "-insecure-plaintext")
	}
	return args
}

// escapeExpansion doubles every "$", which the kubelet turns back into one
// "$" instead of expanding a $(VAR) reference.
func escapeExpansion(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// checkProbes refuses tenant probes on the fiberd container. The overlay
// owns all three. A tenant probe could keep a broken fiberd Ready, or
// restart a healthy one.
func checkProbes(fld *field.Path, c *corev1.Container) field.ErrorList {
	var errs field.ErrorList
	for _, p := range []struct {
		name  string
		probe *corev1.Probe
	}{
		{"startupProbe", c.StartupProbe},
		{"readinessProbe", c.ReadinessProbe},
		{"livenessProbe", c.LivenessProbe},
	} {
		if p.probe != nil {
			errs = append(errs, field.Forbidden(fld.Child(p.name), "the controller sets fiberd's probes"))
		}
	}
	return errs
}

// probeTimeoutSeconds is the timeout of the exec probes. fiberd healthz
// and fiberd readyz give up after five seconds, so the kubelet must wait
// longer than that to read their exit code instead of its own timeout.
const probeTimeoutSeconds = 10

// setProbes sets fiberd's three probes. checkProbes has refused any
// tenant probe.
//
// fiberd opens its port only once it holds a grant, so the startup probe
// outlasts the first fetch, or the kubelet would restart a Pod that is
// waiting on the controller. Readiness runs fiberd readyz, which fails
// while fiberd drains, so a draining Pod leaves its Service. The zygote-ready
// gate still says the template is warm. Liveness runs fiberd healthz. Both
// exec probes name the same state directory as fiberd's -state flag, because
// the admin socket lives there.
func setProbes(c *corev1.Container) {
	c.StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(PortGRPC)}},
		PeriodSeconds:    startupPeriodSeconds,
		FailureThreshold: int32((grantFetchTimeout + startupGrace) / (startupPeriodSeconds * time.Second)),
	}
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: fiberdProbe("readyz")}},
		TimeoutSeconds:   probeTimeoutSeconds,
		PeriodSeconds:    5,
		FailureThreshold: 2,
	}
	c.LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: fiberdProbe("healthz")}},
		TimeoutSeconds:   probeTimeoutSeconds,
		PeriodSeconds:    15,
		FailureThreshold: 3,
	}
}

// fiberdProbe is the command line of fiberd's healthz or readyz
// subcommand.
func fiberdProbe(sub string) []string {
	return []string{fiberdBinary, sub, "-state=" + stateDir}
}
