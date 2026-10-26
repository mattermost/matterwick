// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
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

func TestIsOxideRepo(t *testing.T) {
	for _, tt := range []struct {
		repo string
		want bool
	}{
		{"mattermost-mobile-oxide", true},
		{"Mattermost-Mobile-Oxide", true},
		{"mattermost-mobile", false},
		{"mattermost-desktop", false},
		{"mattermost-mobile-oxide-fork", false},
		{"oxide", false},
		{"", false},
	} {
		t.Run(tt.repo, func(t *testing.T) {
			assert.Equal(t, tt.want, isOxideRepo(tt.repo))
		})
	}
}

func TestOxideTopology(t *testing.T) {
	assert.Equal(t, []string{
		"android-site-1", "android-site-2", "android-site-3",
		"ios-site-1", "ios-site-2", "ios-site-3",
	}, oxideE2EPlatforms)
	assert.Equal(t, []string{
		"ANDROID_SITE_1_URL", "ANDROID_SITE_2_URL", "ANDROID_SITE_3_URL",
		"IOS_SITE_1_URL", "IOS_SITE_2_URL", "IOS_SITE_3_URL",
	}, oxideE2EWorkflowInputKeys)

	// GitHub caps workflow_dispatch at 10 inputs: 6 URLs + MOBILE_VERSION + PLATFORM + pr_number.
	assert.LessOrEqual(t, len(oxideE2EWorkflowInputKeys)+3, 10)

	// DNS LIKE patterns ("<type>-%") must stay disjoint across repo kinds.
	for _, other := range []string{"mobile", "desktop"} {
		assert.False(t, strings.HasPrefix(other, oxideE2EInstanceType))
		assert.False(t, strings.HasPrefix(oxideE2EInstanceType, other))
	}
}

func TestDryRun_OxideDispatch(t *testing.T) {
	s := newDryRunServer(t, "", "mattermost")
	instances := makeOxideInstances()

	for _, tt := range []struct {
		label    string
		platform string
	}{
		{"E2E/Run", "both"},
		{"E2E/Run-iOS", "ios"},
		{"E2E/Run-Android", "android"},
	} {
		t.Run(tt.label, func(t *testing.T) {
			platform := s.extractPlatformFromLabel(tt.label)
			require.Equal(t, tt.platform, platform)

			ghSrv, captures := mockGitHubServer(t, http.StatusNoContent)
			pr := &model.PullRequest{
				RepoOwner: "mattermost",
				RepoName:  "mattermost-mobile-oxide",
				Number:    42,
				Ref:       "feature-oxide",
				Sha:       "abc999",
			}
			require.NoError(t, s.triggerOxideE2EWorkflow(context.Background(), newTestGitHubClient(t, ghSrv), pr, instances, platform))
			require.Len(t, *captures, 1)

			c := (*captures)[0]
			assert.Equal(t, oxideE2EWorkflowFile, c.Workflow)
			assert.Equal(t, "mattermost-mobile-oxide", c.Repo)
			assert.Equal(t, "feature-oxide", c.Ref)

			var keys []string
			for k := range c.Inputs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			assert.Equal(t, []string{
				"ANDROID_SITE_1_URL", "ANDROID_SITE_2_URL", "ANDROID_SITE_3_URL",
				"IOS_SITE_1_URL", "IOS_SITE_2_URL", "IOS_SITE_3_URL",
				"MOBILE_VERSION", "PLATFORM", "pr_number",
			}, keys, "inputs must exactly match e2e-matterwick.yml's declared inputs")

			assert.Equal(t, tt.platform, c.Inputs["PLATFORM"])
			assert.Equal(t, "abc999", c.Inputs["MOBILE_VERSION"])
			assert.Equal(t, "42", c.Inputs["pr_number"])
			for i, p := range oxideE2EPlatforms {
				assert.Equal(t, "https://"+p+".test.example.com", c.Inputs[oxideE2EWorkflowInputKeys[i]])
			}
		})
	}
}

