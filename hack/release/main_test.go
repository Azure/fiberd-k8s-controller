package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	repository = "Azure/fiberd-k8s-controller"
	image      = "ghcr.io/azure/fiberd-k8s-controller"
	chart      = "ghcr.io/azure/charts/fiberd-k8s-controller"
	tag        = "v0.1.0-rc.1"

	digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	other  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	releasesPath = "repos/" + repository + "/releases?per_page=100"
	refsPath     = "repos/" + repository + "/git/matching-refs/tags/" + tag + "?per_page=100"
)

// fake answers each listing from a map. A path it does not hold fails the call.
func fake(answers map[string]string, env map[string]string) outside {
	return outside{
		list: func(path string) ([]byte, error) {
			answer, ok := answers[path]
			if !ok {
				return nil, errors.New("no answer for " + path)
			}
			return []byte(answer), nil
		},
		pin:    func() (module, error) { return module{Version: "v0.4.0"}, nil },
		getenv: func(key string) string { return env[key] },
	}
}

func realRun() map[string]string {
	return map[string]string{"GITHUB_REPOSITORY": repository, "GITHUB_REF": mainRef}
}

func wantError(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case want != "" && err == nil:
		t.Fatalf("got no error, want one holding %q", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("error = %v, want it to hold %q", err, want)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		tag        string
		version    string
		prerelease bool
		wantErr    bool
	}{
		{tag: "v0.1.0", version: "0.1.0"},
		{tag: "v1.20.300", version: "1.20.300"},
		{tag: "v0.1.0-rc.1", version: "0.1.0-rc.1", prerelease: true},
		{tag: "v0.1.0-alpha.0", version: "0.1.0-alpha.0", prerelease: true},
		{tag: "v0.1.0-beta.12", version: "0.1.0-beta.12", prerelease: true},
		{tag: "", wantErr: true},
		{tag: "0.1.0", wantErr: true},
		{tag: "v0.1", wantErr: true},
		{tag: "v01.1.0", wantErr: true},
		{tag: "v0.1.0-rc1", wantErr: true},
		{tag: "v0.1.0-rc.01", wantErr: true},
		{tag: "v0.1.0-dev.1", wantErr: true},
		{tag: "v0.1.0+build", wantErr: true},
		{tag: "v0.1.0\n", wantErr: true},
		{tag: "v0.1.0/../../other", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			version, prerelease, err := parseVersion(tc.tag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want an error %t", err, tc.wantErr)
			}
			if version != tc.version || prerelease != tc.prerelease {
				t.Errorf("got %q, %t, want %q, %t", version, prerelease, tc.version, tc.prerelease)
			}
		})
	}
}

func TestCheckNames(t *testing.T) {
	wantError(t, checkNames(image, chart), "")
	wantError(t, checkNames("ghcr.io/Azure/fiberd-k8s-controller", chart), "capital")
	wantError(t, checkNames(image, ""), "empty")
}

func TestCheckSource(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		chart      string
		ref        string
		repository string
		want       string
	}{
		{name: "main of the owning repository", image: image, chart: chart, ref: mainRef, repository: repository},
		{name: "a branch", image: image, chart: chart, ref: "refs/heads/topic", repository: repository, want: "started from"},
		{name: "a tag", image: image, chart: chart, ref: "refs/tags/v0.1.0", repository: repository, want: "started from"},
		{name: "no ref", image: image, chart: chart, repository: repository, want: "started from"},
		{name: "a fork", image: image, chart: chart, ref: mainRef, repository: "someone/fiberd-k8s-controller", want: "cannot publish"},
		{name: "a moved repository", image: image, chart: chart, ref: mainRef, repository: "Azure/other", want: "cannot publish"},
		{name: "another chart path", image: image, chart: image, ref: mainRef, repository: repository, want: "cannot publish"},
		{name: "no repository", image: image, chart: chart, ref: mainRef, want: "not a repository"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, checkSource(tc.image, tc.chart, tc.ref, tc.repository), tc.want)
		})
	}
}

