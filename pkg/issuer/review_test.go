package issuer

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testIssuerURL      = "https://fiberd-k8s-controller.fiberd-system.svc:8443"
	testServiceAccount = "fiberd-grant"
	testToken          = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2ln"
)

func TestBearerToken(t *testing.T) {
	// atCap fills the header to exactly maxAuthorizationHeader bytes.
	atCap := strings.Repeat("a", maxAuthorizationHeader-len("Bearer "))
	cases := []struct {
		name   string
		header []string
		want   string
		wantOK bool
	}{
		{name: "a Bearer token", header: []string{"Bearer " + testToken}, want: testToken, wantOK: true},
		{name: "the scheme in another case", header: []string{"bEaReR " + testToken}, want: testToken, wantOK: true},
		{name: "two spaces before the token", header: []string{"Bearer  " + testToken}, want: testToken, wantOK: true},
		{name: "a header at the cap", header: []string{"Bearer " + atCap}, want: atCap, wantOK: true},
		{name: "no header"},
		{name: "two headers", header: []string{"Bearer " + testToken, "Bearer " + testToken}},
		{name: "another scheme", header: []string{"Basic " + testToken}},
		{name: "no token", header: []string{"Bearer"}},
		{name: "an empty token", header: []string{"Bearer "}},
		{name: "two tokens", header: []string{"Bearer " + testToken + " " + testToken}},
		{name: "a header over the cap", header: []string{"Bearer " + strings.Repeat("a", maxAuthorizationHeader)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			for _, v := range tc.header {
				r.Header.Add("Authorization", v)
			}
			got, ok := bearerToken(r)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("bearerToken = %q, %t, want %q, %t", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// acceptedStatus is the status of a Pod-bound token the issuer accepts.
// Each test takes a fresh one, so a case may change it freely.
func acceptedStatus() authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{testIssuerURL},
		User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:tenant-a:" + testServiceAccount,
			UID:      "b0a3f1d2-4c5e-4f60-8a71-92b3c4d5e6f7",
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:tenant-a", "system:authenticated"},
			Extra: map[string]authenticationv1.ExtraValue{
				podNameExtra: {"fiberd-web-1-7d9f"},
				podUIDExtra:  {podA},
			},
		},
	}
}

func TestBoundPod(t *testing.T) {
	want := BoundPod{
		PodIdentity: PodIdentity{UID: podA, Name: "fiberd-web-1-7d9f"},
		Namespace:   "tenant-a",
	}
	cases := []struct {
		name string
		// change edits the accepted status into the one under test.
		change func(*authenticationv1.TokenReviewStatus)
		// wantErr is the failed condition in the error. Empty means accepted.
		wantErr string
	}{
		{name: "an accepted token", change: func(*authenticationv1.TokenReviewStatus) {}},
		{
			name:    "a denied token",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.Authenticated = false; s.Error = "token has expired" },
			wantErr: "not authenticated",
		},
		{
			name:    "no audiences",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.Audiences = nil },
			wantErr: "audience",
		},
		{
			name:    "another audience only",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.Audiences = []string{"https://kubernetes.default.svc"} },
			wantErr: "audience",
		},
		{
			name:    "no pod name extra",
			change:  func(s *authenticationv1.TokenReviewStatus) { delete(s.User.Extra, podNameExtra) },
			wantErr: "pod name",
		},
		{
			name:    "no pod uid extra",
			change:  func(s *authenticationv1.TokenReviewStatus) { delete(s.User.Extra, podUIDExtra) },
			wantErr: "pod uid",
		},
		{
			name:    "no extras at all",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Extra = nil },
			wantErr: "pod name",
		},
		{
			name:    "two values in an extra",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Extra[podUIDExtra] = []string{podA, podB} },
			wantErr: "pod uid",
		},
		{
			name:    "an empty extra value",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Extra[podNameExtra] = []string{""} },
			wantErr: "pod name",
		},
		{
			name: "another service account",
			change: func(s *authenticationv1.TokenReviewStatus) {
				s.User.Username = "system:serviceaccount:tenant-a:default"
			},
			wantErr: "service account",
		},
		{
			name:    "a username without the prefix",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Username = "tenant-a:" + testServiceAccount },
			wantErr: "username",
		},
		{
			name: "a username with too few parts",
			change: func(s *authenticationv1.TokenReviewStatus) {
				s.User.Username = "system:serviceaccount:" + testServiceAccount
			},
			wantErr: "username",
		},
		{
			name: "a username with too many parts",
			change: func(s *authenticationv1.TokenReviewStatus) {
				s.User.Username = "system:serviceaccount:tenant-a:" + testServiceAccount + ":extra"
			},
			wantErr: "username",
		},
		{
			name: "a username with an empty namespace",
			change: func(s *authenticationv1.TokenReviewStatus) {
				s.User.Username = "system:serviceaccount::" + testServiceAccount
			},
			wantErr: "username",
		},
		{
			name:    "a username with an empty name",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Username = "system:serviceaccount:tenant-a:" },
			wantErr: "username",
		},
		{
			name: "a namespace that is not a DNS label",
			change: func(s *authenticationv1.TokenReviewStatus) {
				s.User.Username = "system:serviceaccount:Tenant_A:" + testServiceAccount
			},
			wantErr: "username",
		},
		{
			name:    "an empty username",
			change:  func(s *authenticationv1.TokenReviewStatus) { s.User.Username = "" },
			wantErr: "username",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := acceptedStatus()
			tc.change(&status)
			got, err := boundPod(status, testIssuerURL, testServiceAccount)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("boundPod returned %v, want the Pod", err)
				}
				if got != want {
					t.Fatalf("boundPod = %+v, want %+v", got, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("boundPod = %+v, want an error containing %q", got, tc.wantErr)
			}
			if got != (BoundPod{}) {
				t.Errorf("boundPod returned %+v with an error, want the zero value", got)
			}
			if !errors.Is(err, ErrNotPodToken) {
				t.Errorf("boundPod error %q does not wrap ErrNotPodToken", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("boundPod error %q does not contain %q", err, tc.wantErr)
			}
			// The text must carry nothing a client sent or the API server
			// said about it.
			for _, secret := range []string{status.Error, status.User.Username, status.User.UID} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Errorf("boundPod error %q carries %q from the status", err, secret)
				}
			}
		})
	}
}

