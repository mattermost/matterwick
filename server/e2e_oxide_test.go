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

	assert.Zero(t, fc.count(), "push events must never provision servers for Oxide")
	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, "Push-triggered E2E is not enabled for Oxide, skipping", hook.LastEntry().Message)
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

func TestLockOxidePRProvisioning(t *testing.T) {
	s := &Server{}
	const prKey = "mattermost-mobile-oxide-pr-9"

	unlockFirst := s.lockOxidePRProvisioning(prKey)

	// A second request for the same PR (e.g. E2E/Run-iOS while E2E/Run-Android provisions)
	// must wait until the first one has stored its instances.
	acquired := make(chan func())
	go func() { acquired <- s.lockOxidePRProvisioning(prKey) }()
	select {
	case <-acquired:
		t.Fatal("second request for the same PR acquired the provisioning lock concurrently")
	case <-time.After(100 * time.Millisecond):
	}

	// Other PRs are never blocked by it.
	otherDone := make(chan struct{})
	go func() {
		s.lockOxidePRProvisioning("mattermost-mobile-oxide-pr-10")()
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
