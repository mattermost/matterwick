// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	mattermostModel "github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

// E2EInstance is one E2E test server.
type E2EInstance struct {
	Name           string `json:"name"`
	Platform       string `json:"platform"` // the profile slot: desktop OS (linux/macos/windows) or mobile site
	URL            string `json:"url"`
	InstallationID string `json:"installation_id"`
	ServerVersion  string `json:"server_version"`
}

// e2eUniqueSuffix returns an 8-char random hex suffix for unique instance names.
func e2eUniqueSuffix() string {
	return cloudModel.NewID()[:8]
}

// sanitizeForDNS lowercases and replaces non-DNS characters with hyphens.
func sanitizeForDNS(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "/", "-")
	return s
}

// e2eInstanceName builds a DNS-safe instance name and truncates if needed.
// parts are joined with "-". The total name + dnsSuffix must be <= 62.
func e2eInstanceName(dnsSuffix string, parts ...string) string {
	name := strings.Join(parts, "-")
	maxLen := 62 - len(dnsSuffix)
	if maxLen < 1 {
		maxLen = 1
	}
	if len(name) > maxLen {
		name = strings.TrimRight(name[:maxLen], "-")
	}
	return name
}

const forkPRE2EComment = "E2E tests don't run on pull requests from forks: the E2E workflow can only check out branches in this repository, and it holds secrets that fork code must not reach. A maintainer can apply `E2E/Override` to waive the E2E checks."

// isForkPR reports whether the PR head lives outside the base repository. An empty head repo
// (deleted fork) counts as a fork.
func isForkPR(pr *model.PullRequest) bool {
	return !strings.EqualFold(pr.FullName, pr.RepoOwner+"/"+pr.RepoName)
}

// handleE2ETestRequest is the main orchestrator for E2E test requests
func (s *Server) handleE2ETestRequest(pr *model.PullRequest, label string) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo":  pr.RepoName,
		"pr":    pr.Number,
		"label": label,
		"type":  "e2e",
	})
	logger.Info("Handling E2E test request")
	defer recoverE2E(logger)

	p := e2eProfileFor(pr.RepoName)
	if p == nil {
		logger.Error("Repository has no E2E profile")
		return
	}
	if !s.e2eEnabled(p) {
		logger.WithField("kind", p.kind).Info("E2E is disabled for this repository, ignoring E2E label")
		return
	}
	run := e2eRun{owner: pr.RepoOwner, repo: pr.RepoName, ref: pr.Ref, sha: pr.Sha, prNumber: pr.Number, platform: s.extractPlatformFromLabel(label)}
	logger = logger.WithFields(logrus.Fields{"kind": p.kind, "testPlatform": run.platform})

	// The dispatch targets a branch of the base repo and the workflow holds secrets, so fork PRs
	// never get servers.
	if isForkPR(pr) {
		logger.WithField("head_repo", pr.FullName).Info("Ignoring E2E label on a fork PR")
		// Tear down anything provisioned for this PR before the guard existed or still in flight.
		s.handleE2ECleanup(pr)
		s.sendGitHubComment(pr.RepoOwner, pr.RepoName, pr.Number, forkPRE2EComment)
		return
	}

	key := run.key()

	// Guard against duplicate webhook deliveries. Key includes platform so E2E/Run-Android and E2E/Run-iOS run independently.
	inProgressKey := fmt.Sprintf("%s-%s", key, run.platform)
	s.e2eInProgressLock.Lock()
	if s.e2eInProgress[inProgressKey] {
		s.e2eInProgressLock.Unlock()
		logger.Warn("E2E instance creation already in progress for this PR and platform, skipping duplicate request")
		return
	}
	s.e2eInProgress[inProgressKey] = true
	s.e2eInProgressLock.Unlock()
	defer func() {
		s.e2eInProgressLock.Lock()
		delete(s.e2eInProgress, inProgressKey)
		s.e2eInProgressLock.Unlock()
	}()

	// Serialize reuse/create/store per PR so concurrent Android and iOS label requests share
	// one server set instead of each provisioning their own.
	unlock := s.lockE2EPRProvisioning(key)
	defer unlock()

	// Every run starts on fresh servers, but only when no other E2E run or provisioning is
	// active on the PR: Android and iOS runs share one server set, so tearing it down mid-run
	// would break the other platform's run. Otherwise reuse. Superseded runs are cancelled by
	// each workflow's own concurrency group.
	fresh := s.isPRE2EIdle(pr, p.workflow, key, inProgressKey, logger)
	if fresh {
		logger.Info("No other E2E run active on this PR; replacing its servers with fresh ones")
		s.handleE2ECleanup(pr)
	}

	// Snapshot cleanup generation before provisioning; re-checked before storing to prevent stale writes after a concurrent reset.
	s.e2ePRCleanupGenerationLock.Lock()
	startGeneration := s.e2ePRCleanupGeneration[key]
	s.e2ePRCleanupGenerationLock.Unlock()

	// storeIfCurrent atomically writes instances, but only if cleanup hasn't advanced since provisioning started.
	storeIfCurrent := func(toStore []*E2EInstance) bool {
		s.e2ePRCleanupGenerationLock.Lock()
		defer s.e2ePRCleanupGenerationLock.Unlock()
		s.e2eInstancesLock.Lock()
		defer s.e2eInstancesLock.Unlock()
		if s.e2ePRCleanupGeneration[key] != startGeneration {
			return false
		}
		s.e2eInstances[key] = toStore
		return true
	}

	// 1. Reuse existing in-memory instances (servers stay alive between label toggles).
	s.e2eInstancesLock.Lock()
	existingInstances := s.e2eInstances[key]
	s.e2eInstancesLock.Unlock()

	if !fresh && len(existingInstances) > 0 && !s.wakeUpHibernatingInstances(existingInstances, logger) {
		logger.Warn("A tracked E2E server is gone; replacing the PR's servers")
		s.untrackRun(key)
		s.destroyE2EInstances(existingInstances, logger)
		fresh = true
	}
	if !fresh && len(existingInstances) > 0 {
		logger.WithField("instances", len(existingInstances)).Info("Reusing existing in-memory E2E instances")
		if err := s.dispatchE2E(p, run, existingInstances); err != nil {
			logger.WithError(err).Error("Failed to trigger E2E workflow with existing instances")
			s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		}
		return
	}

	// 2. Check cloud API for instances that survived a matterwick restart.
	if !fresh {
		cloudInstances, err := s.findExistingE2EInstancesInCloud(pr, p)
		reusable := err == nil && len(cloudInstances) == len(p.platforms)
		if reusable && !s.wakeUpHibernatingInstances(cloudInstances, logger) {
			logger.Warn("A cloud E2E server is not serving; replacing the PR's servers")
			s.destroyE2EInstances(cloudInstances, logger)
			reusable = false
		}
		if reusable {
			logger.WithField("instances", len(cloudInstances)).Info("Reusing existing cloud E2E instances")
			if !storeIfCurrent(cloudInstances) {
				logger.Warn("E2E reset was requested during cloud-reuse path; discarding reused instances")
				s.destroyE2EInstances(cloudInstances, logger)
				return
			}
			if err := s.dispatchE2E(p, run, cloudInstances); err != nil {
				logger.WithError(err).Error("Failed to trigger E2E workflow with cloud instances")
				s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
			}
			return
		}
	}

	// 3. No existing instances — create fresh ones. PR close / reset stops this (startProvision).
	provisionCtx, provisionDone := s.startProvision(key)
	instances, err := s.provisionRun(provisionCtx, p, run, logger)
	stoppedByCleanup := provisionCtx.Err() != nil // read before provisionDone, which cancels it
	provisionDone()
	if err != nil && stoppedByCleanup {
		logger.Info("PR E2E cleanup stopped provisioning")
		return
	}
	if err != nil {
		logger.WithError(err).Error("Failed to create E2E instances")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to create E2E test instances: %v", err))
		return
	}

	if len(instances) == 0 {
		logger.Error("No instances were created")
		s.postE2EErrorComment(pr, "Failed to create any E2E test instances")
		return
	}

	// Check if PR closed during provisioning (~30 min) — cleanup events don't fire for closed PRs.
	prInfo, _, prErr := newGithubClient(s.Config.GithubAccessToken).PullRequests.Get(
		context.Background(), pr.RepoOwner, pr.RepoName, pr.Number)
	if prErr != nil {
		logger.WithError(prErr).Warn("Failed to check PR state after instance creation; proceeding")
	} else if prInfo.GetState() == "closed" {
		logger.Warn("PR was closed during E2E instance creation; destroying instances without tracking")
		s.destroyE2EInstances(instances, logger)
		return
	} else if head := prInfo.GetHead().GetSHA(); head != "" && head != run.sha {
		// The branch moved on while the servers came up; the workflow runs on the branch, so
		// test its current commit (Oxide rejects any other MOBILE_VERSION).
		logger.WithField("head_sha", head).Info("PR branch moved on during provisioning; testing its current commit")
		run.sha = head
	}

	if !storeIfCurrent(instances) {
		logger.Warn("E2E reset was requested during provisioning; discarding freshly created instances")
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.WithField("instances", len(instances)).Info("Successfully created E2E instances")

	if err = s.dispatchE2E(p, run, instances); err != nil {
		logger.WithError(err).Error("Failed to trigger E2E workflow")
		s.postE2EErrorComment(pr, fmt.Sprintf("Failed to trigger E2E workflow: %v", err))
		// Remove from tracking before cleanup to avoid double-destroy on later cleanup.
		s.e2eInstancesLock.Lock()
		delete(s.e2eInstances, key)
		s.e2eInstancesLock.Unlock()
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.Info("Successfully triggered E2E workflow")
}

// provisionRun creates one fresh server per profile slot for run.
// Names: <kind>-pr-<N>-<platform>-<uid> or <kind>-push-<platform>-<uid>.
func (s *Server) provisionRun(ctx context.Context, p *e2eProfile, run e2eRun, logger logrus.FieldLogger) ([]*E2EInstance, error) {
	slots := make([]e2eSlot, len(p.platforms))
	for i, platform := range p.platforms {
		// Fresh uid per attempt: a replacement must not reuse the failed installation's DNS name.
		slots[i] = e2eSlot{platform: platform, nameFn: func() string {
			return e2eInstanceName(s.Config.DNSNameTestServer, p.kind, run.nameToken(), platform, e2eUniqueSuffix())
		}}
	}
	logger.WithField("servers", len(slots)).Info("Creating E2E instances")
	return s.provisionE2EInstances(ctx, slots, s.resolveMattermostServerVersion(), s.Config.E2EUsername, s.getE2EPassword(p.kind), p.kind, logger)
}

// recoverE2E logs a panic in an E2E goroutine instead of letting it crash the process, which
// would lose the tracking of every server in flight. Use as: defer recoverE2E(logger).
func recoverE2E(logger logrus.FieldLogger) {
	if r := recover(); r != nil {
		logger.WithField("panic", r).Errorf("E2E handler panicked\n%s", debug.Stack())
	}
}

// e2eSlot is one server of a parallel E2E set.
type e2eSlot struct {
	platform string        // e.g. "android-site-2" or "linux"; becomes E2EInstance.Platform
	nameFn   func() string // returns a fresh unique name for each attempt
}

// e2eSlotLabel names a slot for people: "android-site-2" -> "android site 2 server".
func e2eSlotLabel(platform string) string {
	return strings.ReplaceAll(platform, "-", " ") + " server"
}

// provisionE2EInstances creates one instance per slot in parallel and returns them in slot
// order. Each slot replaces its own failed installations (createCloudInstallationWithRetry)
// while its siblings keep going; only a slot that gives up cancels the others. Then every
// instance that did come up is destroyed, and the error names the slot that gave up and why.
func (s *Server) provisionE2EInstances(ctx context.Context, slots []e2eSlot, version, username, password, instanceType string, logger logrus.FieldLogger) ([]*E2EInstance, error) {
	// Shared cancellable context: when a slot gives up, the others stop within one poll
	// interval instead of waiting up to 30 minutes for a set that will be thrown away.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		instance *E2EInstance
		err      error
	}
	// Pre-allocate by index so each goroutine writes to its own slot.
	results := make([]result, len(slots))
	// failed lists failing slots in the order they gave up. The first one cancelled the rest,
	// so it is the root cause; later failures are usually just that cancellation.
	var failed []int
	var failedMu sync.Mutex
	var wg sync.WaitGroup

	for i, slot := range slots {
		wg.Add(1)
		go func(idx int, slot e2eSlot) {
			defer wg.Done()
			slotLogger := logger.WithField("platform", slot.platform)
			inst, err := s.provisionQueued(ctx, func() (*E2EInstance, error) {
				return s.createCloudInstallationWithRetry(ctx, slot.nameFn, version, username, password, instanceType, slotLogger)
			}, slotLogger)
			if err != nil {
				results[idx] = result{err: err}
				failedMu.Lock()
				failed = append(failed, idx)
				failedMu.Unlock()
				cancel()
				return
			}
			inst.Platform = slot.platform
			results[idx] = result{instance: inst}
		}(i, slot)
	}

	wg.Wait()

	var instances []*E2EInstance
	for _, r := range results {
		if r.err == nil {
			instances = append(instances, r.instance)
		}
	}
	if len(failed) == 0 {
		return instances, nil
	}

	// Report the first slot that failed for a reason of its own. Only if every slot was
	// cancelled (the caller cancelled) is the error a cancellation.
	root := failed[0]
	for _, idx := range failed {
		if !errors.Is(results[idx].err, context.Canceled) {
			root = idx
			break
		}
	}

	for i, r := range results {
		slotLogger := logger.WithField("platform", slots[i].platform)
		switch {
		case r.err == nil:
			slotLogger.WithField("installation_id", r.instance.InstallationID).Warn("E2E slot came up, destroying it because another slot failed")
		case i == root:
			slotLogger.WithError(r.err).Warn("E2E slot failed; cancelling the rest of the set")
		default:
			slotLogger.WithError(r.err).Warn("E2E slot did not come up")
		}
	}

	s.destroyE2EInstances(instances, logger)
	return nil, fmt.Errorf("%s: %w", e2eSlotLabel(slots[root].platform), results[root].err)
}

