package issuer_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
)

const keyNamespace = "fiberd-system"

var (
	// keySecretRef is how every error about the key Secret names it.
	keySecretRef = keyNamespace + "/" + issuer.KeySecretName
	// keySecretKey is the key Secret's object key.
	keySecretKey = client.ObjectKey{Namespace: keyNamespace, Name: issuer.KeySecretName}
	secretsGR    = corev1.Resource("secrets")
)

// encodedRing is a fresh ring of one key for alg, as the Secret holds it.
func encodedRing(t *testing.T, alg jose.SignatureAlgorithm) (grant.KeyRing, []byte) {
	t.Helper()
	ring, err := grant.NewKeyRing(alg)
	if err != nil {
		t.Fatalf("NewKeyRing(%s): %v", alg, err)
	}
	raw, err := ring.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return ring, raw
}

// keySecret is the key Secret with data as its whole data map.
func keySecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      issuer.KeySecretName,
			Namespace: keyNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "fiberd-k8s-controller"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func kids(r grant.KeyRing) []string {
	out := make([]string, 0, len(r))
	for _, k := range r {
		out = append(out, k.JWK.KeyID)
	}
	return out
}

// countingClient is a fake client, holding secret when it is set, whose
// Get and Create are counted and may be replaced.
func countingClient(
	secret *corev1.Secret,
	get func(context.Context, client.WithWatch, client.ObjectKey, client.Object) error,
	create func(context.Context, client.WithWatch, client.Object) error,
) (base client.WithWatch, c client.Client, gets, creates *int) {
	builder := fake.NewClientBuilder()
	if secret != nil {
		builder = builder.WithObjects(secret.DeepCopy())
	}
	base = builder.Build()
	gets, creates = new(int), new(int)
	c = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			*gets++
			if get != nil {
				return get(ctx, cl, key, obj)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*creates++
			if create != nil {
				return create(ctx, cl, obj)
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	return base, c, gets, creates
}

func TestSecretKeyStoreLoad(t *testing.T) {
	_, raw := encodedRing(t, jose.EdDSA)
	unavailable := apierrors.NewServiceUnavailable("apiserver down")

	cases := []struct {
		name string
		// secret is the key Secret that exists before the call, if any.
		secret *corev1.Secret
		// get replaces the fake client's Get when set.
		get       func(context.Context, client.WithWatch, client.ObjectKey, client.Object) error
		namespace string
		want      []byte
		// wantErrIs, when set, must be wrapped in the error.
		wantErrIs error
		// wantErr is a substring of the error. Empty means success.
		wantErr string
	}{
		{
			name:      "present",
			secret:    keySecret(map[string][]byte{issuer.KeySecretField: raw}),
			namespace: keyNamespace,
			want:      raw,
		},
		{
			name:      "absent Secret",
			namespace: keyNamespace,
			wantErrIs: grant.ErrNoKeyRing,
			wantErr:   keySecretRef,
		},
		{
			name:      "absent field",
			secret:    keySecret(map[string][]byte{"other": []byte("x")}),
			namespace: keyNamespace,
			wantErrIs: grant.ErrNoKeyRing,
			wantErr:   keySecretRef,
		},
		{
			name:      "empty field",
			secret:    keySecret(map[string][]byte{issuer.KeySecretField: {}}),
			namespace: keyNamespace,
			wantErrIs: grant.ErrNoKeyRing,
			wantErr:   keySecretRef,
		},
		{
			name: "get fails with an error other than NotFound",
			get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object) error {
				return unavailable
			},
			namespace: keyNamespace,
			wantErrIs: unavailable,
			wantErr:   keySecretRef,
		},
		{
			name:    "empty namespace",
			wantErr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c, gets, _ := countingClient(tc.secret, tc.get, nil)
			store := &issuer.SecretKeyStore{Client: c, Namespace: tc.namespace}

			got, err := store.Load(t.Context())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load = %q, want an error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Load error %q does not contain %q", err, tc.wantErr)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Errorf("Load error %q does not wrap %q", err, tc.wantErrIs)
				}
				// Only a missing Secret or field means nothing is stored. Any
				// other failure must not make LoadOrCreate create a Secret.
				if errors.Is(err, grant.ErrNoKeyRing) != errors.Is(tc.wantErrIs, grant.ErrNoKeyRing) {
					t.Errorf("Load error %q: errors.Is(ErrNoKeyRing) = %t, want %t",
						err, errors.Is(err, grant.ErrNoKeyRing), errors.Is(tc.wantErrIs, grant.ErrNoKeyRing))
				}
				if tc.namespace == "" && *gets != 0 {
					t.Errorf("Load called Get %d times without a namespace, want 0", *gets)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("Load = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecretKeyStoreCreate(t *testing.T) {
	_, raw := encodedRing(t, jose.EdDSA)
	_, other := encodedRing(t, jose.ES256)
	forbidden := apierrors.NewForbidden(secretsGR, issuer.KeySecretName, errors.New("denied"))

	cases := []struct {
		name string
		// secret is the key Secret that exists before the call, if any.
		secret *corev1.Secret
		// create replaces the fake client's Create when set.
		create    func(context.Context, client.WithWatch, client.Object) error
		namespace string
		// wantData is the keys.json the Secret holds after the call. Nil
		// means no Secret exists after the call.
		wantData []byte
		// wantErrIs, when set, must be wrapped in the error.
		wantErrIs error
		// wantErr is a substring of the error. Empty means success.
		wantErr string
	}{
		{
			name:      "creates the Secret",
			namespace: keyNamespace,
			wantData:  raw,
		},
		{
			// The fake client answers AlreadyExists for the Secret it holds,
			// as the API server does when another replica created it first.
			name:      "already exists",
			secret:    keySecret(map[string][]byte{issuer.KeySecretField: other}),
			namespace: keyNamespace,
			wantData:  other,
			wantErrIs: grant.ErrKeyRingExists,
			wantErr:   keySecretRef,
		},
		{
			name: "create fails with an error other than AlreadyExists",
			create: func(context.Context, client.WithWatch, client.Object) error {
				return forbidden
			},
			namespace: keyNamespace,
			wantErrIs: forbidden,
			wantErr:   keySecretRef,
		},
		{
			name:    "empty namespace",
			wantErr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, c, _, creates := countingClient(tc.secret, nil, tc.create)
			store := &issuer.SecretKeyStore{Client: c, Namespace: tc.namespace}

			err := store.Create(t.Context(), raw)
			switch {
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("Create succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Create error %q does not contain %q", err, tc.wantErr)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Errorf("Create error %q does not wrap %q", err, tc.wantErrIs)
				}
				// Only AlreadyExists means another replica stored a ring. Any
				// other failure must not send LoadOrCreate back to load.
				if errors.Is(err, grant.ErrKeyRingExists) != errors.Is(tc.wantErrIs, grant.ErrKeyRingExists) {
					t.Errorf("Create error %q: errors.Is(ErrKeyRingExists) = %t, want %t",
						err, errors.Is(err, grant.ErrKeyRingExists), errors.Is(tc.wantErrIs, grant.ErrKeyRingExists))
				}
				if tc.namespace == "" && *creates != 0 {
					t.Errorf("Create called the client %d times without a namespace, want 0", *creates)
				}
			case err != nil:
				t.Fatalf("Create: %v", err)
			}
			if tc.namespace == "" {
				return
			}

			var stored corev1.Secret
			gerr := base.Get(t.Context(), keySecretKey, &stored)
			if tc.wantData == nil {
				if !apierrors.IsNotFound(gerr) {
					t.Errorf("get Secret after the call = %v, want NotFound", gerr)
				}
				return
			}
			if gerr != nil {
				t.Fatalf("get Secret: %v", gerr)
			}
			if !reflect.DeepEqual(stored.Data, map[string][]byte{issuer.KeySecretField: tc.wantData}) {
				t.Errorf("Secret data = %q, want %s %q", stored.Data, issuer.KeySecretField, tc.wantData)
			}
			if stored.Type != corev1.SecretTypeOpaque {
				t.Errorf("Secret type = %q, want %q", stored.Type, corev1.SecretTypeOpaque)
			}
			if v := stored.Labels["app.kubernetes.io/managed-by"]; v != "fiberd-k8s-controller" {
				t.Errorf("Secret managed-by label = %q, want fiberd-k8s-controller", v)
			}
		})
	}
}

// TestLoadOrCreate covers the wrapper over grant.LoadOrCreate. The ring's
// own rules are fiberd's and tested there.
func TestLoadOrCreate(t *testing.T) {
	present, presentRaw := encodedRing(t, jose.EdDSA)
	winner, winnerRaw := encodedRing(t, jose.EdDSA)
	brokenRaw := []byte("{not json")

	cases := []struct {
		name string
		// secret is the key Secret that exists before the first call.
		secret *corev1.Secret
		// create replaces the fake client's Create when set.
		create    func(context.Context, client.WithWatch, client.Object) error
		namespace string
		// wantCreates is the number of Creates after both calls.
		wantCreates int
		// wantKIDs lists the returned kids. Nil means one fresh key.
		wantKIDs []string
		// wantErr is a substring of the error. Empty means success.
		wantErr string
	}{
		{
			name:        "creates once then loads",
			namespace:   keyNamespace,
			wantCreates: 1,
		},
		{
			name:      "loads a stored ring without creating",
			secret:    keySecret(map[string][]byte{issuer.KeySecretField: presentRaw}),
			namespace: keyNamespace,
			wantKIDs:  kids(present),
		},
		{
			name: "lost create race returns the stored ring",
			create: func(ctx context.Context, cl client.WithWatch, obj client.Object) error {
				// Another replica creates the Secret first.
				if err := cl.Create(ctx, keySecret(map[string][]byte{issuer.KeySecretField: winnerRaw})); err != nil {
					return err
				}
				return apierrors.NewAlreadyExists(secretsGR, obj.GetName())
			},
			namespace:   keyNamespace,
			wantCreates: 1,
			wantKIDs:    kids(winner),
		},
		{
			name:      "stored ring that does not decode is kept",
			secret:    keySecret(map[string][]byte{issuer.KeySecretField: brokenRaw}),
			namespace: keyNamespace,
			wantErr:   keySecretRef,
		},
		{
			name:    "empty namespace",
			wantErr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, c, _, creates := countingClient(tc.secret, nil, tc.create)

			first, err := issuer.LoadOrCreate(t.Context(), c, tc.namespace)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadOrCreate returned %v, want an error containing %q", kids(first), tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("LoadOrCreate error %q does not contain %q", err, tc.wantErr)
				}
				if *creates != 0 {
					t.Errorf("Create called %d times, want 0", *creates)
				}
				// A Secret that cannot be read is never overwritten.
				if tc.secret != nil {
					var after corev1.Secret
					if gerr := base.Get(t.Context(), keySecretKey, &after); gerr != nil {
						t.Fatalf("get Secret after the error: %v", gerr)
					}
					if !reflect.DeepEqual(after.Data, tc.secret.Data) {
						t.Errorf("LoadOrCreate changed the Secret data after an error")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadOrCreate: %v", err)
			}
			if tc.wantKIDs != nil {
				if !reflect.DeepEqual(kids(first), tc.wantKIDs) {
					t.Errorf("LoadOrCreate kids = %v, want %v", kids(first), tc.wantKIDs)
				}
			} else if len(first) != 1 || first[0].JWK.Algorithm != string(jose.EdDSA) {
				t.Errorf("LoadOrCreate returned kids %v, want one fresh EdDSA key", kids(first))
			}
			if _, err := first.Signing(); err != nil {
				t.Errorf("Signing on the returned ring: %v", err)
			}

			// A second call, as a restarted replica makes, loads what the
			// first stored and creates nothing more.
			second, err := issuer.LoadOrCreate(t.Context(), c, tc.namespace)
			if err != nil {
				t.Fatalf("second LoadOrCreate: %v", err)
			}
			if !reflect.DeepEqual(kids(second), kids(first)) {
				t.Errorf("second LoadOrCreate kids = %v, want %v", kids(second), kids(first))
			}
			if *creates != tc.wantCreates {
				t.Errorf("Create called %d times, want %d", *creates, tc.wantCreates)
			}
		})
	}
}
