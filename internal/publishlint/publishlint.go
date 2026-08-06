// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package publishlint refuses to let CI publish an image that may not be
// redistributed.
//
// It exists because the comment version of this rule failed.
// `.github/workflows/images.yml` carried the assertion "GHCR packages are
// created private; do not promote this one" — a default nobody had
// verified.  A package published from Actions via GITHUB_TOKEN and linked
// to a public repository inherits that repository's visibility, so the
// first successful run published an image containing a proprietary CLI
// world-readable.  It was caught by a human reading the workflow and
// asking whether the licensing added up.
//
// That is the failure this repository has documented twice already, from
// both directions: `permissions.deny` looked like enforcement and was
// inert; `--append-system-prompt-file` looked absent and worked.  A
// control that has never been observed failing closed should be assumed
// not to be a control.  So the rule is now a build gate.
//
// THE MARKER, not a list of names.  A build context declares itself
// unpublishable by carrying a DO-NOT-PUBLISH.md, and that file explains
// why in the place someone reads when they wonder.  Keying on a marker
// rather than on a hard-coded context name means a new unpublishable image
// is protected the moment it is created, by its author, rather than when
// someone remembers to update a list in another directory.
package publishlint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarkerName is the file whose presence in a build context means "CI must
// never push this".
const MarkerName = "DO-NOT-PUBLISH.md"

// workflow is the slice of GitHub Actions syntax this lint needs.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]any    `yaml:"with"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// Check returns the violations in one workflow file.  repoRoot resolves the
// build contexts a step names; an empty slice means the file is clean.
func Check(repoRoot, path string, data []byte) ([]string, error) {
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var v []string
	for _, job := range sortedKeys(w.Jobs) {
		for i, step := range w.Jobs[job].Steps {
			where := fmt.Sprintf("%s: job %q step %d", path, job, i+1)
			if step.Name != "" {
				where = fmt.Sprintf("%s: job %q step %q", path, job, step.Name)
			}

			// The action we actually use.  `push: true` is what turns a
			// build into a distribution.
			if strings.HasPrefix(step.Uses, "docker/build-push-action@") {
				if !truthy(step.With["push"]) {
					continue
				}
				ctx, _ := step.With["context"].(string)
				if ctx == "" {
					v = append(v, fmt.Sprintf("%s pushes an image but names no `context`, so nothing here can tell what it publishes", where))
					continue
				}
				marker := filepath.Join(repoRoot, filepath.FromSlash(ctx), MarkerName)
				if _, err := os.Stat(marker); err == nil {
					v = append(v, fmt.Sprintf("%s pushes %s, which carries %s — that image may not be redistributed; build it on the host that runs it (see %s/%s)",
						where, ctx, MarkerName, ctx, MarkerName))
				}
				continue
			}

			// The back door.  build-push-action is the sanctioned path, so a
			// raw `docker push` in a shell step is either a mistake or a way
			// around the check above; either way it wants a human.
			if step.Run != "" && strings.Contains(step.Run, "docker push") {
				v = append(v, fmt.Sprintf("%s runs `docker push` directly — publishing goes through docker/build-push-action, where this lint can see the context being published", where))
			}
		}
	}
	return v, nil
}

// CheckDir lints every workflow in dir.
func CheckDir(repoRoot, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var v []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		got, err := Check(repoRoot, filepath.ToSlash(filepath.Join(filepath.Base(dir), e.Name())), data)
		if err != nil {
			return nil, err
		}
		v = append(v, got...)
	}
	return v, nil
}

// MarkedContexts lists the build contexts under root that declare
// themselves unpublishable, so the command can report what it is guarding.
// A guard that names nothing has usually stopped working.
func MarkedContexts(root, dockerDir string) ([]string, error) {
	var found []string
	base := filepath.Join(root, dockerDir)
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == MarkerName {
			rel, rerr := filepath.Rel(root, filepath.Dir(path))
			if rerr != nil {
				return rerr
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// truthy reads the several shapes YAML admits for a boolean `with:` value —
// `push: true` and `push: "true"` mean the same thing to Actions, and a
// linter that only understood one of them would be trivially evaded by
// accident.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		// An expression (`${{ ... }}`) cannot be evaluated here, so treat it
		// as a push: refusing a marked context that MIGHT publish is the
		// safe direction to be wrong in.
		return s == "true" || s == "yes" || s == "on" || strings.Contains(s, "${{")
	default:
		return false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