func TestPinProblems(t *testing.T) {
	const tagged = "v0.4.0"
	tests := []struct {
		name     string
		pinned   module
		variable string
		want     []string
	}{
		{name: "a release", pinned: module{Version: tagged}},
		{name: "a release candidate", pinned: module{Version: "v0.4.0-rc.1"}},
		{name: "the variable agrees", pinned: module{Version: tagged}, variable: tagged},
		{
			name:   "a pseudo-version after a tag",
			pinned: module{Version: "v0.2.1-0.20261106190126-afb3f4c8a866"},
			want:   []string{"pseudo-version"},
		},
		{
			name:   "a pseudo-version with no tag before it",
			pinned: module{Version: "v0.0.0-20261106190126-afb3f4c8a866"},
			want:   []string{"pseudo-version"},
		},
		{
			name:   "a pseudo-version after a pre-release",
			pinned: module{Version: "v0.4.0-rc.1.0.20261106190126-afb3f4c8a866"},
			want:   []string{"pseudo-version"},
		},
		{
			name:   "a replace",
			pinned: module{Version: tagged, Replace: &module{Version: "v0.5.0"}},
			want:   []string{"replaces"},
		},
		{
			name:     "the variable differs",
			pinned:   module{Version: tagged},
			variable: "v0.4.1",
			want:     []string{"FIBERD_VERSION"},
		},
		{
			name:     "every problem at once",
			pinned:   module{Version: "v0.0.0-20261106190126-afb3f4c8a866", Replace: &module{}},
			variable: tagged,
			want:     []string{"pseudo-version", "replaces", "FIBERD_VERSION"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pinProblems(tc.pinned, tc.variable)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d problems %q, want %d", len(got), got, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("problem %d = %q, want it to hold %q", i, got[i], want)
				}
			}
		})
	}
}

func TestPin(t *testing.T) {
	pseudo := func() (module, error) { return module{Version: "v0.0.0-20261106190126-afb3f4c8a866"}, nil }
	tests := []struct {
		name    string
		enforce bool
		pin     func() (module, error)
		env     map[string]string
		wantOut string
		wantErr string
	}{
		{name: "a clean pin says nothing", pin: func() (module, error) { return module{Version: "v0.4.0"}, nil }},
		{name: "warns by default", pin: pseudo, wantOut: "warning: go.mod pins"},
		{name: "warns in the workflow's form", pin: pseudo, env: map[string]string{"GITHUB_ACTIONS": "true"}, wantOut: "::warning::go.mod pins"},
		{name: "enforce fails", enforce: true, pin: pseudo, wantErr: "pseudo-version"},
		{
			name:    "a failed read stops even when warning",
			pin:     func() (module, error) { return module{}, errors.New("no module") },
			wantErr: "no module",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := fake(nil, tc.env)
			o.pin = tc.pin
			var out bytes.Buffer
			wantError(t, pin(tc.enforce, &out, o), tc.wantErr)
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output = %q, want it to hold %q", out.String(), tc.wantOut)
			}
			if tc.wantOut == "" && out.Len() != 0 {
				t.Errorf("output = %q, want none", out.String())
			}
		})
	}
}