// defaultE2EMaxConcurrentProvisions is used when Config.E2EMaxConcurrentProvisions is 0.
const defaultE2EMaxConcurrentProvisions = 64

// provisionQueue returns the queue that bounds how many servers are being created at once
// across every E2E flow (PR labels, pushes, CMT).
func (s *Server) provisionQueue() chan struct{} {
	s.e2eProvisionQueueOnce.Do(func() {
		size := defaultE2EMaxConcurrentProvisions
		if s.Config != nil && s.Config.E2EMaxConcurrentProvisions > 0 {
			size = s.Config.E2EMaxConcurrentProvisions
		}
		s.e2eProvisionQueue = make(chan struct{}, size)
	})
	return s.e2eProvisionQueue
}

// provisionQueued runs create once a provisioning place is free. Any number of servers can
// be requested: past the limit they wait their turn instead of
// piling onto the provisioner. A place is held only while one server is being created, never
// across a whole set, so sets waiting for places cannot deadlock each other. The wait does
// not count against e2eInstallationMaxDuration.
func (s *Server) provisionQueued(ctx context.Context, create func() (*E2EInstance, error), logger logrus.FieldLogger) (*E2EInstance, error) {
	queue := s.provisionQueue()
	select {
	case queue <- struct{}{}:
	default:
		logger.WithField("limit", cap(queue)).Info("E2E provisioning limit reached; waiting for a free place")
		select {
		case queue <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled while waiting to provision: %w", ctx.Err())
		}
	}
	defer func() { <-queue }()
	return create()
}

// e2eCloudCallTimeout bounds one provisioner request. The cloud client has no HTTP timeout,
// so a hung request would otherwise stall its server forever and, for a create, every create
// queued behind it. Var so tests can shorten it.
var e2eCloudCallTimeout = 2 * time.Minute

// withCloudTimeout runs call, returning its error, or an error once ctx is done or
// e2eCloudCallTimeout passes. A call that times out keeps running in the background; call
// must therefore only write variables the caller reads after a nil return. A create that
// times out and later succeeds leaves a server with a matterwick name, which PR-close
// cleanup or the stale scan removes.
func withCloudTimeout(ctx context.Context, call func() error) error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	timer := time.NewTimer(e2eCloudCallTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("provisioner request abandoned: %w", ctx.Err())
	case <-timer.C:
		return fmt.Errorf("provisioner request timed out after %s", e2eCloudCallTimeout)
	}
}

// installationDeleteAttempts is how many times cleanup tries to delete an installation it
// created but could not bring up. A single failed delete would otherwise orphan a paid
// server: the caller has already discarded the ID by the time it returns, and a retry
// provisions under a fresh name, so nothing else references the old one until the periodic
// stale scan reaps it hours later.
const installationDeleteAttempts = 6

// installationDeleteRetryDelay is the first pause between delete attempts; it doubles each
// time (2s, 4s, ... about a minute in all). The provisioner answers 409 while it is working
// on the installation, which is likely right after creation. Var so tests can shorten it.
var installationDeleteRetryDelay = 2 * time.Second

