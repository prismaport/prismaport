package version

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// readPrivate reads a file of the private repository (VERSION, the release scripts, the Pages
// tooling). The public tree carries internal/ without them, so there the check is skipped; the
// private CI runs it in full.
func readPrivate(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s is not in this tree (it is not published)", filepath.ToSlash(path))
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The root VERSION file is the source of truth; the Go constant and every web package.json carry
// the same string (scripts/set-version.sh writes them all).
func TestVersionMatchesTheRootFileAndTheWebManifests(t *testing.T) {
	root := readPrivate(t, filepath.Join("..", "..", "VERSION"))
	want := strings.TrimSpace(string(root))
	if !semver.MatchString(want) {
		t.Fatalf("VERSION %q is not SemVer MAJOR.MINOR.PATCH[-PRERELEASE]", want)
	}
	if Version != want {
		t.Errorf("version.Version = %q, VERSION = %q: run scripts/set-version.sh", Version, want)
	}
	for _, app := range []string{"checkout", "dashboard", "docs", "landing", "pages"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "web", app, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct{ Version string }
		if err := json.Unmarshal(b, &pkg); err != nil {
			t.Fatal(err)
		}
		if pkg.Version != want {
			t.Errorf("web/%s/package.json version = %q, VERSION = %q: run scripts/set-version.sh", app, pkg.Version, want)
		}
	}
}

// Single test: the environment is process-global, so parallel tests would race on it.
func TestCommitPrefersTheEnvironmentAndShortensIt(t *testing.T) {
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "0123456789abcdef")
	t.Setenv("GIT_COMMIT", "fedcba9876543210")
	if got := Get().Commit; got != "0123456" {
		t.Errorf("commit with RAILWAY_GIT_COMMIT_SHA: %q", got)
	}
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "")
	if got := Get().Commit; got != "fedcba9" {
		t.Errorf("commit with GIT_COMMIT: %q", got)
	}
	t.Setenv("GIT_COMMIT", "")
	if got := Get().Commit; got == "" {
		t.Error("commit with no environment is empty, want the VCS stamp or unknown")
	}
	if got := Label(); !strings.HasPrefix(got, "v"+Version+"+") {
		t.Errorf("label %q", got)
	}
}

func TestBuiltAtIsRFC3339OrEmpty(t *testing.T) {
	defer func(v string) { builtAt = v }(builtAt)
	builtAt = ""
	if got := Get().BuiltAt; got != "" {
		t.Errorf("unstamped built_at = %q", got)
	}
	builtAt = "2026-10-30T12:00:00Z"
	if got := Get().BuiltAt; got != "2026-10-30T12:00:00Z" {
		t.Errorf("stamped built_at = %q", got)
	}
	builtAt = "not a time"
	if got := Get().BuiltAt; got != "" {
		t.Errorf("garbage built_at = %q", got)
	}
}