func TestBuildURLInputs(t *testing.T) {
	t.Run("order independent", func(t *testing.T) {
		instances := makeOxideInstances()
		for i, j := 0, len(instances)-1; i < j; i, j = i+1, j-1 {
			instances[i], instances[j] = instances[j], instances[i]
		}
		inputs, err := buildURLInputs("oxide", instances, oxideE2EPlatforms, oxideE2EWorkflowInputKeys)
		require.NoError(t, err)
		assert.Equal(t, "https://ios-site-3.test.example.com", inputs["IOS_SITE_3_URL"])
		assert.Equal(t, "https://android-site-1.test.example.com", inputs["ANDROID_SITE_1_URL"])
	})

	t.Run("count mismatch", func(t *testing.T) {
		_, err := buildURLInputs("oxide", makeOxideInstances()[:5], oxideE2EPlatforms, oxideE2EWorkflowInputKeys)
		require.EqualError(t, err, "oxide E2E requires exactly 6 instances, got 5")
	})

	t.Run("missing platform", func(t *testing.T) {
		instances := makeOxideInstances()
		instances[0].Platform = "site-3"
		_, err := buildURLInputs("oxide", instances, oxideE2EPlatforms, oxideE2EWorkflowInputKeys)
		require.EqualError(t, err, "oxide E2E missing instance for platform android-site-1")
	})

	t.Run("topology mismatch", func(t *testing.T) {
		_, err := buildURLInputs("oxide", makeOxideInstances(), oxideE2EPlatforms, oxideE2EWorkflowInputKeys[:5])
		require.Error(t, err)
	})

	t.Run("mobile wrapper unchanged", func(t *testing.T) {
		_, err := buildMobileURLInputs(makeMobileInstances()[:3])
		require.EqualError(t, err, "mobile E2E requires exactly 5 instances, got 3")
	})
}

func TestOxideInstanceNamesFitAndParse(t *testing.T) {
	const dns = "test.mattermost.cloud"
	// Longest realistic name: 5-digit PR number, longest platform token.
	full := strings.Join([]string{oxideE2EInstanceType, "pr-99999", "android-site-3", "abcdef12"}, "-")
	assert.Equal(t, full, e2eInstanceName(dns, oxideE2EInstanceType, "pr-99999", "android-site-3", "abcdef12"),
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
	found, err := s.findExistingE2EInstancesInCloud(pr, oxideE2EInstanceType, oxideE2EPlatforms)
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

	assert.Equal(t, "mobile-secret", s.getE2EPassword(oxideE2EInstanceType))
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
	assert.Equal(t, mobile, priorityEnv(oxideE2EInstanceType), "Oxide must get exactly the mobile server baseline")
	assert.NotEqual(t, mobile, priorityEnv("desktop"))
}

func TestHandlePushEventE2E_SkipsOxide(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.Config.E2EOxideEnabled = true
	s.Config.E2EAutoTriggerOnMaster = true
	repo := "mattermost-mobile-oxide"
	sha := "abc"
	event := &gogithub.PushEvent{
		Repo:       &gogithub.PushEventRepository{Name: &repo},
		HeadCommit: &gogithub.HeadCommit{ID: &sha},
	}

	logger, hook := test.NewNullLogger()
	s.Logger = logger

	s.handlePushEventE2E(event, "main")

	assert.Zero(t, fc.count(), "the desktop/mobile push path must never provision servers for Oxide")
	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, "Oxide push E2E is handled by handleOxidePushEventE2E, skipping", hook.LastEntry().Message)
}

func oxidePushEvent(ref, defaultBranch, sha string, deleted bool) *gogithub.PushEvent {
	repo := oxideRepoName
	return &gogithub.PushEvent{
		Ref:        &ref,
		Deleted:    &deleted,
		Repo:       &gogithub.PushEventRepository{Name: &repo, DefaultBranch: &defaultBranch},
		HeadCommit: &gogithub.HeadCommit{ID: &sha},
	}
}

