package issuer

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestExceptionHeld(t *testing.T) {
	procLabel := v1alpha1.RuntimeLabel(v1alpha1.RuntimeProc)
	runcLabel := v1alpha1.RuntimeLabel(v1alpha1.RuntimeRunc)
	both := []v1alpha1.Runtime{v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc}
	cases := []struct {
		name    string
		labels  map[string]string
		ceiling []v1alpha1.Runtime
		want    bool
	}{
		{name: "no labels", ceiling: both},
		{name: "the label with the value true", labels: map[string]string{procLabel: "true"}, ceiling: both, want: true},
		{name: "the label with the value false", labels: map[string]string{procLabel: "false"}, ceiling: both},
		{name: "the label with another spelling of true", labels: map[string]string{procLabel: "True"}, ceiling: both},
		{name: "the label with no value", labels: map[string]string{procLabel: ""}, ceiling: both},
		{name: "the runc label on proc", labels: map[string]string{runcLabel: "true"}, ceiling: both},
		{name: "a ceiling without the runtime", labels: map[string]string{procLabel: "true"}, ceiling: []v1alpha1.Runtime{v1alpha1.RuntimeRunc}},
		{name: "an empty ceiling", labels: map[string]string{procLabel: "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceptionHeld(tc.labels, tc.ceiling, v1alpha1.RuntimeProc); got != tc.want {
				t.Errorf("exceptionHeld = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHoldsException(t *testing.T) {
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
		mutate  func(*lineage)
		// fail is the type of object whose read fails.
		fail      client.Object
		want      bool
		wantErr   bool
		wantReads int
	}{
		{name: "proc with the exception", runtime: v1alpha1.RuntimeProc, want: true, wantReads: 2},
		{name: "runc with the exception", runtime: v1alpha1.RuntimeRunc, want: true, wantReads: 2},
		{
			name:      "no label",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.namespace.Labels = nil },
			wantReads: 2,
		},
		{
			name:      "a missing Namespace",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.namespace = nil },
			wantReads: 1,
		},
		{
			name:      "the label on another namespace",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.namespace.Name = "tenant-b" },
			wantReads: 1,
		},
		{
			name:      "a missing GrantPolicy",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.policy = nil },
			wantReads: 2,
		},
		{
			name:      "a GrantPolicy of another name",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.policy.Name = "other" },
			wantReads: 2,
		},
		{
			name:      "a ceiling without the runtime",
			runtime:   v1alpha1.RuntimeProc,
			mutate:    func(l *lineage) { l.policy.Spec.Runtimes = []v1alpha1.Runtime{v1alpha1.RuntimeHyperlight} },
			wantReads: 2,
		},
		{name: "a read of the Namespace that fails", runtime: v1alpha1.RuntimeProc, fail: &corev1.Namespace{}, wantErr: true, wantReads: 1},
		{name: "a read of the GrantPolicy that fails", runtime: v1alpha1.RuntimeProc, fail: &v1alpha1.GrantPolicy{}, wantErr: true, wantReads: 2},
		{name: "gVisor reads nothing", runtime: v1alpha1.RuntimeGvisor},
		{name: "Hyperlight reads nothing", runtime: v1alpha1.RuntimeHyperlight},
		{name: "gVisor reads nothing while the reads would fail", runtime: v1alpha1.RuntimeGvisor, fail: &corev1.Namespace{}},
		{name: "an unset runtime reads nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLineage(t, func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = tc.runtime })
			if tc.mutate != nil {
				tc.mutate(l)
			}
			reads := 0
			h := &Handler{live: newReader(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
					reads++
					if tc.fail != nil && reflect.TypeOf(o) == reflect.TypeOf(tc.fail) {
						return errAPIServer
					}
					return c.Get(ctx, key, o, opts...)
				},
			}, l.objects())}

			got, err := h.holdsException(t.Context(), l.grant)

			if got != tc.want {
				t.Errorf("holdsException = %v, want %v", got, tc.want)
			}
			if reads != tc.wantReads {
				t.Errorf("holdsException read %d objects, want %d", reads, tc.wantReads)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("holdsException: %v", err)
				}
				return
			}
			var rf *refusal
			if !errors.As(err, &rf) || rf.status != http.StatusServiceUnavailable || rf.reason != reasonUnavailable {
				t.Errorf("holdsException error = %v, want the refusal of an unavailable API server", err)
			}
		})
	}
}
