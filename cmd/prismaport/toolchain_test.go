package main

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"testing"
)

// readPrivate reads a deploy or tooling file of the private repository. The public tree carries
// cmd/ and internal/ without them, so there the check is skipped; the private CI runs it in full.
func readPrivate(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s is not in this tree (it is not published)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The image must build with the same patched toolchain go.mod pins, or the
// deployed binary carries standard library fixes the local build has not.
func TestDockerfileBuildsWithTheToolchainGoModPins(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := readPrivate(t, "../../Dockerfile")

	pinned := regexp.MustCompile(`(?m)^toolchain go(\d+\.\d+\.\d+)$`).FindSubmatch(gomod)
	base := regexp.MustCompile(`(?m)^FROM golang:(\d+\.\d+\.\d+)-alpine[@ ]`).FindSubmatch(dockerfile)

	if pinned == nil {
		t.Fatal("go.mod has no toolchain line with a patch version")
	}
	if base == nil {
		t.Fatal("Dockerfile has no golang:<patch>-alpine build stage")
	}
	if string(pinned[1]) != string(base[1]) {
		t.Fatalf("go.mod pins go%s but the Dockerfile builds with golang:%s", pinned[1], base[1])
	}
}

// The Pages deploys (Taskfile) and the local split stack (web/pages) must run one wrangler.
func TestWranglerIsPinnedOnce(t *testing.T) {
	taskfile := readPrivate(t, "../../Taskfile.yml")
	pkg := readPrivate(t, "../../web/pages/package.json")
	task := regexp.MustCompile(`(?m)^  WRANGLER_VERSION: (\d+\.\d+\.\d+)$`).FindSubmatch(taskfile)
	npm := regexp.MustCompile(`"wrangler": "(\d+\.\d+\.\d+)"`).FindSubmatch(pkg)
	if task == nil || npm == nil {
		t.Fatalf("no exact wrangler pin: Taskfile %q, package.json %q", task, npm)
	}
	if string(task[1]) != string(npm[1]) {
		t.Fatalf("Taskfile.yml deploys with wrangler %s but web/pages runs %s", task[1], npm[1])
	}
	if !regexp.MustCompile(`npx wrangler@\{\{\.WRANGLER_VERSION\}\} pages deploy`).Match(taskfile) {
		t.Fatal("the Pages deploys do not use WRANGLER_VERSION")
	}
}