func TestOxidePushE2ESkipReason(t *testing.T) {
	for _, tt := range []struct {
		name          string
		oxideEnabled  bool
		autoTrigger   bool
		branch        string
		defaultBranch string
		deleted       bool
		want          string
	}{
		{"default branch push runs", true, true, "main", "main", false, ""},
		{"non-main default branch runs", true, true, "trunk", "trunk", false, ""},
		{"profile disabled", false, true, "main", "main", false, "Oxide E2E is disabled (E2EOxideEnabled=false)"},
		{"auto-trigger disabled", true, false, "main", "main", false, "push E2E is disabled (E2EAutoTriggerOnMaster=false)"},
		{"branch deleted", true, true, "main", "main", true, "branch was deleted"},
		{"feature branch", true, true, "feature-x", "main", false, "push is not to the default branch"},
		{"master is not the default branch", true, true, "master", "main", false, "push is not to the default branch"},
		{"default branch unknown", true, true, "main", "", false, "push is not to the default branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{Config: &MatterwickConfig{E2EOxideEnabled: tt.oxideEnabled, E2EAutoTriggerOnMaster: tt.autoTrigger}}
			event := oxidePushEvent("refs/heads/"+tt.branch, tt.defaultBranch, "abc", tt.deleted)
			assert.Equal(t, tt.want, s.oxidePushE2ESkipReason(event, tt.branch))
		})
	}
}

func TestOxidePushInstanceNames(t *testing.T) {
	const dns = "test.mattermost.cloud"
	for _, p := range oxideE2EPlatforms {
		full := strings.Join([]string{oxideE2EInstanceType, oxidePushE2ENameToken, p, "abcdef12"}, "-")
		name := e2eInstanceName(dns, oxideE2EInstanceType, oxidePushE2ENameToken, p, "abcdef12")
		assert.Equal(t, full, name, "push names must not be truncated")
		assert.True(t, strings.HasPrefix(name, oxideE2EInstanceType+"-"), "the periodic scan must find push servers")
		assert.NotContains(t, name, "-pr-", "push servers must use the non-PR max age and never match oxide-pr-<N>-%")
	}
}