// task release:check (scripts/check-version.sh --release) fails unless CHANGELOG.md has a section
// for the VERSION being released. It runs against a throwaway tree that is consistent, so only
// the changelog decides.
func TestReleaseCheckNeedsAChangelogSection(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	script := readPrivate(t, filepath.Join("..", "..", "scripts", "check-version.sh"))
	write := func(root, rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tree := func(changelog string) string {
		root := t.TempDir()
		write(root, "scripts/check-version.sh", string(script))
		write(root, "VERSION", "1.2.3\n")
		write(root, "internal/version/version.go", "package version\n\nconst Version = \"1.2.3\"\n")
		for _, app := range []string{"checkout", "dashboard", "docs", "landing", "pages"} {
			write(root, "web/"+app+"/package.json", `{"version":"1.2.3"}`)
			write(root, "web/"+app+"/package-lock.json", `{"version":"1.2.3","packages":{"":{"version":"1.2.3"}}}`)
		}
		if changelog != "" {
			write(root, "CHANGELOG.md", changelog)
		}
		return root
	}
	run := func(root string, args ...string) (string, error) {
		cmd := exec.Command(bash, append([]string{filepath.Join(root, "scripts", "check-version.sh")}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(tree("# Changelog\n\n## [Unreleased]\n\n## [1.2.3] - 2026-01-01\n"), "--release"); err != nil {
		t.Fatalf("release with a section: %v\n%s", err, out)
	}
	for name, changelog := range map[string]string{
		"missing file":        "",
		"only Unreleased":     "# Changelog\n\n## [Unreleased]\n\n## [1.2.2] - 2026-01-01\n",
		"a longer version":    "# Changelog\n\n## [1.2.30] - 2026-01-01\n",
		"heading without ver": "# Changelog\n\n1.2.3 is mentioned in prose only\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := run(tree(changelog), "--release")
			if err == nil || !strings.Contains(out, "CHANGELOG.md has no '## [1.2.3]' section") {
				t.Errorf("%s: release check passed or failed for another reason: %v\n%s", name, err, out)
			}
		})
	}
	// The plain consistency check (task check) does not need release notes.
	if out, err := run(tree("")); err != nil {
		t.Errorf("consistency check without a changelog: %v\n%s", err, out)
	}
}

// The release check moves forward only, by SemVer precedence: a pre-release ranks below its
// release and its numeric identifiers compare as numbers (sort -V gets both wrong). Each case is a
// throwaway repo whose main carries the base VERSION, with the candidate in the working tree.
func TestReleaseCheckOrdersBySemVerPrecedence(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, tool := range []string{"node", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	script := readPrivate(t, filepath.Join("..", "..", "scripts", "check-version.sh"))
	write := func(root, rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setVersion := func(root, v string) {
		t.Helper()
		write(root, "VERSION", v+"\n")
		write(root, "internal/version/version.go", "package version\n\nconst Version = \""+v+"\"\n")
		for _, app := range []string{"checkout", "dashboard", "docs", "landing", "pages"} {
			write(root, "web/"+app+"/package.json", `{"version":"`+v+`"}`)
			write(root, "web/"+app+"/package-lock.json", `{"version":"`+v+`","packages":{"":{"version":"`+v+`"}}}`)
		}
		write(root, "CHANGELOG.md", "# Changelog\n\n## ["+v+"] - 2026-01-01\n")
	}
	git := func(root string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	check := func(base, next string) error {
		root := t.TempDir()
		write(root, "scripts/check-version.sh", string(script))
		setVersion(root, base)
		git(root, "init", "-q", "-b", "main")
		git(root, "add", "-A")
		git(root, "commit", "-q", "-m", "base")
		setVersion(root, next)
		cmd := exec.Command(bash, filepath.Join(root, "scripts", "check-version.sh"), "--release")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "must be greater than") && !strings.Contains(string(out), "is unchanged") {
			t.Fatalf("%s -> %s failed for another reason: %v\n%s", base, next, err, out)
		}
		return err
	}

	for _, c := range []struct {
		base, next string
		ok         bool
	}{
		{"0.6.0", "1.0.0-beta.3", true},
		{"1.0.0-beta.3", "1.0.0-beta.4", true},
		{"1.0.0-beta.9", "1.0.0-beta.10", true},
		{"1.0.0-beta.3", "1.0.0-rc.1", true},
		{"1.0.0-rc.1", "1.0.0", true},
		{"0.6.0", "0.6.1", true},
		{"1.0.0", "1.0.0-beta.4", false},
		{"1.0.0-beta.10", "1.0.0-beta.9", false},
		{"1.0.0-beta.3", "1.0.0-beta.3", false},
		{"1.0.0-beta.3", "0.7.0", false},
	} {
		t.Run(c.base+"->"+c.next, func(t *testing.T) {
			if err := check(c.base, c.next); (err == nil) != c.ok {
				t.Errorf("release check %s -> %s: passed = %v, want %v", c.base, c.next, err == nil, c.ok)
			}
		})
	}
}
