package issuer

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

const (
	// KeySecretName is the Secret that holds the grant signing keys.
	KeySecretName = "grant-issuer-key"
	// KeySecretField is the data field of that Secret. It holds the
	// grant.KeyRing as grant.KeyRing.Encode writes it, newest key first.
	KeySecretField = "keys.json"
)

// SecretKeyStore is a grant.KeyStore backed by the KeySecretName Secret
// in Namespace. The ring's rules, such as which key signs and which keys
// are published, belong to grant.KeyRing. The store only reads and
// creates the Secret, and it never updates or deletes one.
//
// Client should read from the API server rather than from an informer
// cache. A cached read can miss a Secret another replica created a moment
// ago.
type SecretKeyStore struct {
	Client    client.Client
	Namespace string
}

var _ grant.KeyStore = (*SecretKeyStore)(nil)

// objectKey is the key Secret's namespace and name. It is an error when
// the store has no namespace.
func (s *SecretKeyStore) objectKey() (client.ObjectKey, error) {
	if s.Namespace == "" {
		return client.ObjectKey{}, errors.New("issuer: the key Secret needs a namespace")
	}
	return client.ObjectKey{Namespace: s.Namespace, Name: KeySecretName}, nil
}

// Load returns the KeySecretField bytes of the key Secret. A Secret that
// does not exist, or that has no such field or an empty one, is returned
// as grant.ErrNoKeyRing. Any other error names the Secret.
func (s *SecretKeyStore) Load(ctx context.Context) ([]byte, error) {
	key, err := s.objectKey()
	if err != nil {
		return nil, err
	}
	var secret corev1.Secret
	if err := s.Client.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("issuer: Secret %s: %w", key, grant.ErrNoKeyRing)
		}
		return nil, fmt.Errorf("issuer: get Secret %s: %w", key, err)
	}
	raw := secret.Data[KeySecretField]
	if len(raw) == 0 {
		return nil, fmt.Errorf("issuer: Secret %s has no %s field: %w", key, KeySecretField, grant.ErrNoKeyRing)
	}
	return raw, nil
}

// Create creates the key Secret with raw in its KeySecretField. It returns
// grant.ErrKeyRingExists when the Secret already exists, such as when
// another replica created it first, and leaves that Secret as it is.
func (s *SecretKeyStore) Create(ctx context.Context, raw []byte) error {
	key, err := s.objectKey()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedBy},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KeySecretField: raw},
	}
	if err := s.Client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("issuer: Secret %s: %w", key, grant.ErrKeyRingExists)
		}
		return fmt.Errorf("issuer: create Secret %s: %w", key, err)
	}
	return nil
}

// LoadOrCreate returns the key ring in the KeySecretName Secret of
// namespace. It is grant.LoadOrCreate over a SecretKeyStore, so a missing
// Secret is created with one fresh EdDSA key, and a create that loses a
// race to another replica returns the ring that replica stored. A Secret
// that exists but does not decode is an error and is never overwritten.
func LoadOrCreate(ctx context.Context, c client.Client, namespace string) (grant.KeyRing, error) {
	store := &SecretKeyStore{Client: c, Namespace: namespace}
	if _, err := store.objectKey(); err != nil {
		return nil, err
	}
	ring, err := grant.LoadOrCreate(ctx, store, nil)
	if err != nil {
		return nil, fmt.Errorf("issuer: key ring in Secret %s/%s: %w", namespace, KeySecretName, err)
	}
	return ring, nil
}
