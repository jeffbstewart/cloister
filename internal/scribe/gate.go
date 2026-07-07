package scribe

import (
	"path"
	"strings"

	"github.com/jeffbstewart/cloister/internal/manifest"
)

// isBuildLogic reports whether a workspace-relative (slash-separated) path is
// part of the project's BUILD LOGIC — the set whose edits change how the jailed
// build runs.  `gradlew` executes settings.gradle.kts and every build.gradle.kts
// during configuration, so a write here can turn a "build" into arbitrary code.
// These writes are refused outright (rejected_gate) without an approval
// channel, and gated on human approval with one.
//
// This mirrors EXACTLY the airlock's git-clean gate (update-gradle-deps.bat):
//
//	agent-harness.yaml  *.gradle.kts  gradle/  buildSrc/  gradle.properties  gradlew  gradlew.bat
//
// Keep the two lists in sync — they are the same trust boundary from two sides.
func isBuildLogic(rel string) bool {
	rel = strings.TrimPrefix(path.Clean(rel), "./")
	base := path.Base(rel)
	first, _, _ := strings.Cut(rel, "/")
	switch {
	case rel == manifest.DefaultPath: // agent-harness.yaml, the action manifest
		return true
	case base == "gradlew" || base == "gradlew.bat":
		return true
	case base == "gradle.properties":
		return true
	case strings.HasSuffix(base, ".gradle.kts"):
		return true
	case first == "gradle" || first == "buildSrc": // wrapper + version catalogs + buildSrc subtree
		return true
	}
	return false
}
