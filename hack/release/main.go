// Command release makes the decisions of the release workflow, so table tests
// cover them on every pull request. Run it from the root of the repository.
//
//	release validate -version <tag> -image <name> -chart <name> [-dry-run]
//	release pin [-enforce]
//	release guard -tag <tag>
//	release finish -tag <tag> -digest <digest>
//
// validate, guard and finish print key=value lines for GITHUB_OUTPUT. Any doubt
// stops a check. Only a complete listing proves that a tag is not released.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	fiberdModule = "github.com/Azure/fiberd"
	mainRef      = "refs/heads/main"
	registry     = "ghcr.io/"
)

var (
	versionPattern = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(-(?:alpha|beta|rc)\.(?:0|[1-9][0-9]*))?$`)
	pseudoPattern  = regexp.MustCompile(`[-.][0-9]{14}-[0-9a-f]{12}(?:\+incompatible)?$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// release is the part of a GitHub release these checks read.
type release struct {
	ID    int64  `json:"id"`
	Tag   string `json:"tag_name"`
	Draft bool   `json:"draft"`
	Body  string `json:"body"`
}

// gitRef is one entry of a matching-refs listing.
type gitRef struct {
	Ref string `json:"ref"`
}

// module is what go list reports for the fiberd requirement.
type module struct {
	Version string
	Replace *module
}

// outside holds the calls that leave this process, so a test starts none.
type outside struct {
	list   func(path string) ([]byte, error)
	pin    func() (module, error)
	getenv func(key string) string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, outside{list: ghList, pin: fiberdPin, getenv: os.Getenv}); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer, o outside) error {
	if len(args) == 0 {
		return errors.New("usage: release validate|pin|guard|finish")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "validate":
		tag := flags.String("version", "", "the release tag")
		image := flags.String("image", "", "the image name")
		chart := flags.String("chart", "", "the chart's repository")
		dryRun := flags.Bool("dry-run", false, "publish nothing, so skip the rules on publishing")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return validate(*tag, *image, *chart, *dryRun, out, o)
	case "pin":
		enforce := flags.Bool("enforce", false, "fail on a problem instead of warning")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return pin(*enforce, out, o)
	case "guard":
		tag := flags.String("tag", "", "the release tag")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return guardCommand(*tag, out, o)
	case "finish":
		tag := flags.String("tag", "", "the release tag")
		digest := flags.String("digest", "", "the image digest this run built")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return finishCommand(*tag, *digest, out, o)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func validate(tag, image, chart string, dryRun bool, out io.Writer, o outside) error {
	version, prerelease, err := parseVersion(tag)
	if err != nil {
		return err
	}
	if err := checkNames(image, chart); err != nil {
		return err
	}
	if !dryRun {
		repository := o.getenv("GITHUB_REPOSITORY")
		if err := checkSource(image, chart, o.getenv("GITHUB_REF"), repository); err != nil {
			return err
		}
		refs, err := listPages[gitRef](o, "repos/"+repository+"/git/matching-refs/tags/"+tag)
		if err != nil {
			return err
		}
		if slices.Contains(refs, gitRef{Ref: "refs/tags/" + tag}) {
			return fmt.Errorf("the tag %s already exists, and a release creates its own tag", tag)
		}
		releases, err := listReleases(o)
		if err != nil {
			return err
		}
		if _, err := guard(releases, tag); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "version=%s\nprerelease=%t\n", version, prerelease)
	return err
}

// parseVersion returns the tag without its v, and whether it is a pre-release.
func parseVersion(tag string) (string, bool, error) {
	match := versionPattern.FindStringSubmatch(tag)
	if match == nil {
		return "", false, fmt.Errorf("the version %q is not vX.Y.Z, or that with -alpha.N, -beta.N or -rc.N", tag)
	}
	return strings.TrimPrefix(tag, "v"), match[1] != "", nil
}

// checkNames runs on a dry run too, since a registry refuses a capital and no
// other step before a push would notice.
func checkNames(names ...string) error {
	for _, name := range names {
		if name == "" {
			return errors.New("an image or chart name is empty")
		}
		if name != strings.ToLower(name) {
			return fmt.Errorf("%s holds a capital, which a registry refuses", name)
		}
	}
	return nil
}

// checkSource holds a real run to main and to the repository that owns the
// literal names, so a fork or a moved repository stops before its gates run.
func checkSource(image, chart, ref, repository string) error {
	if ref != mainRef {
		return fmt.Errorf("a release starts from %s, and this run started from %q", mainRef, ref)
	}
	owner, name, ok := strings.Cut(strings.ToLower(repository), "/")
	if !ok {
		return fmt.Errorf("%q is not a repository", repository)
	}
	if want := registry + owner + "/" + name; image != want {
		return fmt.Errorf("%s cannot publish %s", repository, image)
	}
	if want := registry + owner + "/charts/" + name; chart != want {
		return fmt.Errorf("%s cannot publish %s", repository, chart)
	}
	return nil
}

func pin(enforce bool, out io.Writer, o outside) error {
	pinned, err := o.pin()
	if err != nil {
		return err
	}
	problems := pinProblems(pinned, o.getenv("FIBERD_VERSION"))
	if enforce && len(problems) > 0 {
		return errors.New(strings.Join(problems, ". "))
	}
	prefix := "warning: "
	if o.getenv("GITHUB_ACTIONS") != "" {
		prefix = "::warning::"
	}
	for _, problem := range problems {
		if _, err := fmt.Fprintln(out, prefix+problem); err != nil {
			return err
		}
	}
	return nil
}

// pinProblems lists why this fiberd pin may not ship. go.mod is the only
// statement of the version, and the variable selects what tests run.
func pinProblems(pinned module, variable string) []string {
	var problems []string
	if pseudoPattern.MatchString(pinned.Version) {
		problems = append(problems, fmt.Sprintf("go.mod pins fiberd at the pseudo-version %s, and a release needs a fiberd tag", pinned.Version))
	}
	if pinned.Replace != nil {
		problems = append(problems, "go.mod replaces fiberd, and a release ships the version it names")
	}
	if variable != "" && variable != pinned.Version {
		problems = append(problems, fmt.Sprintf("FIBERD_VERSION is %s, and go.mod pins %s", variable, pinned.Version))
	}
	return problems
}

func guardCommand(tag string, out io.Writer, o outside) error {
	if _, _, err := parseVersion(tag); err != nil {
		return err
	}
	releases, err := listReleases(o)
	if err != nil {
		return err
	}
	id, err := guard(releases, tag)
	if err != nil {
		return err
	}
	return writeDraft(out, id)
}

func finishCommand(tag, digest string, out io.Writer, o outside) error {
	if _, _, err := parseVersion(tag); err != nil {
		return err
	}
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("%q is not an image digest", digest)
	}
	releases, err := listReleases(o)
	if err != nil {
		return err
	}
	id, finished, err := finish(releases, tag, digest)
	if err != nil {
		return err
	}
	if err := writeDraft(out, id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "finished=%t\n", finished)
	return err
}

// writeDraft prints an empty value for no draft, which a workflow tests with if.
func writeDraft(out io.Writer, id int64) error {
	value := ""
	if id != 0 {
		value = strconv.FormatInt(id, 10)
	}
	_, err := fmt.Fprintf(out, "draft-id=%s\n", value)
	return err
}

// guard is the first step of every write job. It refuses a released tag and
// returns the tag's draft, or 0 when it has none. A token that cannot write
// contents sees no drafts, so such a job only learns that the tag is not
// released.
func guard(releases []release, tag string) (int64, error) {
	published, drafts := ofTag(releases, tag)
	if len(published) > 0 {
		return 0, fmt.Errorf("%s is already released", tag)
	}
	switch len(drafts) {
	case 0:
		return 0, nil
	case 1:
		return drafts[0].ID, nil
	default:
		return 0, fmt.Errorf("%s has %d drafts, so this run cannot tell which is its own", tag, len(drafts))
	}
}

// finish decides the last step. A digest names one build, so the draft must
// list this run's digest, and an old run that is re-run cannot publish a draft
// that a newer run refreshed. A release that is live with this run's digest
// means an earlier attempt published it and died, so it reports finished.
func finish(releases []release, tag, digest string) (int64, bool, error) {
	published, drafts := ofTag(releases, tag)
	if len(published) > 0 {
		if strings.Contains(published[0].Body, digest) {
			return 0, true, nil
		}
		return 0, false, fmt.Errorf("%s is already released with another image", tag)
	}
	switch len(drafts) {
	case 0:
		return 0, false, fmt.Errorf("%s has no draft to publish", tag)
	case 1:
		if !strings.Contains(drafts[0].Body, digest) {
			return 0, false, fmt.Errorf("the draft of %s does not list %s, so it belongs to another run", tag, digest)
		}
		return drafts[0].ID, false, nil
	default:
		return 0, false, fmt.Errorf("%s has %d drafts, so this run cannot tell which is its own", tag, len(drafts))
	}
}

func ofTag(releases []release, tag string) (published, drafts []release) {
	for _, r := range releases {
		switch {
		case r.Tag != tag:
		case r.Draft:
			drafts = append(drafts, r)
		default:
			published = append(published, r)
		}
	}
	return published, drafts
}

func listReleases(o outside) ([]release, error) {
	return listPages[release](o, "repos/"+o.getenv("GITHUB_REPOSITORY")+"/releases")
}

// listPages reads a whole paginated listing. A failed call, an empty answer or
// anything that is not an array stops here, since each would read as "not
// there" further on.
func listPages[T any](o outside, path string) ([]T, error) {
	data, err := o.list(path + "?per_page=100")
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var items []T
	pages := 0
	for {
		var page []T
		err := decoder.Decode(&page)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the listing of %s: %w", path, err)
		}
		if page == nil {
			return nil, fmt.Errorf("the listing of %s holds no array", path)
		}
		items = append(items, page...)
		pages++
	}
	if pages == 0 {
		return nil, fmt.Errorf("the listing of %s is empty", path)
	}
	return items, nil
}

func ghList(path string) ([]byte, error) {
	cmd := exec.Command("gh", "api", "--paginate", path)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func fiberdPin() (module, error) {
	cmd := exec.Command("go", "list", "-m", "-json", fiberdModule)
	cmd.Stderr = os.Stderr
	data, err := cmd.Output()
	if err != nil {
		return module{}, fmt.Errorf("reading the fiberd pin: %w", err)
	}
	var pinned module
	if err := json.Unmarshal(data, &pinned); err != nil {
		return module{}, fmt.Errorf("reading the fiberd pin: %w", err)
	}
	if pinned.Version == "" {
		return module{}, errors.New("go.mod names no fiberd version")
	}
	return pinned, nil
}
