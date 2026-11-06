package workload

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCacheThenLiveGet(t *testing.T) {
	const uid = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"
	errRead := errors.New("connection refused")
	pod := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", UID: uid}}
	}
	failing := func() client.Reader {
		return fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errRead
			},
		}).Build()
	}
	holding := func() client.Reader { return fake.NewClientBuilder().WithObjects(pod()).Build() }
	empty := func() client.Reader { return fake.NewClientBuilder().Build() }

	cases := []struct {
		name    string
		cache   client.Reader
		live    client.Reader
		wantErr error
		wantHit bool
	}{
		{name: "a cache hit never asks the API server", cache: holding(), live: failing(), wantHit: true},
		{name: "a cache miss reads live", cache: empty(), live: holding(), wantHit: true},
		{name: "a failed cache read does not fall through", cache: failing(), live: holding(), wantErr: errRead},
		{name: "a failed live read comes back", cache: empty(), live: failing(), wantErr: errRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := &corev1.Pod{}
			err := CacheThenLive(tc.cache, tc.live).Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, got)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Get error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantHit && got.UID != uid {
				t.Fatalf("Get read UID %q, want %q", got.UID, uid)
			}
		})
	}
}