// deleteInstallationWithRetry deletes installationID, retrying transient provisioner
// failures. An installation that is already gone (404) counts as deleted. If every attempt
// fails the installation is orphaned, so it reports to Mattermost with the ID — the periodic
// stale scan is the backstop, but it runs hours later and a silent orphan bills until then.
func (s *Server) deleteInstallationWithRetry(installationID, name string, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"installation_id": installationID,
		"instance":        name,
	})

	var lastErr error
	delay := installationDeleteRetryDelay
	for attempt := 1; attempt <= installationDeleteAttempts; attempt++ {
		lastErr = withCloudTimeout(context.Background(), func() error { return s.CloudClient.DeleteInstallation(installationID) })
		if lastErr == nil || s.installationGoneOrDeleting(installationID, lastErr) {
			s.e2eCreated.Delete(installationID)
			return
		}
		logger.WithError(lastErr).WithFields(logrus.Fields{
			"attempt":  attempt,
			"attempts": installationDeleteAttempts,
		}).Warn("Failed to delete E2E installation")
		if attempt < installationDeleteAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}

	logger.WithError(lastErr).Error("Gave up deleting E2E installation; it is orphaned until the periodic stale scan reaps it")
	s.logErrorToMattermost("Orphaned E2E installation %s (%s): delete failed %d times (%v). The periodic stale scan will retry, but it may need manual cleanup.",
		installationID, name, installationDeleteAttempts, lastErr)
}

// installationGoneOrDeleting reports whether a failed delete needs no retry: the installation
// is gone (404) or already being deleted (the provisioner answers 400 to a second delete).
func (s *Server) installationGoneOrDeleting(installationID string, deleteErr error) bool {
	code, ok := provisionerStatusCode(deleteErr)
	if ok && code == http.StatusNotFound {
		return true
	}
	if !ok || code != http.StatusBadRequest {
		return false
	}
	var inst *cloudModel.InstallationDTO
	err := withCloudTimeout(context.Background(), func() error {
		got, getErr := s.CloudClient.GetInstallation(installationID, nil)
		inst = got
		return getErr
	})
	return err == nil && (inst == nil || strings.HasPrefix(inst.State, "deletion"))
}

// deleteInstallationsByDNS deletes every installation with this DNS name.
func (s *Server) deleteInstallationsByDNS(dns string, logger logrus.FieldLogger) {
	var installations []*cloudModel.InstallationDTO
	err := withCloudTimeout(context.Background(), func() error {
		found, getErr := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{DNS: dns, Paging: cloudModel.AllPagesNotDeleted()})
		installations = found
		return getErr
	})
	if err != nil {
		logger.WithError(err).WithField("dns", dns).Warn("Failed to look up an installation whose create got no answer; the stale scan will remove it if it exists")
		return
	}
	for _, inst := range installations {
		logger.WithField("installation_id", inst.ID).Warn("Deleting an installation whose create got no answer")
		s.deleteInstallationWithRetry(inst.ID, inst.OwnerID, logger)
	}
}

// createCloudInstallationAttempts is how many installations a slot tries before giving up.
// Each failed one is deleted and replaced under a fresh name, so a retry never collides with
// the failed attempt's DNS record.
const createCloudInstallationAttempts = 3

// e2eInstallationRetryBackoff is the wait before replacing a failed installation (attempt 2,
// attempt 3, ...; the last entry repeats). Without it a fast failure (provisioner 5xx or rate
// limit, a DNS blip) burns every attempt within seconds. Var so tests can change it.
var e2eInstallationRetryBackoff = []time.Duration{15 * time.Second, 45 * time.Second, 90 * time.Second}

// e2eInstallationMaxDuration caps one slot's total time across all attempts, so a run cannot
// hang. It covers waiting for cluster capacity, so it is longer than one attempt's
// stability timeout. Var so tests can shorten it.
var e2eInstallationMaxDuration = 2 * time.Hour

// e2eRetrySleep waits d, or returns ctx's error as soon as ctx is done. Var so tests can
// record the backoff without waiting.
var e2eRetrySleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// e2eInstallationRetryDelay returns the backoff before the given retry (1 = before attempt 2),
// plus up to 20% jitter so slots that failed together do not retry in lockstep.
func e2eInstallationRetryDelay(retry int) time.Duration {
	i := retry - 1
	if i >= len(e2eInstallationRetryBackoff) {
		i = len(e2eInstallationRetryBackoff) - 1
	}
	base := e2eInstallationRetryBackoff[i]
	if base <= 0 {
		return 0
	}
	return base + time.Duration(rand.Int63n(int64(base)/5+1))
}

// provisionerStatusPattern matches the mattermost-cloud client's error for a non-success
// response. The client drops the response body, so the status code is all there is.
var provisionerStatusPattern = regexp.MustCompile(`failed with status code (\d+)`)

// provisionerStatusCode returns the provisioner HTTP status carried by err, if any.
func provisionerStatusCode(err error) (int, bool) {
	m := provisionerStatusPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	code, convErr := strconv.Atoi(m[1])
	return code, convErr == nil
}

// isRetryableE2EInstallationError reports whether replacing the installation can fix err.
// A replacement is a fresh installation under a fresh name, so it fixes provisioner hiccups
// (5xx, network errors), rate limiting (429), a DNS name conflict (409), and a server that
// failed to become stable or to initialize. Any other provisioner 4xx means the request or
// matterwick's credentials are wrong, and every attempt would fail the same way.
func isRetryableE2EInstallationError(err error) bool {
	code, ok := provisionerStatusCode(err)
	if !ok || code < 400 || code >= 500 {
		return true
	}
	return code == http.StatusConflict || code == http.StatusTooManyRequests
}

// createCloudInstallationWithRetry provisions one instance. A failed installation is deleted
// (by createCloudInstallation) and, if the error is retryable, replaced after a backoff, up to
// createCloudInstallationAttempts times within e2eInstallationMaxDuration. nameFn is called per
// attempt and must return a fresh unique name each time. It never cancels anything itself;
// the caller decides what a slot that gives up means for its siblings.
func (s *Server) createCloudInstallationWithRetry(ctx context.Context, nameFn func() string, version, username, password, instanceType string, logger logrus.FieldLogger) (*E2EInstance, error) {
	slotCtx, cancel := context.WithTimeout(ctx, e2eInstallationMaxDuration)
	defer cancel()

	var lastErr error
	attempts := 0
	for attempts < createCloudInstallationAttempts {
		if attempts > 0 {
			delay := e2eInstallationRetryDelay(attempts)
			logger.WithFields(logrus.Fields{
				"next_attempt": attempts + 1,
				"attempts":     createCloudInstallationAttempts,
				"backoff":      delay.String(),
			}).Warn("Replacing failed installation after backoff")
			if e2eRetrySleep(slotCtx, delay) != nil {
				break
			}
		}
		if slotCtx.Err() != nil {
			break
		}

		attempts++
		instance, err := s.createCloudInstallation(slotCtx, nameFn(), version, username, password, instanceType, logger)
		if err == nil {
			return instance, nil
		}
		lastErr = err
		if slotCtx.Err() != nil {
			break
		}
		logger.WithError(err).WithFields(logrus.Fields{
			"attempt":  attempts,
			"attempts": createCloudInstallationAttempts,
			"version":  version,
		}).Warn("Installation attempt failed")
		if !isRetryableE2EInstallationError(err) {
			return nil, fmt.Errorf("not retryable: %w", err)
		}
	}

	switch {
	case ctx.Err() != nil:
		// The caller (or a sibling that gave up) cancelled. Keep it recognisable as a
		// cancellation so it is never reported as this slot's own failure.
		if lastErr == nil {
			return nil, fmt.Errorf("installation creation cancelled before attempt %d: %w", attempts+1, ctx.Err())
		}
		if errors.Is(lastErr, context.Canceled) {
			return nil, lastErr
		}
		return nil, fmt.Errorf("installation creation cancelled after attempt %d failed (%v): %w", attempts, lastErr, ctx.Err())
	case slotCtx.Err() != nil:
		if lastErr == nil {
			lastErr = slotCtx.Err()
		}
		return nil, fmt.Errorf("gave up after %s (%d attempts): %w", e2eInstallationMaxDuration, attempts, lastErr)
	default:
		return nil, fmt.Errorf("failed after %d attempts: %w", attempts, lastErr)
	}
}

