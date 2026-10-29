// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v32/github"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCloud records every request made through Server.CloudClient.
type fakeCloud struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (f *fakeCloud) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
}

// dnsQueries returns the dns_name filter of every GET /api/installations call.
func (f *fakeCloud) dnsQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if r.Method == http.MethodGet && r.URL.Path == "/api/installations" {
			out = append(out, r.URL.Query().Get("dns_name"))
		}
	}
	return out
}

func (f *fakeCloud) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// newFakeCloudServer serves GET /api/installations with listJSON and records everything else.
func newFakeCloudServer(t *testing.T, listJSON string) (*Server, *fakeCloud) {
	t.Helper()
	fc := &fakeCloud{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.record(r)
		if r.Method == http.MethodGet && r.URL.Path == "/api/installations" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(listJSON))
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// The servers makeE2EInstances tracks are up; "cold-" ones hibernate.
		if id, ok := strings.CutPrefix(r.URL.Path, "/api/installation/"); ok && r.Method == http.MethodGet {
			state := "stable"
			if strings.HasPrefix(id, "cold-") {
				state = "hibernating"
			}
			_, _ = w.Write([]byte(`{"ID":"` + id + `","State":"` + state + `"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	s := &Server{
		Config: &MatterwickConfig{
			DNSNameTestServer: "test.mattermost.cloud",
			E2ELabel:          "E2E/Run",
			E2EMobileIOSLabel: "E2E/Run-iOS",
			E2EUsername:       "e2eadmin",
			E2EPassword:       "e2epassword",
		},
		CloudClient:  model.NewCloudClient(ts.URL, "", "", "", ""),
		Logger:       logrus.New(),
		e2eInstances: make(map[string][]*E2EInstance),
	}
	return s, fc
}

func makeOxideInstances() []*E2EInstance {
	var out []*E2EInstance
	for i, p := range oxideE2EPlatforms {
		out = append(out, &E2EInstance{
			Name:           "oxide-pr-42-" + p,
			Platform:       p,
			URL:            "https://" + p + ".test.example.com",
			InstallationID: "id-" + string(rune('a'+i)),
			ServerVersion:  "11.9.0",
		})
	}
	return out
}

func TestE2EProfileFor(t *testing.T) {
	for _, tt := range []struct {
		repo string
		want *e2eProfile
	}{
		// Oxide is matched by exact name before the "mobile" substring: otherwise an Oxide
		// event would provision, dispatch to and clean up mattermost-mobile's servers.
		{"mattermost-mobile-oxide", oxideE2EProfile},
		{"Mattermost-Mobile-Oxide", oxideE2EProfile},
		{"mattermost-mobile", mobileE2EProfile},
		{"desktop", desktopE2EProfile},
		{"mattermost-server", nil},
	} {
		t.Run(tt.repo, func(t *testing.T) {
			assert.Same(t, tt.want, e2eProfileFor(tt.repo))
		})
	}

	// DNS LIKE patterns ("<kind>-%") must stay disjoint across profiles.
	for _, a := range e2eProfiles {
		for _, b := range e2eProfiles {
			if a != b {
				assert.False(t, strings.HasPrefix(a.kind, b.kind), "%s must not start with %s", a.kind, b.kind)
			}
		}
	}
}

func instancesFor(platforms []string) []*E2EInstance {
	var out []*E2EInstance
	for i, p := range platforms {
		out = append(out, &E2EInstance{Platform: p, URL: "https://" + p + ".test", InstallationID: fmt.Sprintf("id-%d", i), ServerVersion: "11.9.0"})
	}
	return out
}

// TestDispatchE2E pins every profile's dispatch for PR and push runs: the workflow, the ref,
// and the exact input set (GitHub rejects undeclared inputs with 422 and allows at most 10).
func TestDispatchE2E(t *testing.T) {
	pr := e2eRun{owner: "mattermost", ref: "feature-x", sha: "prsha", prNumber: 42, platform: "ios"}
	push := e2eRun{owner: "mattermost", ref: "main", sha: "pushsha", platform: "both"}
	reversed := instancesFor(oxideE2EPlatforms)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	mobileURLs := map[string]interface{}{
		"ANDROID_SITE_1_URL": "https://android-site-1.test", "ANDROID_SITE_2_URL": "https://android-site-2.test",
		"IOS_SITE_1_URL": "https://ios-site-1.test", "IOS_SITE_2_URL": "https://ios-site-2.test", "SITE_3_URL": "https://site-3.test",
	}
	oxideURLs := map[string]interface{}{
		"ANDROID_SITE_1_URL": "https://android-site-1.test", "ANDROID_SITE_2_URL": "https://android-site-2.test", "ANDROID_SITE_3_URL": "https://android-site-3.test",
		"IOS_SITE_1_URL": "https://ios-site-1.test", "IOS_SITE_2_URL": "https://ios-site-2.test", "IOS_SITE_3_URL": "https://ios-site-3.test",
	}
	with := func(base map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	desktopDetails := `[{"platform":"linux","runner":"ubuntu-latest","url":"https://linux.test","installation-id":"id-0","server_version":"11.9.0"},` +
		`{"platform":"macos","runner":"macos-latest","url":"https://macos.test","installation-id":"id-1","server_version":"11.9.0"},` +
		`{"platform":"windows","runner":"windows-2022","url":"https://windows.test","installation-id":"id-2","server_version":"11.9.0"}]`

	for _, tt := range []struct {
		name      string
		profile   *e2eProfile
		run       e2eRun
		instances []*E2EInstance
		want      map[string]interface{}
		wantErr   string
	}{
		{"desktop PR", desktopE2EProfile, pr, instancesFor(desktopE2EProfile.platforms), map[string]interface{}{
			"instance_details": desktopDetails, "version_name": "feature-x", "MM_TEST_USER_NAME": "e2eadmin", "MM_SERVER_VERSION": "11.9.0", "pr_number": "42"}, ""},
		{"desktop push", desktopE2EProfile, push, instancesFor(desktopE2EProfile.platforms), map[string]interface{}{
			"instance_details": desktopDetails, "version_name": "main", "MM_TEST_USER_NAME": "e2eadmin", "MM_SERVER_VERSION": "11.9.0", "run_type": "MASTER"}, ""},
		{"mobile PR", mobileE2EProfile, pr, instancesFor(mobileE2EPlatforms),
			with(mobileURLs, map[string]interface{}{"MOBILE_VERSION": "prsha", "PLATFORM": "ios", "pr_number": "42"}), ""},
		{"mobile push", mobileE2EProfile, push, instancesFor(mobileE2EPlatforms),
			with(mobileURLs, map[string]interface{}{"MOBILE_VERSION": "pushsha", "PLATFORM": "both", "run_type": "MASTER"}), ""},
		{"oxide PR, instances in any order", oxideE2EProfile, pr, reversed,
			with(oxideURLs, map[string]interface{}{"MOBILE_VERSION": "prsha", "PLATFORM": "ios", "pr_number": "42"}), ""},
		{"oxide push: empty pr_number marks a default-branch run", oxideE2EProfile, push, instancesFor(oxideE2EPlatforms),
			with(oxideURLs, map[string]interface{}{"MOBILE_VERSION": "pushsha", "PLATFORM": "both", "pr_number": ""}), ""},
		{"missing a server", mobileE2EProfile, pr, instancesFor(mobileE2EPlatforms)[:4], nil, "mobile E2E requires exactly 5 instances, got 4"},
		{"wrong slot", oxideE2EProfile, pr, instancesFor(append([]string{"site-3"}, oxideE2EPlatforms[1:]...)), nil, "E2E missing instance for platform android-site-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newDryRunServer(t, "", "mattermost")
			gh, captures := mockGitHubServer(t, http.StatusNoContent)
			s.githubAPIBase = gh.URL + "/"
			run := tt.run
			run.repo = "repo-under-test"

			err := s.dispatchE2E(tt.profile, run, tt.instances)

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Empty(t, *captures)
				return
			}
			require.NoError(t, err)
			require.Len(t, *captures, 1)
			c := (*captures)[0]
			assert.Equal(t, tt.profile.workflow, c.Workflow)
			assert.Equal(t, "repo-under-test", c.Repo)
			assert.Equal(t, run.ref, c.Ref)
			assert.Equal(t, tt.want, c.Inputs)
			assert.LessOrEqual(t, len(c.Inputs), 10)
		})
	}
}

func TestOxideInstanceNamesFitAndParse(t *testing.T) {
	const dns = "test.mattermost.cloud"
	// Longest realistic name: 5-digit PR number, longest platform token.
	full := strings.Join([]string{oxideE2EProfile.kind, "pr-99999", "android-site-3", "abcdef12"}, "-")
	assert.Equal(t, full, e2eInstanceName(dns, oxideE2EProfile.kind, "pr-99999", "android-site-3", "abcdef12"),
		"oxide instance names must not be truncated, or the platform suffix becomes unparseable")

	// Reuse path: OwnerIDs come back in arbitrary order and must map to every platform.
	var list []map[string]string
	for i := len(oxideE2EPlatforms) - 1; i >= 0; i-- {
		p := oxideE2EPlatforms[i]
		list = append(list, map[string]string{
			"ID":      "inst-" + p,
			"OwnerID": "oxide-pr-12-" + p + "-1a2b3c4d",
			"State":   "stable",
			"Version": "11.9.0",
		})
	}
	listJSON, err := json.Marshal(list)
	require.NoError(t, err)

	s, fc := newFakeCloudServer(t, string(listJSON))
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile-oxide", Number: 12}
	found, err := s.findExistingE2EInstancesInCloud(pr, oxideE2EProfile)
	require.NoError(t, err)
	require.Len(t, found, len(oxideE2EPlatforms))
	for i, p := range oxideE2EPlatforms {
		assert.Equal(t, p, found[i].Platform)
		assert.Equal(t, "inst-"+p, found[i].InstallationID)
		assert.Equal(t, "https://oxide-pr-12-"+p+"-1a2b3c4d.test.mattermost.cloud", found[i].URL)
	}
	assert.Equal(t, []string{"oxide-pr-12-%"}, fc.dnsQueries())
}

func TestCleanupOrphanedE2EInstances_PatternsPerRepo(t *testing.T) {
	for _, tt := range []struct {
		repo string
		want string
	}{
		// Regression: before the Oxide profile, an Oxide PR close matched "mobile" and
		// queried (then deleted) mattermost-mobile's servers for the same PR number.
		{"mattermost-mobile-oxide", "oxide-pr-42-%"},
		{"mattermost-mobile", "mobile-pr-42-%"},
		{"mattermost-desktop", "desktop-pr-42-%"},
	} {
		t.Run(tt.repo, func(t *testing.T) {
			s, fc := newFakeCloudServer(t, "[]")
			s.Config.E2EOxideEnabled = false // cleanup must run regardless of the flag
			pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: tt.repo, Number: 42}
			s.cleanupOrphanedE2EInstances(pr, s.Logger)
			assert.Equal(t, []string{tt.want}, fc.dnsQueries())
		})
	}
}

func TestCleanupStaleE2EInstances_IncludesOxide(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.cleanupStaleE2EInstances()
	assert.Equal(t, []string{"desktop-%", "mobile-%", "oxide-%"}, fc.dnsQueries())
}

func TestHandleE2ETestRequest_OxideDisabledDoesNothing(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.Config.E2EOxideEnabled = false
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile-oxide", Number: 7, Ref: "b", Sha: "s"}

	s.handleE2ETestRequest(pr, "E2E/Run")

	assert.Zero(t, fc.count(), "no cloud calls may be made for Oxide while E2EOxideEnabled=false")
	assert.Empty(t, s.e2eInstances)
}

func TestGetE2EPassword_OxideUsesMobileSecret(t *testing.T) {
	t.Setenv("MM_MOBILE_E2E_ADMIN_PASSWORD", "mobile-secret")
	t.Setenv("MM_DESKTOP_E2E_USER_CREDENTIALS", "desktop-secret")
	s := &Server{Config: &MatterwickConfig{}, Logger: logrus.New()}

	assert.Equal(t, "mobile-secret", s.getE2EPassword(oxideE2EProfile.kind))
	assert.Equal(t, "mobile-secret", s.getE2EPassword("mobile"))
	assert.Equal(t, "desktop-secret", s.getE2EPassword("desktop"))
}

func TestCreateCloudInstallation_OxideEnvMatchesMobile(t *testing.T) {
	priorityEnv := func(instanceType string) map[string]interface{} {
		s, fc := newFakeCloudServer(t, "[]")
		_, err := s.createCloudInstallation(context.Background(), instanceType+"-pr-1-x-abcdef12", "11.9.0", "admin", "pw", instanceType, s.Logger)
		require.Error(t, err) // fake cloud rejects the create; we only need the request body
		for i, r := range fc.requests {
			if r.Method == http.MethodPost && r.URL.Path == "/api/installations" {
				var req struct {
					PriorityEnv map[string]interface{}
				}
				require.NoError(t, json.Unmarshal(fc.bodies[i], &req))
				return req.PriorityEnv
			}
		}
		t.Fatalf("no create request for %s", instanceType)
		return nil
	}

	mobile := priorityEnv("mobile")
	require.NotEmpty(t, mobile)
	assert.Equal(t, mobile, priorityEnv(oxideE2EProfile.kind), "Oxide must get exactly the mobile server baseline")
	assert.NotEqual(t, mobile, priorityEnv("desktop"))
}

func pushEvent(repo, ref, defaultBranch, sha string, deleted bool) *gogithub.PushEvent {
	return &gogithub.PushEvent{
		Ref:        &ref,
		Deleted:    &deleted,
		Repo:       &gogithub.PushEventRepository{Name: &repo, DefaultBranch: &defaultBranch},
		HeadCommit: &gogithub.HeadCommit{ID: &sha},
	}
}

func TestPushE2ESkipReason(t *testing.T) {
	for _, tt := range []struct {
		name          string
		repo          string
		oxideEnabled  bool
		autoTrigger   bool
		branch        string
		defaultBranch string
		deleted       bool
		want          string
	}{
		{"oxide default branch runs", oxideRepoName, true, true, "main", "main", false, ""},
		{"mobile default branch runs", "mattermost-mobile", false, true, "main", "main", false, ""},
		{"desktop master default branch runs", "desktop", false, true, "master", "master", false, ""},
		{"no profile", "mattermost-server", true, true, "master", "master", false, "repository has no E2E profile"},
		{"oxide disabled", oxideRepoName, false, true, "main", "main", false, "E2E is disabled for this repository"},
		{"auto-trigger disabled", "mattermost-mobile", true, false, "main", "main", false, "push E2E is disabled (E2EAutoTriggerOnMaster=false)"},
		{"branch deleted", oxideRepoName, true, true, "main", "main", true, "branch was deleted"},
		{"feature branch", "mattermost-mobile", true, true, "feature-x", "main", false, "push is not to the default branch"},
		{"master that is not the default branch", oxideRepoName, true, true, "master", "main", false, "push is not to the default branch"},
		{"default branch unknown", oxideRepoName, true, true, "main", "", false, "push is not to the default branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{Config: &MatterwickConfig{E2EOxideEnabled: tt.oxideEnabled, E2EAutoTriggerOnMaster: tt.autoTrigger}}
			event := pushEvent(tt.repo, "refs/heads/"+tt.branch, tt.defaultBranch, "abc", tt.deleted)
			assert.Equal(t, tt.want, s.pushE2ESkipReason(e2eProfileFor(tt.repo), event, tt.branch))
		})
	}
}

func TestPushInstanceNames(t *testing.T) {
	const dns = "test.mattermost.cloud"
	for _, p := range e2eProfiles {
		for _, platform := range p.platforms {
			name := e2eInstanceName(dns, p.kind, e2eRun{}.nameToken(), platform, "abcdef12")
			assert.Equal(t, strings.Join([]string{p.kind, "push", platform, "abcdef12"}, "-"), name, "push names must not be truncated")
			assert.NotContains(t, name, "-pr-", "push servers must use the non-PR max age and never match <kind>-pr-<N>-%")
		}
	}
}

// fakePushGitHub serves the commits lookup (HEAD sha, or headStatus) and records dispatches.
func fakePushGitHub(t *testing.T, head string, headStatus, dispatchStatus int) (*httptest.Server, *[]dispatchCapture) {
	t.Helper()
	dispatch, captures := mockGitHubServer(t, dispatchStatus)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") {
			w.WriteHeader(headStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": head})
			return
		}
		dispatch.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, captures
}

func deletedInstallationIDs(fc *fakeCloud) []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var ids []string
	for _, r := range fc.requests {
		if r.Method == http.MethodDelete {
			ids = append(ids, strings.TrimPrefix(r.URL.Path, "/api/installation/"))
		}
	}
	sort.Strings(ids)
	return ids
}

func instanceIDs(instances []*E2EInstance) []string {
	var ids []string
	for _, inst := range instances {
		ids = append(ids, inst.InstallationID)
	}
	sort.Strings(ids)
	return ids
}

func TestDispatchPushRun(t *testing.T) {
	const pushSHA = "1111111"
	run := e2eRun{owner: "mattermost", repo: oxideRepoName, ref: "main", sha: pushSHA, platform: "both"}
	key := run.key()

	for _, tt := range []struct {
		name           string
		head           string
		headStatus     int
		dispatchStatus int
		wantDispatch   bool
		wantTracked    bool
		wantDestroyed  bool
	}{
		{"head is the push: tracked and dispatched", pushSHA, http.StatusOK, http.StatusNoContent, true, true, false},
		{"head lookup fails: dispatched with the push sha", "", http.StatusInternalServerError, http.StatusNoContent, true, true, false},
		{"branch moved on: superseded, servers destroyed", "2222222", http.StatusOK, http.StatusNoContent, false, false, true},
		{"dispatch fails: untracked, servers destroyed", pushSHA, http.StatusOK, http.StatusInternalServerError, true, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, fc := newFakeCloudServer(t, "[]")
			s.Config.Org = "mattermost"
			gh, captures := fakePushGitHub(t, tt.head, tt.headStatus, tt.dispatchStatus)
			s.githubAPIBase = gh.URL + "/"
			instances := makeOxideInstances()

			s.dispatchPushRun(oxideE2EProfile, run, instances, s.Logger)

			if tt.wantDispatch {
				require.Len(t, *captures, 1)
				c := (*captures)[0]
				assert.Equal(t, oxideE2EProfile.workflow, c.Workflow)
				assert.Equal(t, "main", c.Ref)
				assert.Equal(t, pushSHA, c.Inputs["MOBILE_VERSION"], "the run must test the pushed commit")
			} else {
				assert.Empty(t, *captures)
			}

			if tt.wantTracked {
				assert.Equal(t, map[string][]*E2EInstance{key: instances}, s.e2eInstances)
			} else {
				assert.Empty(t, s.e2eInstances)
			}

			if tt.wantDestroyed {
				assert.Equal(t, instanceIDs(instances), deletedInstallationIDs(fc))
			} else {
				assert.Empty(t, deletedInstallationIDs(fc))
			}
		})
	}
}

func TestHandleWorkflowRunEvent_PushCleanup(t *testing.T) {
	for _, tt := range []struct {
		name       string
		repo       string
		workflow   string // run name
		path       string
		action     string
		headBranch string
		wantGone   bool   // the run's own push servers are destroyed
		event      string // defaults to workflow_dispatch
	}{
		{"manual run of the same workflow on the same commit keeps them", oxideRepoName, "maestro", ".github/workflows/e2e-matterwick.yml", "completed", "main", false, "push"},
		{"oxide main run completed", oxideRepoName, "maestro", ".github/workflows/e2e-matterwick.yml", "completed", "main", true, ""},
		{"path with ref suffix", oxideRepoName, "maestro", ".github/workflows/e2e-matterwick.yml@refs/heads/main", "completed", "main", true, ""},
		{"mobile main run completed", "mattermost-mobile", "E2E", ".github/workflows/e2e-detox-pr.yml", "completed", "main", true, ""},
		{"desktop master run completed", "desktop", "Electron Playwright Tests", ".github/workflows/e2e-functional.yml", "completed", "master", true, ""},
		{"PR run on the same commit keeps main's servers", oxideRepoName, "maestro", ".github/workflows/e2e-matterwick.yml", "completed", "feature-x", false, ""},
		{"run still in progress", oxideRepoName, "maestro", ".github/workflows/e2e-matterwick.yml", "in_progress", "main", false, ""},
		{"another workflow", oxideRepoName, "ci", ".github/workflows/ci.yml", "completed", "main", false, ""},
		{"another repo's workflow file", "mattermost-mobile", "maestro", ".github/workflows/e2e-matterwick.yml", "completed", "main", false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.event == "" {
				tt.event = "workflow_dispatch"
			}
			s, fc := newFakeCloudServer(t, "[]")
			doneKey := pushRunKey(tt.repo, "main", "sha1")
			if tt.headBranch == "master" {
				doneKey = pushRunKey(tt.repo, "master", "sha1")
			}
			otherPushKey := pushRunKey(tt.repo, "main", "sha2")
			prKey := tt.repo + "-pr-5"
			s.e2eInstances[doneKey] = []*E2EInstance{{InstallationID: "done"}}
			s.e2eInstances[otherPushKey] = []*E2EInstance{{InstallationID: "other-push"}}
			s.e2eInstances[prKey] = []*E2EInstance{{InstallationID: "pr"}}

			s.handleWorkflowRunEventWithInputs(&WorkflowRunWebhookPayload{
				Action:      tt.action,
				WorkflowRun: WorkflowRunWithInputs{ID: 1, Name: tt.workflow, HeadBranch: tt.headBranch, HeadSHA: "sha1", Path: tt.path, Event: tt.event},
				Repository:  map[string]interface{}{"name": tt.repo, "owner": map[string]interface{}{"login": "mattermost"}},
			})

			assert.Contains(t, s.e2eInstances, otherPushKey)
			assert.Contains(t, s.e2eInstances, prKey)
			if tt.wantGone {
				assert.NotContains(t, s.e2eInstances, doneKey)
				assert.Equal(t, []string{"done"}, deletedInstallationIDs(fc))
			} else {
				assert.Contains(t, s.e2eInstances, doneKey)
				assert.Empty(t, deletedInstallationIDs(fc))
			}
		})
	}
}

func TestParseWorkflowRunEvent_Path(t *testing.T) {
	payload, err := ParseWorkflowRunEventWithInputs(strings.NewReader(
		`{"action":"completed","workflow_run":{"name":"maestro","head_branch":"main","head_sha":"abc","path":".github/workflows/e2e-matterwick.yml"}}`))
	require.NoError(t, err)
	assert.Equal(t, oxideE2EProfile.workflow, workflowFileOf(payload.WorkflowRun.Path))
}

func TestHandleCMTTrigger_SkipsOxide(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.Config.E2EOxideEnabled = true

	logger, hook := test.NewNullLogger()

	s.handleCMTTrigger("mattermost", "mattermost-mobile-oxide", "main", "abc", 1, logger)

	assert.Zero(t, fc.count(), "CMT must never provision servers for Oxide")
	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, "Repository has no CMT, skipping CMT trigger", hook.LastEntry().Message)
}

func TestLockE2EPRProvisioning(t *testing.T) {
	s := &Server{}
	const prKey = "mattermost-mobile-oxide-pr-9"

	unlockFirst := s.lockE2EPRProvisioning(prKey)

	// A second request for the same PR (e.g. E2E/Run-iOS while E2E/Run-Android provisions)
	// must wait until the first one has stored its instances.
	acquired := make(chan func())
	go func() { acquired <- s.lockE2EPRProvisioning(prKey) }()
	select {
	case <-acquired:
		t.Fatal("second request for the same PR acquired the provisioning lock concurrently")
	case <-time.After(100 * time.Millisecond):
	}

	// Other PRs are never blocked by it.
	otherDone := make(chan struct{})
	go func() {
		s.lockE2EPRProvisioning("mattermost-mobile-oxide-pr-10")()
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("a different PR was blocked by another PR's provisioning lock")
	}

	unlockFirst()
	select {
	case unlockSecond := <-acquired:
		unlockSecond()
	case <-time.After(time.Second):
		t.Fatal("second request never acquired the lock after the first released it")
	}
}

func TestStartProvision(t *testing.T) {
	s := &Server{}
	older, olderDone := s.startProvision("oxide/main")
	other, otherDone := s.startProvision("oxide/release-1")
	newer, newerDone := s.startProvision("oxide/main")
	defer otherDone()

	assert.Error(t, older.Err(), "a newer push to the same branch stops the older one's provisioning")
	assert.NoError(t, other.Err(), "other branches are unaffected")
	assert.NoError(t, newer.Err())

	olderDone() // the older run finishing must not unregister the newer one
	s.cancelProvision("oxide/main")
	assert.Error(t, newer.Err(), "the newer run was still registered, so cancelling the key stopped it")
	newerDone()

	t.Run("PR cleanup stops the PR's provisioning", func(t *testing.T) {
		s, _ := newFakeCloudServer(t, "[]")
		s.e2ePRCleanupGeneration = map[string]int64{}
		pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: oxideRepoName, Number: 9}
		ctx, done := s.startProvision(oxideRepoName + "-pr-9")
		defer done()
		s.handleE2ECleanup(pr)
		assert.Error(t, ctx.Err())
	})
}

func TestHandlePushEventE2E_SkipsSupersededPushBeforeProvisioning(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.Config.Org = "mattermost"
	gh, captures := fakePushGitHub(t, "newer-sha", http.StatusOK, http.StatusNoContent)
	s.githubAPIBase = gh.URL + "/"

	s.handlePushEventE2E(oxideE2EProfile, pushEvent(oxideRepoName, "refs/heads/main", "main", "older-sha", false), "main")

	assert.Zero(t, fc.count(), "no servers may be created for a push the branch has already moved past")
	assert.Empty(t, *captures)
}

func TestCleanupStaleE2EInstances_KeepsServersOfActiveRuns(t *testing.T) {
	sevenHoursAgo := time.Now().Add(-7 * time.Hour).UnixMilli()
	list := fmt.Sprintf(`[{"ID":"owned","OwnerID":"oxide-push-ios-site-1-aaaaaaaa","State":"stable","CreateAt":%d},`+
		`{"ID":"unowned","OwnerID":"oxide-push-ios-site-2-bbbbbbbb","State":"stable","CreateAt":%d}]`, sevenHoursAgo, sevenHoursAgo)
	run := e2eRun{owner: "mattermost", repo: oxideRepoName, ref: "main", sha: "abc"}

	for _, tt := range []struct {
		name        string
		runsStatus  int
		runsJSON    string
		wantDeleted []string
	}{
		{"run still queued: its server is kept", http.StatusOK, `{"workflow_runs":[{"status":"queued"}]}`, []string{"unowned"}},
		{"run finished: its server is removed", http.StatusOK, `{"workflow_runs":[{"status":"completed"}]}`, []string{"owned", "unowned"}},
		{"run state unknown: its server is kept", http.StatusInternalServerError, `{}`, []string{"unowned"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The scan queries each kind's name pattern; only "oxide-%" matches these servers.
			s, fc := newFakeCloudServer(t, "[]")
			oxide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fc.record(r)
				switch {
				case r.Method == http.MethodGet && r.URL.Query().Get("dns_name") == "oxide-%":
					_, _ = w.Write([]byte(list))
				case r.Method == http.MethodGet:
					_, _ = w.Write([]byte("[]"))
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			t.Cleanup(oxide.Close)
			s.CloudClient = model.NewCloudClient(oxide.URL, "", "", "", "")
			base, _ := newFakeGitHub(t, tt.runsStatus, tt.runsJSON)
			s.githubAPIBase = base
			s.e2eInstances[run.key()] = []*E2EInstance{{InstallationID: "owned"}}
			s.e2eTrackedRuns = map[string]e2eRun{run.key(): run}

			s.cleanupStaleE2EInstances()

			assert.Equal(t, tt.wantDeleted, deletedInstallationIDs(fc))
			_, stillTracked := s.e2eInstances[run.key()]
			assert.Equal(t, len(tt.wantDeleted) == 1, stillTracked, "a removed run's tracking is dropped with it")
		})
	}
}

// The user's case: a PR run that starts late must never lose its servers to an age limit,
// and idle PR servers must not linger for hours.
func TestCleanupStaleE2EInstances_PRServersGoWhenIdleNotByAge(t *testing.T) {
	nineHoursAgo := time.Now().Add(-9 * time.Hour).UnixMilli() // older than E2EPRInstanceMaxAge (8h)
	twoHoursAgo := time.Now().Add(-2 * time.Hour).UnixMilli()
	run := e2eRun{owner: "mattermost", repo: oxideRepoName, ref: "feature-x", sha: "abc", prNumber: 5}

	for _, tt := range []struct {
		name         string
		createdAt    int64
		runsJSON     string
		dispatchedAt time.Duration // ago
		wantDeleted  bool
	}{
		{"run in progress on 9h-old servers: kept", nineHoursAgo, `{"workflow_runs":[{"status":"in_progress"}]}`, 3 * time.Hour, false},
		{"no run, last dispatch 3h ago: idle, removed", twoHoursAgo, `{"workflow_runs":[{"status":"completed"}]}`, 3 * time.Hour, true},
		{"no run, dispatched 10m ago: kept for a re-run", twoHoursAgo, `{"workflow_runs":[{"status":"completed"}]}`, 10 * time.Minute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			list := fmt.Sprintf(`[{"ID":"pr-server","OwnerID":"oxide-pr-5-ios-site-1-aaaaaaaa","State":"stable","CreateAt":%d}]`, tt.createdAt)
			s, fc := newFakeCloudServer(t, "[]")
			oxide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fc.record(r)
				switch {
				case r.Method == http.MethodGet && r.URL.Query().Get("dns_name") == "oxide-%":
					_, _ = w.Write([]byte(list))
				case r.Method == http.MethodGet:
					_, _ = w.Write([]byte("[]"))
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			t.Cleanup(oxide.Close)
			s.CloudClient = model.NewCloudClient(oxide.URL, "", "", "", "")
			base, _ := newFakeGitHub(t, http.StatusOK, tt.runsJSON)
			s.githubAPIBase = base
			s.e2eInstances[run.key()] = []*E2EInstance{{InstallationID: "pr-server"}}
			s.e2eTrackedRuns = map[string]e2eRun{run.key(): run}
			s.e2eLastDispatch.Store(run.key(), time.Now().Add(-tt.dispatchedAt))

			s.cleanupStaleE2EInstances()

			if tt.wantDeleted {
				assert.Equal(t, []string{"pr-server"}, deletedInstallationIDs(fc))
				assert.NotContains(t, s.e2eInstances, run.key(), "a removed set must not be reused")
			} else {
				assert.Empty(t, deletedInstallationIDs(fc))
			}
		})
	}
}

func TestDispatchPushRun_DuplicatePushKeepsTheFirstRun(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	gh, captures := fakePushGitHub(t, "1111111", http.StatusOK, http.StatusNoContent)
	s.githubAPIBase = gh.URL + "/"
	run := e2eRun{owner: "mattermost", repo: oxideRepoName, ref: "main", sha: "1111111", platform: "both"}
	first := makeOxideInstances()
	s.e2eInstances[run.key()] = first

	second := []*E2EInstance{{InstallationID: "dup"}}
	s.dispatchPushRun(oxideE2EProfile, run, second, s.Logger)

	assert.Equal(t, first, s.e2eInstances[run.key()], "the first run's servers stay tracked")
	assert.Equal(t, []string{"dup"}, deletedInstallationIDs(fc))
	assert.Empty(t, *captures, "no second dispatch")
}