func TestValidate(t *testing.T) {
	clean := map[string]string{refsPath: `[]`, releasesPath: `[]`}
	tests := []struct {
		name    string
		tag     string
		image   string
		dryRun  bool
		answers map[string]string
		env     map[string]string
		want    string
		wantErr string
	}{
		{
			name:    "a new pre-release",
			tag:     tag,
			answers: clean,
			env:     realRun(),
			want:    "version=0.1.0-rc.1\nprerelease=true\n",
		},
		{
			name:    "a new final release",
			tag:     "v0.1.0",
			answers: map[string]string{"repos/" + repository + "/git/matching-refs/tags/v0.1.0?per_page=100": `[]`, releasesPath: `[]`},
			env:     realRun(),
			want:    "version=0.1.0\nprerelease=false\n",
		},
		{
			name:   "a dry run from a branch of a fork lists nothing",
			tag:    tag,
			dryRun: true,
			env:    map[string]string{"GITHUB_REPOSITORY": "someone/fork", "GITHUB_REF": "refs/heads/topic"},
			want:   "version=0.1.0-rc.1\nprerelease=true\n",
		},
		{name: "a dry run still checks the version", tag: "v1", dryRun: true, wantErr: "is not vX.Y.Z"},
		{name: "a dry run still checks the names", tag: tag, image: "ghcr.io/Azure/x", dryRun: true, wantErr: "capital"},
		{
			name:    "a real run from a branch",
			tag:     tag,
			answers: clean,
			env:     map[string]string{"GITHUB_REPOSITORY": repository, "GITHUB_REF": "refs/heads/topic"},
			wantErr: "started from",
		},
		{
			name:    "the tag exists",
			tag:     tag,
			answers: map[string]string{refsPath: `[{"ref":"refs/tags/v0.1.0-rc.1"}]`, releasesPath: `[]`},
			env:     realRun(),
			wantErr: "already exists",
		},
		{
			name:    "a longer tag shares the prefix",
			tag:     tag,
			answers: map[string]string{refsPath: `[{"ref":"refs/tags/v0.1.0-rc.10"}]`, releasesPath: `[]`},
			env:     realRun(),
			want:    "version=0.1.0-rc.1\nprerelease=true\n",
		},
		{
			name:    "the refs cannot be listed",
			tag:     tag,
			answers: map[string]string{releasesPath: `[]`},
			env:     realRun(),
			wantErr: "no answer",
		},
		{
			name:    "the releases cannot be listed",
			tag:     tag,
			answers: map[string]string{refsPath: `[]`},
			env:     realRun(),
			wantErr: "no answer",
		},
		{
			name:    "the version is released with no tag visible",
			tag:     tag,
			answers: map[string]string{refsPath: `[]`, releasesPath: `[{"tag_name":"v0.1.0-rc.1","draft":false}]`},
			env:     realRun(),
			wantErr: "already released",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.image
			if name == "" {
				name = image
			}
			var out bytes.Buffer
			wantError(t, validate(tc.tag, name, chart, tc.dryRun, &out, fake(tc.answers, tc.env)), tc.wantErr)
			if diff := cmp.Diff(tc.want, out.String()); diff != "" {
				t.Errorf("output differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGuard(t *testing.T) {
	draft := release{ID: 7, Tag: tag, Draft: true}
	live := release{ID: 8, Tag: tag}
	tests := []struct {
		name     string
		releases []release
		want     int64
		wantErr  string
	}{
		{name: "no release at all"},
		{name: "only other tags", releases: []release{{ID: 1, Tag: "v0.0.1"}, {ID: 2, Tag: "v0.1.0-rc.10", Draft: true}}},
		{name: "its own draft", releases: []release{{ID: 1, Tag: "v0.0.1"}, draft}, want: 7},
		{name: "released", releases: []release{live}, wantErr: "already released"},
		{name: "released beside a draft", releases: []release{draft, live}, wantErr: "already released"},
		{name: "two drafts", releases: []release{draft, {ID: 9, Tag: tag, Draft: true}}, wantErr: "2 drafts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := guard(tc.releases, tag)
			wantError(t, err, tc.wantErr)
			if got != tc.want {
				t.Errorf("draft = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFinish(t *testing.T) {
	mine := "The image is ghcr.io/azure/fiberd-k8s-controller@" + digest
	theirs := "The image is ghcr.io/azure/fiberd-k8s-controller@" + other
	tests := []struct {
		name         string
		releases     []release
		want         int64
		wantFinished bool
		wantErr      string
	}{
		{name: "its own draft", releases: []release{{ID: 7, Tag: tag, Draft: true, Body: mine}}, want: 7},
		{name: "live with its digest", releases: []release{{ID: 7, Tag: tag, Body: mine}}, wantFinished: true},
		{name: "live with another digest", releases: []release{{ID: 7, Tag: tag, Body: theirs}}, wantErr: "another image"},
		{
			name:         "live with its digest beside a draft",
			releases:     []release{{ID: 9, Tag: tag, Draft: true, Body: theirs}, {ID: 7, Tag: tag, Body: mine}},
			wantFinished: true,
		},
		{name: "a newer run's draft", releases: []release{{ID: 7, Tag: tag, Draft: true, Body: theirs}}, wantErr: "another run"},
		{name: "a draft that lists no image", releases: []release{{ID: 7, Tag: tag, Draft: true}}, wantErr: "another run"},
		{name: "no draft", releases: []release{{ID: 1, Tag: "v0.0.1", Body: mine}}, wantErr: "no draft"},
		{
			name:     "two drafts",
			releases: []release{{ID: 7, Tag: tag, Draft: true, Body: mine}, {ID: 9, Tag: tag, Draft: true, Body: mine}},
			wantErr:  "2 drafts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, finished, err := finish(tc.releases, tag, digest)
			wantError(t, err, tc.wantErr)
			if got != tc.want || finished != tc.wantFinished {
				t.Errorf("got %d, %t, want %d, %t", got, finished, tc.want, tc.wantFinished)
			}
		})
	}
}

func TestCommandsPrintOutputs(t *testing.T) {
	draft := `[{"id":7,"tag_name":"v0.1.0-rc.1","draft":true,"body":"` + digest + `"}]`
	live := `[{"id":7,"tag_name":"v0.1.0-rc.1","draft":false,"body":"` + digest + `"}]`
	tests := []struct {
		name    string
		args    []string
		answer  string
		want    string
		wantErr string
	}{
		{name: "guard with no draft", args: []string{"guard", "-tag", tag}, answer: `[]`, want: "draft-id=\n"},
		{name: "guard with a draft", args: []string{"guard", "-tag", tag}, answer: draft, want: "draft-id=7\n"},
		{name: "guard on a released tag", args: []string{"guard", "-tag", tag}, answer: live, wantErr: "already released"},
		{name: "guard on a bad tag", args: []string{"guard", "-tag", "main"}, answer: `[]`, wantErr: "is not vX.Y.Z"},
		{
			name:   "finish a draft",
			args:   []string{"finish", "-tag", tag, "-digest", digest},
			answer: draft,
			want:   "draft-id=7\nfinished=false\n",
		},
		{
			name:   "finish a live release",
			args:   []string{"finish", "-tag", tag, "-digest", digest},
			answer: live,
			want:   "draft-id=\nfinished=true\n",
		},
		{
			name:    "finish with a bad digest",
			args:    []string{"finish", "-tag", tag, "-digest", "sha256:abc"},
			answer:  draft,
			wantErr: "not an image digest",
		},
		{name: "no command", answer: `[]`, wantErr: "usage"},
		{name: "an unknown command", args: []string{"publish"}, answer: `[]`, wantErr: "unknown command"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			o := fake(map[string]string{releasesPath: tc.answer}, realRun())
			wantError(t, run(tc.args, &out, o), tc.wantErr)
			if diff := cmp.Diff(tc.want, out.String()); diff != "" {
				t.Errorf("output differs (-want +got):\n%s", diff)
			}
		})
	}
}

// TestListingStopsOnDoubt is the rule the guards rest on. Each answer here would
// read as "not released" if it were taken for an empty list.
func TestListingStopsOnDoubt(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		wantErr string
	}{
		{name: "the call failed", wantErr: "no answer"},
		{name: "an empty answer", answers: map[string]string{releasesPath: ""}, wantErr: "is empty"},
		{name: "only space", answers: map[string]string{releasesPath: " \n"}, wantErr: "is empty"},
		{name: "null", answers: map[string]string{releasesPath: "null"}, wantErr: "no array"},
		{name: "an error object", answers: map[string]string{releasesPath: `{"message":"Bad credentials"}`}, wantErr: "reading the listing"},
		{name: "a cut-off answer", answers: map[string]string{releasesPath: `[{"tag_name":"v0`}, wantErr: "reading the listing"},
		{name: "a good page then a cut-off one", answers: map[string]string{releasesPath: `[][{"tag_name"`}, wantErr: "reading the listing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := listReleases(fake(tc.answers, realRun()))
			wantError(t, err, tc.wantErr)
		})
	}
}

func TestListingReadsEveryPage(t *testing.T) {
	pages := `[{"id":1,"tag_name":"v0.0.1"}]` + "\n" + `[{"id":2,"tag_name":"v0.1.0-rc.1"}]`
	releases, err := listReleases(fake(map[string]string{releasesPath: pages}, realRun()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard(releases, tag); err == nil {
		t.Fatal("a release on the second page was missed")
	}
}