// createCloudInstallation creates one installation and polls until stable. Cancelling ctx aborts the wait so parallel callers can fail fast.
func (s *Server) createCloudInstallation(ctx context.Context, name, version, username, password, instanceType string, logger logrus.FieldLogger) (*E2EInstance, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("installation creation cancelled before request: %w", err)
	}

	// Create installation request
	envVars := cloudModel.EnvVarMap{
		"MM_SERVICESETTINGS_ENABLETUTORIAL":                  cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_ENABLEONBOARDINGFLOW":            cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_ENABLEUSERTYPINGMESSAGES":        cloudModel.EnvVar{Value: "false"},
		"MM_SERVICESETTINGS_SESSIONLENGTHMOBILEINHOURS":      cloudModel.EnvVar{Value: "5000"},
		"MM_SERVICESETTINGS_SESSIONCACHEINMINUTES":           cloudModel.EnvVar{Value: "180"},
		"MM_SERVICEENVIRONMENT":                              cloudModel.EnvVar{Value: "test"},
		"MM_RATELIMITSETTINGS_ENABLE":                        cloudModel.EnvVar{Value: "true"},
		"MM_RATELIMITSETTINGS_PERSEC":                        cloudModel.EnvVar{Value: "3000"},
		"MM_RATELIMITSETTINGS_MAXBURST":                      cloudModel.EnvVar{Value: "5000"},
		"MM_RATELIMITSETTINGS_MEMORYSTORESIZE":               cloudModel.EnvVar{Value: "10000"},
		"MM_RATELIMITSETTINGS_VARYBYREMOTEADDR":              cloudModel.EnvVar{Value: "false"},
		"MM_RATELIMITSETTINGS_VARYBYUSER":                    cloudModel.EnvVar{Value: "false"},
		"MM_TEAMSETTINGS_EXPERIMENTALENABLEAUTOMATICREPLIES": cloudModel.EnvVar{Value: "true"},
	}
	if mobileServerKind(instanceType) {
		envVars["MM_FEATUREFLAGS_CHANNELBOOKMARKS"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_FEATUREFLAGS_CUSTOMPROFILEATTRIBUTES"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_FEATUREFLAGS_INTERACTIVEDIALOGAPPSFORM"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_FEATUREFLAGS_MMBLOCKSENABLED"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_FILESETTINGS_ENABLEPUBLICLINK"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_PASSWORDSETTINGS_MINIMUMLENGTH"] = cloudModel.EnvVar{Value: "8"}
		envVars["MM_PLUGINSETTINGS_ENABLEMARKETPLACE"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_PLUGINSETTINGS_ENABLEREMOTEMARKETPLACE"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_PLUGINSETTINGS_ENABLEUPLOADS"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_RATELIMITSETTINGS_MAXBURST"] = cloudModel.EnvVar{Value: "50000"}
		envVars["MM_RATELIMITSETTINGS_PERSEC"] = cloudModel.EnvVar{Value: "10000"}
		envVars["MM_SERVICESETTINGS_ENABLEBOTACCOUNTCREATION"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_SERVICESETTINGS_ENABLECHANNELBOOKMARKS"] = cloudModel.EnvVar{Value: "true"}
		envVars["MM_SERVICESETTINGS_MAXIMUMACTIVEUSERS"] = cloudModel.EnvVar{Value: "999999"}
		envVars["MM_SERVICESETTINGS_MAXIMUMLOGINATTEMPTS"] = cloudModel.EnvVar{Value: "999999"}
		envVars["MM_SERVICESETTINGS_SESSIONLENGTHWEBINHOURS"] = cloudModel.EnvVar{Value: "4320"}
		envVars["MM_SUPPORTSETTINGS_HELPLINK"] = cloudModel.EnvVar{Value: "https://docs.mattermost.com/"}
		envVars["MM_SUPPORTSETTINGS_REPORTAPROBLEMTYPE"] = cloudModel.EnvVar{Value: "default"}
		envVars["MM_TEAMSETTINGS_MAXUSERSPERTEAM"] = cloudModel.EnvVar{Value: "999999"}
		envVars["MM_EXPERIMENTALSETTINGS_ENABLEWATERMARK"] = cloudModel.EnvVar{Value: "false"}
		envVars["MM_EXPERIMENTALSETTINGS_RESTRICTSYSTEMADMIN"] = cloudModel.EnvVar{Value: "false"}
	}

	// DNS is deprecated: send DNSNames, and Name, which the provisioner only derives from DNS.
	dnsName := fmt.Sprintf("%s.%s", name, s.Config.DNSNameTestServer)
	installationRequest := &cloudModel.CreateInstallationRequest{
		Name:        name,
		OwnerID:     name,
		Version:     version,
		DNSNames:    []string{dnsName},
		Size:        "miniSingleton",
		Affinity:    cloudModel.InstallationAffinityMultiTenant,
		Database:    cloudModel.InstallationDatabaseMultiTenantRDSPostgresPGBouncer,
		Filestore:   cloudModel.InstallationFilestoreBifrost,
		Annotations: []string{defaultMultiTenantAnnotation},
		PriorityEnv: envVars,
	}

	if len(s.Config.CloudGroupID) != 0 {
		installationRequest.GroupID = s.Config.CloudGroupID
	}

	installation, err := s.sendCreateInstallation(ctx, installationRequest, logger)
	if err != nil {
		if _, answered := provisionerStatusCode(err); !answered {
			// No answer (cancelled, timed out, connection lost): the provisioner may have
			// created it anyway. Look it up by name and delete it, or it bills unseen.
			s.deleteInstallationsByDNS(dnsName, logger)
		}
		return nil, fmt.Errorf("failed to create installation %s: %w", name, err)
	}
	s.e2eCreated.Store(installation.ID, struct{}{})

	// Delete the installation and name it in the error, so a failed run says which
	// installation failed and at which step.
	cleanupCreatedInstallation := func(cause error) error {
		s.deleteInstallationWithRetry(installation.ID, name, logger)
		return fmt.Errorf("installation %s (%s): %w", installation.ID, name, cause)
	}

	logger.WithField("installation_id", installation.ID).Info("Installation created, waiting for stable state")

	if err := s.waitForE2EInstallationStable(ctx, installation.ID, logger); err != nil {
		return nil, cleanupCreatedInstallation(err)
	}

	// Check cancellation before the 10-minute initialization phase (DNS + ping + user setup).
	if err := ctx.Err(); err != nil {
		return nil, cleanupCreatedInstallation(fmt.Errorf("installation creation cancelled before initialization: %w", err))
	}

	// Initialize Mattermost server with provided credentials
	spinwickURL := fmt.Sprintf("https://%s", cloudtools.GetInstallationDNSFromDNSRecords(installation))
	adminClient, err := initializeE2EServer(s, spinwickURL, username, password, logger)
	if err != nil {
		return nil, cleanupCreatedInstallation(fmt.Errorf("failed to initialize Mattermost server: %w", err))
	}

	if mobileServerKind(instanceType) {
		if err := s.restartWithSharedChannels(ctx, adminClient, installation.ID, logger); err != nil {
			return nil, cleanupCreatedInstallation(fmt.Errorf("failed to enable Shared Channels: %w", err))
		}
	}

	return &E2EInstance{
		Name:           name,
		URL:            spinwickURL,
		InstallationID: installation.ID,
		ServerVersion:  version,
	}, nil
}

// e2eCreateConflictBackoff is the wait before re-sending a create the provisioner answered with
// 409. Var so tests can change it.
var e2eCreateConflictBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}

// sendCreateInstallation sends one create request. With CloudGroupID set, the provisioner
// takes a non-blocking lock on the group for every create and answers 409 to any create that
// arrives while the lock is held. A server set's creates are sent in parallel, so they would
// conflict with each other: send them one at a time. The provisioner's group supervisor also
// holds that lock while it works, so a 409 is retried with the same request; nothing was
// created, so there is nothing to clean up. Waiting for the turn and the request itself both
// end as soon as ctx is cancelled; a cancelled caller never sends a create.
func (s *Server) sendCreateInstallation(ctx context.Context, request *cloudModel.CreateInstallationRequest, logger logrus.FieldLogger) (*cloudModel.InstallationDTO, error) {
	gate := s.createGate()
	for attempt := 0; ; attempt++ {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("installation create cancelled: %w", ctx.Err())
		}
		installation, err := s.CloudClient.CreateInstallationContext(ctx, request)
		<-gate
		if err == nil || ctx.Err() != nil || attempt == len(e2eCreateConflictBackoff) {
			return installation, err
		}
		if code, ok := provisionerStatusCode(err); !ok || code != http.StatusConflict {
			return installation, err
		}
		logger.WithError(err).WithField("backoff", e2eCreateConflictBackoff[attempt].String()).Warn("Provisioner group is locked; re-sending the create request")
		if e2eRetrySleep(ctx, e2eCreateConflictBackoff[attempt]) != nil {
			return nil, err
		}
	}
}

// createGate is the one-at-a-time gate for create requests.
func (s *Server) createGate() chan struct{} {
	s.e2eCreateGateOnce.Do(func() { s.e2eCreateGate = make(chan struct{}, 1) })
	return s.e2eCreateGate
}

