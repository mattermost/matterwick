// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"fmt"
	"path"
	"strings"
)

// E2E repo profiles. Every E2E flow (PR label, push to the default branch, PR-close cleanup,
// the stale scan, CMT) looks up the repo's profile instead of branching on the repo name, so
// all that differs between repos is here: the server slots, the workflow, and its inputs.
type e2eProfile struct {
	// kind is the instance type and DNS prefix: <kind>-pr-<N>-* for PRs, <kind>-push-* for
	// pushes. Kinds must not prefix each other, so DNS LIKE patterns stay disjoint.
	kind string
	// platforms are the server slots; each becomes E2EInstance.Platform.
	platforms []string
	// workflow is the dispatched workflow file. Its completed push runs are matched by it.
	workflow string
	// cmt enables compatibility-matrix testing.
	cmt bool
	// inputs fills the workflow's declared workflow_dispatch inputs from exactly one instance
	// per slot. GitHub rejects undeclared inputs with 422 and allows at most 10.
	inputs func(s *Server, run e2eRun, instances []*E2EInstance) (map[string]interface{}, error)
}

// oxideRepoName is matched exactly: its name also contains "mobile".
const oxideRepoName = "mattermost-mobile-oxide"

var mobileE2EPlatforms = []string{"android-site-1", "android-site-2", "ios-site-1", "ios-site-2", "site-3"}

// oxideE2EPlatforms: three servers per mobile OS. Oxide's Maestro shards share them, each
// shard as its own user/team.
var oxideE2EPlatforms = []string{"android-site-1", "android-site-2", "android-site-3", "ios-site-1", "ios-site-2", "ios-site-3"}

var (
	desktopE2EProfile = &e2eProfile{
		kind:      "desktop",
		platforms: []string{"linux", "macos", "windows"},
		workflow:  "e2e-functional.yml",
		cmt:       true,
		inputs:    desktopE2EInputs,
	}
	mobileE2EProfile = &e2eProfile{
		kind:      "mobile",
		platforms: mobileE2EPlatforms,
		workflow:  "e2e-detox-pr.yml",
		cmt:       true,
		inputs:    mobileE2EInputs,
	}
	oxideE2EProfile = &e2eProfile{
		kind:      "oxide",
		platforms: oxideE2EPlatforms,
		workflow:  "e2e-matterwick.yml",
		inputs:    oxideE2EInputs,
	}
	e2eProfiles = []*e2eProfile{desktopE2EProfile, mobileE2EProfile, oxideE2EProfile}
)

// e2eProfileFor returns repoName's profile, or nil if the repo has no E2E.
func e2eProfileFor(repoName string) *e2eProfile {
	switch {
	case strings.EqualFold(repoName, oxideRepoName):
		return oxideE2EProfile
	case strings.Contains(repoName, "desktop"):
		return desktopE2EProfile
	case strings.Contains(repoName, "mobile"):
		return mobileE2EProfile
	}
	return nil
}

// e2eEnabled reports whether p may provision servers. Cleanup never checks it, so servers
// created before a profile was turned off are still removed.
func (s *Server) e2eEnabled(p *e2eProfile) bool {
	return p != oxideE2EProfile || s.Config.E2EOxideEnabled
}

// mobileServerKind reports whether kind's servers get the mobile baseline: mobile env,
// mobile admin secret, and Shared Channels on at boot.
func mobileServerKind(kind string) bool {
	return kind == mobileE2EProfile.kind || kind == oxideE2EProfile.kind
}

// e2eRun is one dispatch: a PR label (prNumber > 0) or a push to the default branch.
type e2eRun struct {
	owner, repo, ref, sha string
	prNumber              int
	platform              string // ios, android or both; desktop ignores it
}

// key is the run's e2eInstances key.
func (r e2eRun) key() string {
	if r.prNumber > 0 {
		return fmt.Sprintf("%s-pr-%d", r.repo, r.prNumber)
	}
	return pushRunKey(r.repo, r.ref, r.sha)
}

// pushRunKey is a push run's e2eInstances key. Completion cleanup rebuilds it from the
// finished run's head_branch and head_sha, so it only ever matches that run.
func pushRunKey(repo, branch, sha string) string {
	return fmt.Sprintf("%s-push-%s-%s", repo, branch, sha)
}

// nameToken goes between kind and platform in server names. Push servers never contain
// "-pr-", so they never match a PR's pattern and the stale scan gives them the non-PR max age.
func (r e2eRun) nameToken() string {
	if r.prNumber > 0 {
		return fmt.Sprintf("pr-%d", r.prNumber)
	}
	return "push"
}

// workflowFileOf returns the file name of a workflow_run "path"
// (".github/workflows/x.yml", possibly with an "@ref" suffix).
func workflowFileOf(workflowPath string) string {
	return path.Base(strings.SplitN(workflowPath, "@", 2)[0])
}

// urlInputs maps each slot's server URL to its input: "android-site-1" -> ANDROID_SITE_1_URL.
// dispatchE2E has already checked there is one instance per slot.
func urlInputs(platforms []string, instances []*E2EInstance) (map[string]interface{}, error) {
	byPlatform := make(map[string]string, len(instances))
	for _, inst := range instances {
		byPlatform[inst.Platform] = inst.URL
	}
	inputs := make(map[string]interface{}, len(platforms))
	for _, p := range platforms {
		u, ok := byPlatform[p]
		if !ok {
			return nil, fmt.Errorf("E2E missing instance for platform %s", p)
		}
		inputs[strings.ToUpper(strings.ReplaceAll(p, "-", "_"))+"_URL"] = u
	}
	return inputs, nil
}

// runTypeInputs: desktop and mobile workflows take pr_number for PR runs and run_type=MASTER
// for default-branch runs.
func runTypeInputs(run e2eRun, inputs map[string]interface{}) map[string]interface{} {
	if run.prNumber > 0 {
		inputs["pr_number"] = fmt.Sprintf("%d", run.prNumber)
	} else {
		inputs["run_type"] = "MASTER"
	}
	return inputs
}

func desktopE2EInputs(s *Server, run e2eRun, instances []*E2EInstance) (map[string]interface{}, error) {
	details, err := s.buildInstanceDetailsJSON(instances)
	if err != nil {
		return nil, err
	}
	return runTypeInputs(run, map[string]interface{}{
		"instance_details":  details,
		"version_name":      run.ref,
		"MM_TEST_USER_NAME": s.Config.E2EUsername,
		"MM_SERVER_VERSION": instances[0].ServerVersion,
	}), nil
}

func mobileE2EInputs(_ *Server, run e2eRun, instances []*E2EInstance) (map[string]interface{}, error) {
	inputs, err := urlInputs(mobileE2EPlatforms, instances)
	if err != nil {
		return nil, err
	}
	inputs["MOBILE_VERSION"] = run.sha
	inputs["PLATFORM"] = run.platform
	return runTypeInputs(run, inputs), nil
}

// oxideE2EInputs: an empty pr_number marks a default-branch run (statuses on the commit, no
// label, queued runs). The workflow rejects a MOBILE_VERSION other than the ref's commit.
func oxideE2EInputs(_ *Server, run e2eRun, instances []*E2EInstance) (map[string]interface{}, error) {
	inputs, err := urlInputs(oxideE2EPlatforms, instances)
	if err != nil {
		return nil, err
	}
	inputs["MOBILE_VERSION"] = run.sha
	inputs["PLATFORM"] = run.platform
	inputs["pr_number"] = ""
	if run.prNumber > 0 {
		inputs["pr_number"] = fmt.Sprintf("%d", run.prNumber)
	}
	return inputs, nil
}
