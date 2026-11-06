//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	grantv1 "github.com/Azure/fiberd/api/grant/v1"
)

// The harness passes these. A missing one fails the test, since a skip here
// would pass a run that proved nothing.
const (
	envTarget    = "FIBERD_E2E_TARGET"
	envGrantFile = "FIBERD_E2E_GRANT_FILE"
)

// callTimeout bounds each call. A clone forks a warm template, so this only
// needs to outlast a loaded CI runner.
const callTimeout = 30 * time.Second

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set, run this through hack/tests/kind/conform.sh smoke", name)
	}
	return v
}

// podGrant reads the grant the controller issued to the Pod.
func podGrant(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(requireEnv(t, envGrantFile))
	if err != nil {
		t.Fatalf("read the Pod's grant: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// dial connects to the Pod. Grant Pods serve plaintext today.
func dial(t *testing.T) grantv1.FibersClient {
	t.Helper()
	conn, err := grpc.NewClient(requireEnv(t, envTarget), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial the Pod: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grantv1.NewFibersClient(conn)
}

// TestPodGrant clones with the Pod's own grant on an image with no test hooks.
// fiberd admits it only because its UID is the Pod's, which is the one path
// conformance cannot cover, since its image lifts that check.
func TestPodGrant(t *testing.T) {
	api := dial(t)
	token := podGrant(t)
	session := fmt.Sprintf("smoke-%d", time.Now().UnixNano())

	steps := []struct {
		name     string
		release  bool
		wantKind grantv1.CloneKind
		wantCode codes.Code
	}{
		{name: "a first clone creates the fiber", wantKind: grantv1.CloneKind_CREATE},
		{name: "a repeat attaches to it", wantKind: grantv1.CloneKind_ATTACH},
		{name: "a release ends it", release: true, wantCode: codes.OK},
		{name: "a second release finds nothing", release: true, wantCode: codes.NotFound},
	}

	var fiberID string
	for _, s := range steps {
		ok := t.Run(s.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
			defer cancel()

			if s.release {
				_, err := api.Release(ctx, &grantv1.ReleaseRequest{FiberId: fiberID, Discard: true})
				if got := status.Code(err); got != s.wantCode {
					t.Fatalf("release code = %v, want %v (err: %v)", got, s.wantCode, err)
				}
				return
			}

			got, err := api.Clone(ctx, &grantv1.CloneRequest{GrantJwt: token, Session: session})
			if err != nil {
				t.Fatalf("clone: %v", err)
			}
			if got.GetKind() != s.wantKind {
				t.Fatalf("clone kind = %v, want %v", got.GetKind(), s.wantKind)
			}
			if fiberID != "" && got.GetFiberId() != fiberID {
				t.Fatalf("clone answered fiber %q, want %q", got.GetFiberId(), fiberID)
			}
			fiberID = got.GetFiberId()
		})
		// Each step acts on what the one before it left.
		if !ok {
			t.FailNow()
		}
	}
}