// waitForE2EInstallationStable polls the provisioner until the installation is stable.
// Cancelling ctx aborts the wait so parallel callers can fail fast.
func (s *Server) waitForE2EInstallationStable(ctx context.Context, installationID string, logger logrus.FieldLogger) error {
	deadline := time.Now().Add(e2eInstallationStableTimeout)
	pollErrors := 0
	waitingForCapacity := false
	for {
		var inst *cloudModel.InstallationDTO
		err := withCloudTimeout(ctx, func() error {
			got, getErr := s.CloudClient.GetInstallation(installationID, nil)
			inst = got
			return getErr
		})
		if err != nil {
			// A busy provisioner can fail a poll; only a run of transient failures (or one
			// non-retryable 4xx) gives up the server.
			pollErrors++
			if pollErrors >= e2eStatusPollErrorLimit || ctx.Err() != nil || !isRetryableE2EInstallationError(err) {
				return fmt.Errorf("failed to get installation status: %w", err)
			}
			logger.WithError(err).WithField("consecutive_errors", pollErrors).Warn("Installation status poll failed; retrying")
			if e2eRetrySleep(ctx, e2eStatusPollRetryDelay) != nil {
				return fmt.Errorf("failed to get installation status: %w", err)
			}
			continue
		}
		pollErrors = 0
		if inst == nil {
			return fmt.Errorf("installation %s not found", installationID)
		}

		// The provisioner never retries these on its own, so waiting longer cannot help. A
		// deletion state means someone (PR cleanup, a manual delete) is removing it.
		if inst.State == cloudModel.InstallationStateCreationFailed || inst.State == cloudModel.InstallationStateUpdateFailed ||
			strings.HasPrefix(inst.State, "deletion") {
			return fmt.Errorf("installation entered state %q", inst.State)
		}

		if inst.State == cloudModel.InstallationStateStable || inst.State == cloudModel.InstallationStateHibernating {
			logger.WithField("state", inst.State).Info("Installation is stable")
			return nil
		}

		// No cluster has room yet; the provisioner places the installation once one does. A
		// replacement would only wait in the same line, so wait here, outside the stability
		// timeout (the slot's e2eInstallationMaxDuration still bounds it).
		if inst.State == cloudModel.InstallationStateCreationNoCompatibleClusters {
			if !waitingForCapacity {
				logger.Warn("No cluster has capacity for the installation yet; waiting")
				waitingForCapacity = true
			}
			deadline = time.Now().Add(e2eInstallationStableTimeout)
		} else {
			waitingForCapacity = false
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for installation to become stable")
		}

		// Context-aware sleep: wake immediately if a sibling goroutine failed and cancelled ctx.
		select {
		case <-ctx.Done():
			return fmt.Errorf("installation wait cancelled: %w", ctx.Err())
		case <-time.After(e2eInstallationPollInterval):
		}
	}
}

// e2eInstallationStableTimeout is how long one installation may take to become stable, not
// counting time spent waiting for cluster capacity. Var so tests can shorten it.
var e2eInstallationStableTimeout = 30 * time.Minute

// e2eStatusPollErrorLimit consecutive failed status polls give up the installation;
// e2eStatusPollRetryDelay is the pause after a failed poll.
// 30 x 10s rides out a provisioner restart of about 5 minutes without replacing every server.
const e2eStatusPollErrorLimit = 30

var e2eStatusPollRetryDelay = 10 * time.Second

// e2eInstallationPollInterval is the pause between installation status polls. Var, not
// const, so tests can shorten it.
var e2eInstallationPollInterval = 30 * time.Second