// fakeOxideGitHub serves the commits lookup (HEAD sha, or headStatus) and records dispatches.
func fakeOxideGitHub(t *testing.T, head string, headStatus, dispatchStatus int) (*httptest.Server, *[]dispatchCapture) {
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

func TestDispatchOxidePushRun(t *testing.T) {
	const pushSHA = "1111111"
	key := oxidePushInstanceKey(oxideRepoName, "main", pushSHA)

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
			gh, captures := fakeOxideGitHub(t, tt.head, tt.headStatus, tt.dispatchStatus)
			s.githubAPIBase = gh.URL + "/"
			instances := makeOxideInstances()

			s.dispatchOxidePushRun("mattermost", oxideRepoName, "main", pushSHA, instances, s.Logger)

			if tt.wantDispatch {
				require.Len(t, *captures, 1)
				c := (*captures)[0]
				assert.Equal(t, oxideE2EWorkflowFile, c.Workflow)
				assert.Equal(t, oxideRepoName, c.Repo)
				assert.Equal(t, "main", c.Ref)
				assert.Equal(t, pushSHA, c.Inputs["MOBILE_VERSION"])
				assert.Equal(t, "both", c.Inputs["PLATFORM"])
				assert.Equal(t, "", c.Inputs["pr_number"], "an empty pr_number marks a default-branch run")
				assert.Len(t, c.Inputs, 9, "inputs must exactly match e2e-matterwick.yml's declared inputs")
				for i, p := range oxideE2EPlatforms {
					assert.Equal(t, "https://"+p+".test.example.com", c.Inputs[oxideE2EWorkflowInputKeys[i]])
				}
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

func TestHandleWorkflowRunEvent_OxidePushCleanup(t *testing.T) {
	const (
		oxideWorkflowPath = ".github/workflows/" + oxideE2EWorkflowFile
		doneKey           = "mattermost-mobile-oxide-push-main-sha1"
		otherPushKey      = "mattermost-mobile-oxide-push-main-sha2"
		prKey             = "mattermost-mobile-oxide-pr-5"
	)
	for _, tt := range []struct {
		name       string
		repo       string
		action     string
		path       string
		headBranch string
		headSHA    string
		wantGone   []string
	}{
		{"main run completed", oxideRepoName, "completed", oxideWorkflowPath, "main", "sha1", []string{doneKey}},
		{"path with ref suffix", oxideRepoName, "completed", oxideWorkflowPath + "@refs/heads/main", "main", "sha1", []string{doneKey}},
		{"PR run on the same commit", oxideRepoName, "completed", oxideWorkflowPath, "feature-x", "sha1", nil},
		{"main run still in progress", oxideRepoName, "in_progress", oxideWorkflowPath, "main", "sha1", nil},
		{"other Oxide workflow", oxideRepoName, "completed", ".github/workflows/ci.yml", "main", "sha1", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, fc := newFakeCloudServer(t, "[]")
			s.Config.E2ETestWorkflowNames = []string{"E2E"}
			tracked := map[string][]*E2EInstance{
				doneKey:      {{InstallationID: "main-sha1"}},
				otherPushKey: {{InstallationID: "main-sha2"}},
				prKey:        {{InstallationID: "pr-5"}},
			}
			for k, v := range tracked {
				s.e2eInstances[k] = v
			}

			s.handleWorkflowRunEventWithInputs(&WorkflowRunWebhookPayload{
				Action: tt.action,
				WorkflowRun: WorkflowRunWithInputs{
					ID: 1, Name: "maestro", HeadBranch: tt.headBranch, HeadSHA: tt.headSHA, Path: tt.path,
				},
				Repository: map[string]interface{}{
					"name":  tt.repo,
					"owner": map[string]interface{}{"login": "mattermost"},
				},
			})

			var wantDeleted []string
			for k, v := range tracked {
				gone := false
				for _, g := range tt.wantGone {
					gone = gone || g == k
				}
				if gone {
					assert.NotContains(t, s.e2eInstances, k)
					wantDeleted = append(wantDeleted, instanceIDs(v)...)
				} else {
					assert.Contains(t, s.e2eInstances, k)
				}
			}
			sort.Strings(wantDeleted)
			assert.Equal(t, wantDeleted, deletedInstallationIDs(fc))
		})
	}
}

func TestParseWorkflowRunEvent_Path(t *testing.T) {
	payload, err := ParseWorkflowRunEventWithInputs(strings.NewReader(
		`{"action":"completed","workflow_run":{"name":"maestro","head_branch":"main","head_sha":"abc","path":".github/workflows/e2e-matterwick.yml"}}`))
	require.NoError(t, err)
	assert.True(t, isOxideE2EWorkflowRun(oxideRepoName, payload.WorkflowRun.Path))
}

func TestHandleCMTTrigger_SkipsOxide(t *testing.T) {
	s, fc := newFakeCloudServer(t, "[]")
	s.Config.E2EOxideEnabled = true

	logger, hook := test.NewNullLogger()

	s.handleCMTTrigger("mattermost", "mattermost-mobile-oxide", "main", "abc", 1, logger)

	assert.Zero(t, fc.count(), "CMT must never provision servers for Oxide")
	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, "CMT is not enabled for Oxide, skipping CMT trigger", hook.LastEntry().Message)
}

func TestCancelPRWorkflowRuns_SkipsOxide(t *testing.T) {
	logger, hook := test.NewNullLogger()
	s := &Server{Config: &MatterwickConfig{GithubAccessToken: "t"}, Logger: logger}
	pr := &model.PullRequest{RepoOwner: "mattermost", RepoName: "mattermost-mobile-oxide", Number: 3, Ref: "b"}

	s.cancelPRWorkflowRuns(pr, logger)

	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, "Skipping workflow-run cancellation for Oxide (handled by workflow concurrency)", hook.LastEntry().Message)
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