func TestAPITokenReviewerReview(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("apiserver down")
	denied := authenticationv1.TokenReviewStatus{Error: "token has expired"}

	cases := []struct {
		name string
		// status is what the API server writes into the review.
		status authenticationv1.TokenReviewStatus
		// createErr fails the Create when set.
		createErr error
	}{
		{name: "an accepted review's status comes back", status: acceptedStatus()},
		{name: "a denied review's status comes back", status: denied},
		{name: "an API error comes back wrapped", createErr: unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sent *authenticationv1.TokenReview
			var deadline time.Time
			var hasDeadline, lateDeadline, discards bool
			c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					review, ok := obj.(*authenticationv1.TokenReview)
					if !ok {
						t.Fatalf("Create was given a %T, want a TokenReview", obj)
					}
					sent = review.DeepCopy()
					deadline, hasDeadline = ctx.Deadline()
					// The timeout starts before this call, so a deadline
					// within the limit is never after now plus the limit.
					lateDeadline = deadline.After(time.Now().Add(tokenReviewTimeout))
					discards = klog.FromContext(ctx).GetSink() == nil
					if tc.createErr != nil {
						return tc.createErr
					}
					review.Status = *tc.status.DeepCopy()
					return nil
				},
			}).Build()
			r := &APITokenReviewer{Client: c, IssuerURL: testIssuerURL}

			got, err := r.Review(context.Background(), testToken)

			if sent == nil {
				t.Fatal("Review did not create a TokenReview")
			}
			if sent.Spec.Token != testToken {
				t.Errorf("the review carries token %q, want the one given", sent.Spec.Token)
			}
			if want := []string{testIssuerURL}; !reflect.DeepEqual(sent.Spec.Audiences, want) {
				t.Errorf("the review asks for audiences %q, want %q", sent.Spec.Audiences, want)
			}
			if !hasDeadline || lateDeadline {
				t.Errorf("the request deadline is %v, has one %t, want one within %v", deadline, hasDeadline, tokenReviewTimeout)
			}
			if !discards {
				t.Error("the request context logs through a real logger, want the discard logger")
			}
			if tc.createErr != nil {
				if err == nil {
					t.Fatalf("Review = %+v, want an error", got)
				}
				if !errors.Is(err, tc.createErr) || !apierrors.IsServiceUnavailable(err) {
					t.Errorf("Review error %q does not wrap the API error", err)
				}
				if !strings.HasPrefix(err.Error(), "issuer: ") {
					t.Errorf("Review error %q is not an issuer error", err)
				}
				if strings.Contains(err.Error(), testToken) {
					t.Errorf("Review error %q carries the token", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Review returned %v, want the status", err)
			}
			if !reflect.DeepEqual(got, tc.status) {
				t.Errorf("Review = %+v, want %+v", got, tc.status)
			}
		})
	}
}