// restartWithSharedChannels turns on both Connected Workspaces flags, then restarts the
// server once. The server starts the remote cluster and shared channels services only at
// boot, and only if the flags are already on.
func (s *Server) restartWithSharedChannels(ctx context.Context, client *mattermostModel.Client4, installationID string, logger logrus.FieldLogger) error {
	logger = logger.WithField("installation_id", installationID)

	logger.Info("Enabling Shared Channels in config")
	// Raw JSON: the v6 client's Config has no ConnectedWorkspacesSettings.
	resp, err := client.DoAPIPut("/config/patch", `{"ConnectedWorkspacesSettings":{"EnableRemoteClusterService":true,"EnableSharedChannels":true}}`)
	if err != nil {
		return fmt.Errorf("failed to patch ConnectedWorkspacesSettings: %w", err)
	}
	resp.Body.Close()

	// Changing a PriorityEnv value makes the operator restart the pod. The patch merges,
	// so the existing env vars stay.
	logger.Info("Restarting server so Shared Channels starts at boot")
	err = withCloudTimeout(ctx, func() error {
		_, updateErr := s.CloudClient.UpdateInstallation(installationID, &cloudModel.PatchInstallationRequest{
			PriorityEnv: cloudModel.EnvVarMap{"MATTERWICK_E2E_RESTART_NONCE": cloudModel.EnvVar{Value: cloudModel.NewID()}},
		})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to update installation to restart the server: %w", err)
	}

	if err := s.waitForE2EInstallationStable(ctx, installationID, logger); err != nil {
		return err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := checkMMPing(pingCtx, client, logger); err != nil {
		return fmt.Errorf("failed to get mattermost ping response after restart: %w", err)
	}

	logger.Info("Server restarted with Shared Channels enabled")
	return nil
}

// getE2EPassword returns the password for E2E testing from config or org-level secrets
func (s *Server) getE2EPassword(instanceType string) string {
	var password string

	// Try config first, then fall back to environment variables
	password = s.Config.E2EPassword
	if password == "" {
		if mobileServerKind(instanceType) {
			password = os.Getenv("MM_MOBILE_E2E_ADMIN_PASSWORD")
		} else {
			password = os.Getenv("MM_DESKTOP_E2E_USER_CREDENTIALS")
		}
	}

	if password == "" {
		s.Logger.Warnf("E2E password not configured for %s; instance creation may fail", instanceType)
	}
	return password
}

// initializeE2EServer runs the post-stable setup (DNS, ping, admin user, team). Var so tests
// can bring an installation up without a real Mattermost server.
var initializeE2EServer = (*Server).initializeMattermostE2EServer

// initializeMattermostE2EServer initializes a Mattermost server with E2E credentials and
// returns a client logged in as the admin.
func (s *Server) initializeMattermostE2EServer(spinwickURL, username, password string, logger logrus.FieldLogger) (*mattermostModel.Client4, error) {
	return s.setupE2EServerCredentials(spinwickURL, username, password, logger)
}

// createE2EDefaultTeam creates the 'ad-1' team and adds userID to it.
// Separated so it can be unit-tested without DNS/ping dependencies.
func createE2EDefaultTeam(client *mattermostModel.Client4, userID string, logger logrus.FieldLogger) error {
	team := &mattermostModel.Team{
		Name:        "ad-1",
		DisplayName: "ad-1",
		Type:        "O",
	}
	createdTeam, _, err := client.CreateTeam(team)
	if err != nil {
		return fmt.Errorf("failed to create E2E default team 'ad-1': %w", err)
	}
	_, _, err = client.AddTeamMember(createdTeam.Id, userID)
	if err != nil {
		return fmt.Errorf("failed to add admin user to E2E team 'ad-1': %w", err)
	}
	logger.Info("Created default team 'ad-1' and added admin user")
	return nil
}

// setupE2EServerCredentials sets up a Mattermost server with provided E2E credentials
func (s *Server) setupE2EServerCredentials(spinwickURL, username, password string, logger logrus.FieldLogger) (*mattermostModel.Client4, error) {
	logger.Info("Setting up E2E server with provided credentials")

	wait := 600
	logger.Infof("Waiting up to %d seconds for DNS to propagate", wait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()

	// Parse the URL to get the hostname
	mmHost, err := url.Parse(spinwickURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse MM URL: %w", err)
	}

	// Check DNS
	if err := checkDNS(ctx, fmt.Sprintf("%s:443", mmHost.Host)); err != nil {
		return nil, fmt.Errorf("timed out waiting for DNS to propagate: %w", err)
	}

	client := mattermostModel.NewAPIv4Client(spinwickURL)

	// Wait for Mattermost to be available
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	if err := checkMMPing(ctx, client, logger); err != nil {
		return nil, fmt.Errorf("failed to get mattermost ping response: %w", err)
	}

	// Create the admin user with provided credentials
	adminUser := &mattermostModel.User{
		Username: username,
		Email:    fmt.Sprintf("%s@example.mattermost.com", username),
		Password: password,
	}
	_, _, err = client.CreateUser(adminUser)
	if err != nil {
		return nil, fmt.Errorf("failed to create E2E admin user: %w", err)
	}

	// Login as admin and capture the user ID
	userLogged, _, err := client.Login(username, password)
	if err != nil {
		return nil, fmt.Errorf("failed to log in as E2E admin user: %w", err)
	}

	// Create the default team 'ad-1' and add admin user to it
	if err := createE2EDefaultTeam(client, userLogged.Id, logger); err != nil {
		return nil, err
	}

	logger.Info("E2E server setup complete")
	return client, nil
}

// dispatchE2E dispatches p's workflow for run on run.ref with the profile's inputs.
func (s *Server) dispatchE2E(p *e2eProfile, run e2eRun, instances []*E2EInstance) error {
	if len(instances) != len(p.platforms) {
		return fmt.Errorf("%s E2E requires exactly %d instances, got %d", p.kind, len(p.platforms), len(instances))
	}
	inputs, err := p.inputs(s, run, instances)
	if err != nil {
		return err
	}
	client := s.e2eGithubClient()
	req, err := client.NewRequest("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", run.owner, run.repo, p.workflow),
		map[string]interface{}{"ref": run.ref, "inputs": inputs})
	if err != nil {
		return fmt.Errorf("failed to create workflow dispatch request: %w", err)
	}
	if _, err := client.Do(context.Background(), req, nil); err != nil {
		return fmt.Errorf("failed to dispatch %s: %w", p.workflow, err)
	}
	s.e2eLastDispatch.Store(run.key(), time.Now())
	s.e2eInstancesLock.Lock()
	if s.e2eTrackedRuns == nil {
		s.e2eTrackedRuns = make(map[string]e2eRun)
	}
	s.e2eTrackedRuns[run.key()] = run
	s.e2eInstancesLock.Unlock()
	s.Logger.WithFields(logrus.Fields{"repo": run.repo, "ref": run.ref, "workflow": p.workflow, "pr": run.prNumber}).Info("Dispatched E2E workflow")
	return nil
}

// e2eGithubClient returns a GitHub client, pointed at the test server when one is set.
func (s *Server) e2eGithubClient() *github.Client {
	client := newGithubClient(s.Config.GithubAccessToken)
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}
	return client
}

// lockE2EPRProvisioning blocks until this PR's provisioning/reuse section is free and
// returns its unlock func. E2E/Run-Android and E2E/Run-iOS have different in-progress keys
// (so they dispatch independently), but must not both create a server set: the second
// request waits here, then reuses the instances the first one stored. The per-PR mutex is
// kept for the process lifetime (one small entry per PR that ran E2E).
func (s *Server) lockE2EPRProvisioning(key string) func() {
	m, _ := s.e2ePRProvisionLocks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// handleE2ECleanup destroys tracked E2E instances, then queries the cloud API by DNS pattern to catch orphans.
func (s *Server) handleE2ECleanup(pr *model.PullRequest) {
	logger := s.Logger.WithFields(logrus.Fields{
		"repo": pr.RepoName,
		"pr":   pr.Number,
		"type": "e2e_cleanup",
	})
	logger.Info("Handling E2E cleanup request")

	key := fmt.Sprintf("%s-pr-%d", pr.RepoName, pr.Number)

	// Increment the cleanup generation counter *before* destroying anything.
	// handleE2ETestRequest captures this value before provisioning starts and
	// checks it again after the ~30 min creation window. If the counter advanced,
	// provisioning discards its result instead of writing stale instances to the
	// tracking map and dispatching a workflow against already-deleted servers.
	s.e2ePRCleanupGenerationLock.Lock()
	s.e2ePRCleanupGeneration[key]++
	s.e2ePRCleanupGenerationLock.Unlock()
	s.cancelProvision(key)

	// Fast path: in-memory map
	s.e2eInstancesLock.Lock()
	instances := s.e2eInstances[key]
	delete(s.e2eInstances, key)
	delete(s.e2eTrackedRuns, key)
	s.e2eInstancesLock.Unlock()

	if len(instances) > 0 {
		logger.WithField("instances", len(instances)).Info("Destroying tracked E2E instances")
		s.destroyE2EInstances(instances, logger)
	}

	// Fallback: catch orphans from restarts, map overwrites, or failed goroutines
	s.cleanupOrphanedE2EInstances(pr, logger)
}

// cleanupOrphanedE2EInstances queries the cloud API by DNS LIKE pattern and destroys any matches.
func (s *Server) cleanupOrphanedE2EInstances(pr *model.PullRequest, logger logrus.FieldLogger) {
	// Runs even when the profile is disabled, so servers created before then are removed.
	p := e2eProfileFor(pr.RepoName)
	if p == nil {
		logger.Debug("Skipping orphan E2E cleanup for non-E2E repo")
		return
	}

	dnsPattern := fmt.Sprintf("%s-pr-%d-%%", p.kind, pr.Number) // e.g. "mobile-pr-9587-%"

	var installations []*cloudModel.InstallationDTO
	err := withCloudTimeout(context.Background(), func() error {
		found, getErr := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{DNS: dnsPattern, Paging: cloudModel.AllPagesNotDeleted()})
		installations = found
		return getErr
	})
	if err != nil {
		logger.WithError(err).Error("Failed to query cloud API for orphaned E2E instances")
		return
	}

	var orphans []*E2EInstance
	for _, inst := range installations {
		// deletion-failed is not retried by the provisioner, so it is deleted again here.
		if inst.State != cloudModel.InstallationStateDeletionFailed && strings.HasPrefix(inst.State, "deletion") ||
			inst.State == cloudModel.InstallationStateDeleted {
			continue
		}
		orphans = append(orphans, &E2EInstance{Name: inst.OwnerID, InstallationID: inst.ID})
	}
	if len(orphans) > 0 {
		logger.WithField("orphans", len(orphans)).Warn("Destroying orphaned E2E instances found via cloud API")
		s.destroyE2EInstances(orphans, logger)
	}
}

// e2eInstanceMaxAge returns the configured maximum age for non-PR E2E instances before
// they are considered orphaned and eligible for deletion by the periodic cleanup scan.
// Falls back to 3 hours when the config value is 0 (unset).
func (s *Server) e2eInstanceMaxAge() time.Duration {
	if s.Config.E2EInstanceMaxAge > 0 {
		return time.Duration(s.Config.E2EInstanceMaxAge) * time.Hour
	}
	return 3 * time.Hour
}

// e2ePRInstanceMaxAge returns the maximum age a PR E2E instance may reach before the periodic
// scan deletes it. PR instances are reused across label toggles and commits, so this is much
// longer than e2eInstanceMaxAge. Falls back to 24 hours when the config value is 0 (unset).
func (s *Server) e2ePRInstanceMaxAge() time.Duration {
	if s.Config.E2EPRInstanceMaxAge > 0 {
		return time.Duration(s.Config.E2EPRInstanceMaxAge) * time.Hour
	}
	// Keep in sync with E2EPRInstanceMaxAge in config-matterwick.default.json.
	return 8 * time.Hour
}

// cleanupStaleE2EInstances reaps aged-out E2E instances: non-PR flows use e2eInstanceMaxAge, PR instances use e2ePRInstanceMaxAge (PR servers are kept alive for reuse; the cap prevents indefinite accumulation).
func (s *Server) cleanupStaleE2EInstances() {
	nonPRMaxAge := s.e2eInstanceMaxAge()
	prMaxAge := s.e2ePRInstanceMaxAge()
	logger := s.Logger.WithField("type", "periodic_e2e_cleanup")
	logger.WithFields(logrus.Fields{
		"non_pr_max_age_hours": nonPRMaxAge.Hours(),
		"pr_max_age_hours":     prMaxAge.Hours(),
	}).Info("Scanning for stale E2E instances")

	now := time.Now()

	var reapedPRInstallationIDs []string
	owners := s.trackedRunOwners()
	runActive := map[string]bool{} // per run key, looked up once per scan

	for _, p := range e2eProfiles {
		instanceType := p.kind
		pattern := instanceType + "-%"
		installations, err := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
			DNS:    pattern,
			Paging: cloudModel.AllPagesNotDeleted(),
		})
		if err != nil {
			logger.WithError(err).Errorf("Failed to query %s instances", instanceType)
			continue
		}

		for _, inst := range installations {
			// Guard against a nil embedded Installation — must come first, all other
			// field accesses (OwnerID, CreateAt, State, ID) are on the embedded struct.
			if inst.Installation == nil {
				logger.Warn("Skipping instance with nil Installation pointer in cleanup scan")
				continue
			}
			// Skip instances already being deleted. deletion-failed is not retried by the
			// provisioner, so it is deleted again here.
			if inst.State != cloudModel.InstallationStateDeletionFailed && strings.HasPrefix(inst.State, "deletion") ||
				inst.State == cloudModel.InstallationStateDeleted {
				continue
			}

			isPR := strings.Contains(inst.OwnerID, "-pr-")
			instLogger := logger.WithFields(logrus.Fields{
				"installation_id": inst.ID,
				"owner_id":        inst.OwnerID,
				"state":           inst.State,
				"is_pr":           isPR,
			})
			if keep, why := s.keepE2EInstance(inst, isPR, owners, runActive, now); keep {
				instLogger.WithField("reason", why).Debug("Keeping E2E instance")
				continue
			}

			instLogger.Warn("Destroying stale E2E instance")
			if err := withCloudTimeout(context.Background(), func() error { return s.CloudClient.DeleteInstallation(inst.ID) }); err != nil {
				instLogger.WithError(err).Error("Failed to destroy stale E2E instance")
				continue
			}
			s.e2eCreated.Delete(inst.ID)
			if isPR {
				reapedPRInstallationIDs = append(reapedPRInstallationIDs, inst.ID)
			}
			if ownerKey, owned := owners[inst.ID]; owned {
				s.untrackRun(ownerKey)
			}
		}
	}

	// Evict in-memory PR tracking entries whose servers were just reaped so the reuse path
	// in handleE2ETestRequest sees no live instances and creates a fresh set on the next
	// E2E/Run, instead of dispatching a workflow against deleted servers.
	if len(reapedPRInstallationIDs) > 0 {
		s.evictReapedPRInstances(reapedPRInstallationIDs, logger)
	}

	logger.Info("E2E instance cleanup scan complete")
}

