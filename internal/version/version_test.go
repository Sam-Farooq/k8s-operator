package version

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMakefileAgrees reads the Makefile rather than trusting it. The Makefile
// stamps VERSION into the binary with -ldflags, so if the two drift, a
// `make build` binary reports one number and the package reports another.
func TestMakefileAgrees(t *testing.T) {
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^VERSION \?= (.+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the Makefile has no VERSION assignment")
	}
	if got := strings.TrimSpace(string(m[1])); got != Version {
		t.Fatalf("Makefile VERSION is %q and version.Version is %q", got, Version)
	}
}

// TestLdflagsTargetExists catches a silent no-op that cost half an hour the
// first time. `go build -ldflags "-X main.version=..."` against a binary
// whose variable is called something else does not fail and does not warn:
// the flag is ignored and the binary reports the compiled-in default.
func TestLdflagsTargetExists(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	m := regexp.MustCompile(`-X main\.([A-Za-z_][A-Za-z0-9_]*)=`).FindSubmatch(makefile)
	if m == nil {
		t.Fatal("the Makefile does not stamp a version with -X main.<var>")
	}
	target := string(m[1])

	main, err := os.ReadFile("../../cmd/main.go")
	if err != nil {
		t.Fatalf("reading cmd/main.go: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^var ([A-Za-z_][A-Za-z0-9_]*) = version\.Version$`).FindSubmatch(main)
	if declared == nil {
		t.Fatal("cmd/main.go has no package-level string var defaulting to version.Version")
	}
	if got := string(declared[1]); got != target {
		t.Fatalf("the Makefile stamps main.%s and cmd/main.go declares main.%s, so the flag is ignored", target, got)
	}
}

func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version = %q", Version)
	}
}
