package testutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	// PollInterval is short because most waits are for an informer inside
	// the API server, which catches up quickly.
	PollInterval = 100 * time.Millisecond

	// PollTimeout bounds every wait, so a rule that never takes effect
	// fails the test instead of hanging it.
	PollTimeout = 30 * time.Second
)

// PodSpecPath is spec.podTemplate.spec in a CapacityGrant.
var PodSpecPath = []string{"spec", "podTemplate", "spec"}

// ReadObjects reads every YAML document in paths. Documents that hold only
// comments are skipped.
func ReadObjects(paths ...string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", p, err)
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				return nil, fmt.Errorf("parse %s: %w", p, err)
			}
			if len(u.Object) == 0 {
				continue
			}
			objs = append(objs, u)
		}
	}
	return objs, nil
}

// ReadObject reads the one object in path. It is ReadSample for code that
// has no testing.T, such as a TestMain.
func ReadObject(path string) (*unstructured.Unstructured, error) {
	objs, err := ReadObjects(path)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("%s holds %d objects, want 1", path, len(objs))
	}
	return objs[0], nil
}

// ReadSample reads the one object in path and fails t if there is not
// exactly one.
func ReadSample(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	u, err := ReadObject(path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// SetPodField sets one field of spec.podTemplate.spec.
func SetPodField(t *testing.T, u *unstructured.Unstructured, field string, value any) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, value, slices.Concat(PodSpecPath, []string{field})...); err != nil {
		t.Fatal(err)
	}
}

// EditContainers hands the pod template's containers to edit and stores
// what it returns.
func EditContainers(t *testing.T, u *unstructured.Unstructured, edit func([]any) []any) {
	t.Helper()
	path := slices.Concat(PodSpecPath, []string{"containers"})
	cs, _, err := unstructured.NestedSlice(u.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(u.Object, edit(cs), path...); err != nil {
		t.Fatal(err)
	}
}

// SetContainerField sets one field of the fiberd container, which the CRD
// requires to be the only one.
func SetContainerField(t *testing.T, u *unstructured.Unstructured, field string, value any) {
	t.Helper()
	EditContainers(t, u, func(cs []any) []any {
		cs[0].(map[string]any)[field] = value
		return cs
	})
}

// CleanupContext is t's context without its cancellation. t.Context is
// canceled before cleanups run, and a cleanup that restores shared state
// still calls the API server.
func CleanupContext(t *testing.T) context.Context {
	return context.WithoutCancel(t.Context())
}

// notYet is an answer that means the condition does not hold yet.
type notYet struct{ err error }

func (e notYet) Error() string { return e.err.Error() }
func (e notYet) Unwrap() error { return e.err }

// NotYet wraps err so Eventually keeps polling and, if the wait times out,
// reports err as the last answer. A deadline alone says nothing about why
// the condition never held. NotYet(nil) is nil.
func NotYet(err error) error {
	if err == nil {
		return nil
	}
	return notYet{err: err}
}

// Eventually polls cond until it holds and fails t when PollTimeout passes.
// A NotFound error from cond counts as not yet, so a cond can Get an object
// that is still being created.
func Eventually(t *testing.T, what string, cond func(ctx context.Context) (bool, error)) {
	t.Helper()
	var last error
	err := wait.PollUntilContextTimeout(CleanupContext(t), PollInterval, PollTimeout, true, func(ctx context.Context) (bool, error) {
		ok, err := cond(ctx)
		var ny notYet
		switch {
		case errors.As(err, &ny):
			last = ny.err
			return false, nil
		case apierrors.IsNotFound(err):
			last = err
			return false, nil
		}
		return ok, err
	})
	if err == nil {
		return
	}
	if last != nil {
		t.Fatalf("waiting for %s, the last answer was %v: %v", what, last, err)
	}
	t.Fatalf("waiting for %s: %v", what, err)
}