// keepE2EInstance decides whether the stale scan keeps a server, and why.
//   - Tracked servers (a dispatched PR, push or CMT run) are kept while their run is queued or
//     running, or its state cannot be read, and for E2EPRIdleTimeoutMinutes after the last
//     dispatch. Age alone never removes them, so a long run keeps its servers. A run stuck for
//     e2eOwnedInstanceMaxAge loses them.
//   - Servers this process created but has not tracked yet (a set still provisioning) are
//     kept up to e2eOwnedInstanceMaxAge.
//   - Anything else (servers from before a restart) goes by age: E2EPRInstanceMaxAge for PR
//     servers, E2EInstanceMaxAge for the rest.
func (s *Server) keepE2EInstance(inst *cloudModel.InstallationDTO, isPR bool, owners map[string]string, runActive map[string]bool, now time.Time) (bool, string) {
	ownedCutoff := now.Add(-e2eOwnedInstanceMaxAge).UnixMilli()
	if key, owned := owners[inst.ID]; owned {
		if inst.CreateAt <= ownedCutoff {
			return false, ""
		}
		if last, ok := s.e2eLastDispatch.Load(key); ok && now.Sub(last.(time.Time)) < s.e2ePRIdleTimeout() {
			return true, "dispatched recently"
		}
		active, seen := runActive[key]
		if !seen {
			var err error
			if active, err = s.isTrackedRunActive(key); err != nil {
				s.Logger.WithError(err).WithField("tracking_key", key).Warn("Failed to read the state of an E2E run; keeping its servers")
				active = true
			}
			runActive[key] = active
		}
		return active, "its run is queued or running"
	}
	if _, ours := s.e2eCreated.Load(inst.ID); ours && inst.CreateAt > ownedCutoff {
		return true, "still being provisioned"
	}
	maxAge := s.e2eInstanceMaxAge()
	if isPR {
		maxAge = s.e2ePRInstanceMaxAge()
	}
	return inst.CreateAt > now.Add(-maxAge).UnixMilli(), "younger than its max age"
}

// e2ePRIdleTimeout returns Config.E2EPRIdleTimeoutMinutes, default 60 minutes.
func (s *Server) e2ePRIdleTimeout() time.Duration {
	if s.Config.E2EPRIdleTimeoutMinutes > 0 {
		return time.Duration(s.Config.E2EPRIdleTimeoutMinutes) * time.Minute
	}
	return time.Hour
}

// e2eOwnedInstanceMaxAge is the most the stale scan keeps a server matterwick is using. Past
// it the run is presumed stuck and the server is removed.
const e2eOwnedInstanceMaxAge = 24 * time.Hour

// trackedRunOwners maps each server of a dispatched PR, push or CMT run to its e2eInstances key.
func (s *Server) trackedRunOwners() map[string]string {
	s.e2eInstancesLock.Lock()
	defer s.e2eInstancesLock.Unlock()
	owners := map[string]string{}
	for key, instances := range s.e2eInstances {
		if _, tracked := s.e2eTrackedRuns[key]; !tracked && !strings.Contains(key, "-cmt-") {
			continue
		}
		for _, inst := range instances {
			if inst != nil {
				owners[inst.InstallationID] = key
			}
		}
	}
	return owners
}

// untrackRun drops a run's tracking once the stale scan has removed its servers.
func (s *Server) untrackRun(key string) {
	s.e2eInstancesLock.Lock()
	defer s.e2eInstancesLock.Unlock()
	delete(s.e2eInstances, key)
	delete(s.e2eTrackedRuns, key)
}

// isTrackedRunActive reports whether the workflow run owning a tracked PR, push or CMT key is
// queued or running on GitHub.
func (s *Server) isTrackedRunActive(key string) (bool, error) {
	s.e2eInstancesLock.Lock()
	run, tracked := s.e2eTrackedRuns[key]
	s.e2eInstancesLock.Unlock()

	client := s.e2eGithubClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if tracked {
		p := e2eProfileFor(run.repo)
		if p == nil {
			return false, nil
		}
		// A PR's runs are any dispatch on its branch; a push run is the one for its commit.
		query := "branch=" + url.QueryEscape(run.ref) + "&event=workflow_dispatch&per_page=50"
		if run.prNumber == 0 {
			query += "&head_sha=" + run.sha
		}
		var runs struct {
			WorkflowRuns []struct {
				Status string `json:"status"`
			} `json:"workflow_runs"`
		}
		req, err := client.NewRequest("GET", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?%s", run.owner, run.repo, p.workflow, query), nil)
		if err != nil {
			return false, err
		}
		if _, err := client.Do(ctx, req, &runs); err != nil {
			return false, err
		}
		for _, r := range runs.WorkflowRuns {
			if r.Status != "completed" {
				return true, nil
			}
		}
		return false, nil
	}

	// CMT keys are "<repo>-cmt-<run id>".
	i := strings.LastIndex(key, "-cmt-")
	if i < 0 {
		return false, fmt.Errorf("unrecognised tracking key %q", key)
	}
	var cmtRun struct {
		Status string `json:"status"`
	}
	req, err := client.NewRequest("GET", fmt.Sprintf("/repos/%s/%s/actions/runs/%s", s.Config.Org, key[:i], key[i+len("-cmt-"):]), nil)
	if err != nil {
		return false, err
	}
	if _, err := client.Do(ctx, req, &cmtRun); err != nil {
		return false, err
	}
	return cmtRun.Status != "completed", nil
}

// evictReapedPRInstances removes PR tracking entries when any member was reaped, so the reuse path never returns a partially-deleted set.
func (s *Server) evictReapedPRInstances(reapedInstallationIDs []string, logger logrus.FieldLogger) {
	reaped := make(map[string]bool, len(reapedInstallationIDs))
	for _, id := range reapedInstallationIDs {
		reaped[id] = true
	}

	s.e2eInstancesLock.Lock()
	defer s.e2eInstancesLock.Unlock()
	for key, instances := range s.e2eInstances {
		// PR tracking keys are "{repo}-pr-{number}"; non-PR keys are keyed by SHA elsewhere.
		if !strings.Contains(key, "-pr-") {
			continue
		}
		for _, inst := range instances {
			if inst != nil && reaped[inst.InstallationID] {
				delete(s.e2eInstances, key)
				logger.WithField("key", key).Info("Evicted expired PR E2E instances from tracking map")
				break
			}
		}
	}
}

// resolveBranchHeadSHA returns the branch HEAD SHA at dispatch time. Non-PR flows dispatch with ref=branch, so the run's head_sha may differ from the trigger SHA if the branch advanced during provisioning.
func (s *Server) resolveBranchHeadSHA(owner, repoName, branch string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newGithubClient(s.Config.GithubAccessToken)
	if s.githubAPIBase != "" {
		if baseURL, parseErr := url.Parse(s.githubAPIBase); parseErr == nil {
			client.BaseURL = baseURL
		}
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	req, err := client.NewRequest("GET", fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repoName, branch), nil)
	if err != nil {
		return "", err
	}
	if _, err := client.Do(ctx, req, &commit); err != nil {
		return "", err
	}
	if commit.SHA == "" {
		return "", fmt.Errorf("empty SHA for %s/%s@%s", owner, repoName, branch)
	}
	return commit.SHA, nil
}

// destroyE2EInstances destroys all given E2E instances in parallel, each with
// deleteInstallationWithRetry, so a busy provisioner does not leave billed servers behind.
func (s *Server) destroyE2EInstances(instances []*E2EInstance, logger logrus.FieldLogger) {
	var wg sync.WaitGroup
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		wg.Add(1)
		go func(instance *E2EInstance) {
			defer wg.Done()
			logger.WithField("instance_id", instance.InstallationID).Info("Destroying E2E instance")
			s.deleteInstallationWithRetry(instance.InstallationID, instance.Name, logger)
		}(instance)
	}
	wg.Wait()
}

// findExistingE2EInstancesInCloud queries the cloud API for E2E instances that match a PR and
// reconstructs E2EInstance objects for reuse. Returns an error if the count doesn't match
// the expected number of platforms (indicating a partial or fully absent set).
func (s *Server) findExistingE2EInstancesInCloud(pr *model.PullRequest, p *e2eProfile) ([]*E2EInstance, error) {
	platforms := p.platforms
	dnsPattern := fmt.Sprintf("%s-pr-%d-%%", p.kind, pr.Number)

	installations, err := s.CloudClient.GetInstallations(&cloudModel.GetInstallationsRequest{
		DNS:    dnsPattern,
		Paging: cloudModel.AllPagesNotDeleted(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query cloud API for existing E2E instances: %w", err)
	}

	// Only reuse instances that are in a stable, usable state.
	var reusable []*cloudModel.InstallationDTO
	for _, inst := range installations {
		if inst.State == cloudModel.InstallationStateStable ||
			inst.State == cloudModel.InstallationStateHibernating {
			reusable = append(reusable, inst)
		}
	}

	// Parse the platform token from each OwnerID.
	// OwnerID format: {type}-{version}-{platform}-{uid} where uid is 8 hex chars.
	// Strip the trailing "-{uid}" (9 chars) then check which expected platform is a suffix.
	platformByInst := make(map[string]*cloudModel.InstallationDTO, len(reusable)) // platform → inst
	for _, inst := range reusable {
		ownerID := inst.OwnerID
		var matched string
		for _, p := range platforms {
			// OwnerID without uid ends with "-{platform}"; uid is 9 chars ("-" + 8 hex).
			withoutUID := ownerID
			if len(ownerID) > 9 {
				withoutUID = ownerID[:len(ownerID)-9]
			}
			if strings.HasSuffix(withoutUID, "-"+p) {
				matched = p
				break
			}
		}
		if matched == "" {
			return nil, fmt.Errorf("could not determine platform for instance %q", ownerID)
		}
		if _, dup := platformByInst[matched]; dup {
			return nil, fmt.Errorf("duplicate platform %q found among cloud instances", matched)
		}
		platformByInst[matched] = inst
	}

	// Validate that every expected platform is present.
	for _, p := range platforms {
		if _, ok := platformByInst[p]; !ok {
			return nil, fmt.Errorf("expected platform %q not found among cloud instances (found %d, want %d)", p, len(reusable), len(platforms))
		}
	}

	// Build result in platforms[] order so index-based assignment is stable for callers.
	result := make([]*E2EInstance, len(platforms))
	for i, platform := range platforms {
		inst := platformByInst[platform]
		result[i] = &E2EInstance{
			Name:           inst.OwnerID,
			Platform:       platform,
			URL:            fmt.Sprintf("https://%s.%s", inst.OwnerID, s.Config.DNSNameTestServer),
			InstallationID: inst.ID,
			ServerVersion:  inst.Version,
		}
	}
	return result, nil
}

// wakeUpHibernatingInstances reports whether every instance is serving, waking any that
// hibernate. It reports false for one that is gone, failed, or not stable within
// e2eWakeTimeout, so callers replace the servers instead of dispatching to a dead one.
func (s *Server) wakeUpHibernatingInstances(instances []*E2EInstance, logger logrus.FieldLogger) bool {
	for _, inst := range instances {
		if !s.awaitServing(inst.InstallationID, logger.WithField("installation_id", inst.InstallationID)) {
			return false
		}
	}
	return true
}

// e2eWakeTimeout bounds how long a reused instance may take to be stable. Var so tests can
// shorten it.
var e2eWakeTimeout = 10 * time.Minute

// awaitServing polls the installation until it is stable, waking it while it hibernates.
// Failed checks and in-between states are waited out, so a brief cloud API error does not
// tear down servers a run is using.
func (s *Server) awaitServing(id string, log logrus.FieldLogger) bool {
	timeout := time.Now().Add(e2eWakeTimeout)
	for {
		state, err := s.installationState(id)
		switch {
		case err != nil:
			log.WithError(err).Warn("Failed to check E2E instance state")
		case state == cloudModel.InstallationStateStable:
			return true
		case state == "" || state == cloudModel.InstallationStateDeleted ||
			strings.HasPrefix(state, "deletion") || strings.HasSuffix(state, "-failed"):
			log.WithField("state", state).Warn("E2E instance is gone or failed")
			return false
		case state == cloudModel.InstallationStateHibernating:
			log.Info("Waking up hibernating E2E instance")
			if _, err := s.CloudClient.WakeupInstallation(id, nil); err != nil {
				log.WithError(err).Warn("Failed to wake up hibernating E2E instance")
			}
		}
		if time.Now().After(timeout) {
			log.WithField("state", state).Warn("E2E instance is not serving")
			return false
		}
		time.Sleep(e2eInstallationPollInterval)
	}
}

// installationState is the installation's state, or "" when it no longer exists.
func (s *Server) installationState(id string) (string, error) {
	var installation *cloudModel.InstallationDTO
	err := withCloudTimeout(context.Background(), func() error {
		got, getErr := s.CloudClient.GetInstallation(id, nil)
		installation = got
		return getErr
	})
	if err != nil || installation == nil {
		return "", err
	}
	return installation.State, nil
}

// postE2EStartedComment posts a comment when E2E tests start
func (s *Server) postE2EStartedComment(pr *model.PullRequest, instances []*E2EInstance) {
	ctx := context.Background()
	client := newGithubClient(s.Config.GithubAccessToken)

	var platformsList string
	for _, inst := range instances {
		platformsList += fmt.Sprintf("- %s: %s\n", inst.Platform, inst.URL)
	}

	comment := fmt.Sprintf(`## E2E Test Servers Created

The following test servers have been created and are ready for E2E testing:

%s

Tests will run against these servers. Please monitor the workflow run for progress.`, platformsList)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post E2E started comment")
	}
}

// postE2EErrorComment posts an error comment
func (s *Server) postE2EErrorComment(pr *model.PullRequest, errorMsg string) {
	ctx := context.Background()
	client := s.e2eGithubClient()

	comment := fmt.Sprintf("❌ E2E Test Setup Failed\n\n%s", errorMsg)

	_, _, err := client.Issues.CreateComment(ctx, pr.RepoOwner, pr.RepoName, pr.Number, &github.IssueComment{
		Body: &comment,
	})

	if err != nil {
		s.Logger.WithError(err).Error("Failed to post E2E error comment")
	}
}

// buildInstanceDetailsJSON builds the instance details JSON for desktop workflows
func (s *Server) buildInstanceDetailsJSON(instances []*E2EInstance) (string, error) {
	type instanceDetail struct {
		Platform       string `json:"platform"`
		Runner         string `json:"runner"`
		URL            string `json:"url"`
		InstallationID string `json:"installation-id"`
		ServerVersion  string `json:"server_version"`
	}

	details := make([]instanceDetail, len(instances))
	for i, inst := range instances {
		details[i] = instanceDetail{
			Platform:       inst.Platform,
			Runner:         getRunnerForPlatform(inst.Platform),
			URL:            inst.URL,
			InstallationID: inst.InstallationID,
			ServerVersion:  inst.ServerVersion,
		}
	}

	jsonBytes, err := json.Marshal(details)
	if err != nil {
		return "", err
	}

	return string(jsonBytes), nil
}

// e2eDispatchGracePeriod covers the delay between a workflow dispatch and GitHub listing
// the run: a trigger within it counts as busy.
const e2eDispatchGracePeriod = 5 * time.Minute

// isPRE2EIdle reports whether nothing else is using the PR's E2E servers: no other
// platform's request is provisioning, no E2E workflow was dispatched for the PR in the last
// e2eDispatchGracePeriod, and no E2E workflow run on the PR branch is queued or running.
// Any error counts as busy, so an unknown state never tears servers down.
func (s *Server) isPRE2EIdle(pr *model.PullRequest, workflowFile, key, inProgressKey string, logger logrus.FieldLogger) bool {
	s.e2eInProgressLock.Lock()
	for k := range s.e2eInProgress {
		if k != inProgressKey && strings.HasPrefix(k, key+"-") {
			s.e2eInProgressLock.Unlock()
			logger.WithField("other_request", k).Info("Another E2E request for this PR is in progress; reusing servers")
			return false
		}
	}
	s.e2eInProgressLock.Unlock()

	if last, ok := s.e2eLastDispatch.Load(key); ok && time.Since(last.(time.Time)) < e2eDispatchGracePeriod {
		logger.Info("An E2E workflow was dispatched for this PR moments ago; reusing servers")
		return false
	}

	client := s.e2eGithubClient()
	listURL := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?branch=%s&event=workflow_dispatch&per_page=100",
		pr.RepoOwner, pr.RepoName, workflowFile, url.QueryEscape(pr.Ref))
	req, err := client.NewRequest("GET", listURL, nil)
	if err != nil {
		logger.WithError(err).Warn("Failed to build workflow runs request; reusing servers")
		return false
	}
	var runs struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			HeadBranch string `json:"head_branch"`
			Status     string `json:"status"`
		} `json:"workflow_runs"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Do(ctx, req, &runs); err != nil {
		logger.WithError(err).Warn("Failed to list E2E workflow runs; reusing servers")
		return false
	}
	for _, run := range runs.WorkflowRuns {
		if run.HeadBranch == pr.Ref && run.Status != "completed" {
			logger.WithFields(logrus.Fields{"run_id": run.ID, "status": run.Status}).Info("An E2E workflow run is active on this PR; reusing servers")
			return false
		}
	}
	return true
}
