// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// handleCMTTrigger resolves instance type and server versions, then delegates to handleCMTWithServerVersions.
func (s *Server) handleCMTTrigger(owner, repoName, branch, sha string, runID int64, logger logrus.FieldLogger) {
	instanceType := "desktop"
	if strings.Contains(repoName, "mobile") {
		instanceType = "mobile"
	} else if !strings.Contains(repoName, "desktop") {
		logger.Warn("Repository is neither desktop nor mobile, skipping CMT trigger")
		return
	}

	versions := s.cmtServerVersions(instanceType)
	logger.WithFields(logrus.Fields{
		"instanceType": instanceType,
		"versions":     versions,
	}).Info("Provisioning CMT instances for resolved server versions")

	s.handleCMTWithServerVersions(owner, repoName, instanceType, branch, sha, versions, runID, logger)
}

// cmtDroppedVersionRetryDelay waits for provisioner capacity before retrying failed versions. Var so tests can zero it.
var cmtDroppedVersionRetryDelay = 5 * time.Minute

// cmtMobileProvisionAcquireTimeout bounds how long a trigger waits for the mobile provision slot.
var cmtMobileProvisionAcquireTimeout = 10 * time.Minute

// cmtMobileProvisionSem is a single-slot semaphore so primary + retry (or concurrent triggers)
// cannot overlap CreateInstallation storms. Prefer this over a mutex so waiters honor context cancel.
var cmtMobileProvisionSem = make(chan struct{}, 1)

func acquireCMTMobileProvision(ctx context.Context) error {
	select {
	case cmtMobileProvisionSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseCMTMobileProvision() {
	select {
	case <-cmtMobileProvisionSem:
	default:
	}
}

// cmtContextWithStop returns a context cancelled when timeout elapses (if >0) or s.stopCh closes.
func (s *Server) cmtContextWithStop(timeout time.Duration) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	if s == nil || s.stopCh == nil {
		return ctx, cancel
	}
	stopCtx, stopCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-s.stopCh:
			stopCancel()
		case <-stopCtx.Done():
		}
	}()
	return stopCtx, func() {
		stopCancel()
		cancel()
		<-done
	}
}

// cmtRunDroppedRetry schedules delayed re-provision + follow-up CMT dispatch. Var so tests can capture without sleeping.
// Owns the retry delay (retryDroppedCMTVersions does not sleep). Honors s.stopCh when set.
var cmtRunDroppedRetry = func(s *Server, repoOwner, repoName, instanceType, branch, sha string, runID int64, dropped []string, fullSuiteVersion string, logger logrus.FieldLogger) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.WithField("panic", r).Error("CMT dropped-version retry goroutine panicked")
			}
		}()
		timer := time.NewTimer(cmtDroppedVersionRetryDelay)
		defer timer.Stop()
		if s.stopCh != nil {
			select {
			case <-timer.C:
			case <-s.stopCh:
				logger.Warn("Server stopping; skipping CMT dropped-version retry")
				return
			}
		} else {
			<-timer.C
		}
		s.retryDroppedCMTVersions(repoOwner, repoName, instanceType, branch, sha, runID, dropped, fullSuiteVersion, logger)
	}()
}

// cmtProvisionVersions is the provision entry used by handle/retry. Var so tests can stub results.
var cmtProvisionVersions = func(s *Server, repoName, instanceType string, serverVersions []string, fullSuiteVersion string, logger logrus.FieldLogger) (allInstances []*E2EInstance, validVersions, droppedVersions []string) {
	return s.provisionCMTVersions(repoName, instanceType, serverVersions, fullSuiteVersion, logger)
}

// cmtDispatchAndTrack is the dispatch entry used by handle/retry. Var so tests can assert it was skipped.
var cmtDispatchAndTrack = func(s *Server, repoOwner, repoName, instanceType, branch string, runID int64, versions []string, instances []*E2EInstance, logger logrus.FieldLogger) {
	s.dispatchAndTrackCMT(repoOwner, repoName, instanceType, branch, runID, versions, instances, logger)
}

// cmtVersionsContain reports whether versions includes want (v-prefix optional either side).
func cmtVersionsContain(versions []string, want string) bool {
	want = strings.TrimPrefix(strings.TrimSpace(want), "v")
	if want == "" {
		return false
	}
	for _, version := range versions {
		if strings.TrimPrefix(strings.TrimSpace(version), "v") == want {
			return true
		}
	}
	return false
}

// handleCMTWithServerVersions provisions CMT instances and dispatches compatibility-matrix-testing.yml.
// Failed versions are dropped from the primary matrix and retried later.
func (s *Server) handleCMTWithServerVersions(repoOwner, repoName, instanceType, branch, sha string, serverVersions []string, runID int64, logger logrus.FieldLogger) {
	versionCap := cmtVersionCapFor(instanceType)
	if len(serverVersions) > versionCap {
		if instanceType == "mobile" {
			originalCount := len(serverVersions)
			serverVersions = spanCMTServerVersions(serverVersions, versionCap)
			logger.Warnf("Capping mobile server versions from %d to %d (keeping range ends): %s",
				originalCount, len(serverVersions), strings.Join(serverVersions, ", "))
		} else {
			logger.Warnf("Capping server versions from %d to %d (keeping newest)", len(serverVersions), versionCap)
			serverVersions = capCMTServerVersions(serverVersions, versionCap)
		}
	}

	requestedVersions := append([]string(nil), serverVersions...)
	fullSuiteVersion := ""
	if instanceType == "mobile" {
		fullSuiteVersion = cmtLatestServerVersion(requestedVersions)
	}

	logger = logger.WithFields(logrus.Fields{
		"serverVersionCount": len(serverVersions),
		"branch":             branch,
		"sha":                sha,
		"run_id":             runID,
	})
	logger.Info("Starting CMT with server versions")

	allInstances, validVersions, droppedVersions := cmtProvisionVersions(s, repoName, instanceType, serverVersions, fullSuiteVersion, logger)

	// Mobile primary dispatch requires the full-suite version. Smoke-only survivors must not ship.
	if instanceType == "mobile" && fullSuiteVersion != "" && !cmtVersionsContain(validVersions, fullSuiteVersion) {
		msg := fmt.Sprintf("refusing to dispatch mobile CMT without full-suite version %s", fullSuiteVersion)
		logger.WithFields(logrus.Fields{
			"full_suite_version": fullSuiteVersion,
			"valid_versions":     validVersions,
			"dropped_versions":   droppedVersions,
		}).Error(msg)
		s.logErrorToMattermost("CMT on %s/%s (%s): %s. Destroying any smoke-only survivors and retrying requested versions.",
			repoOwner, repoName, branch, msg)
		if len(allInstances) > 0 {
			s.destroyE2EInstances(allInstances, logger)
		}
		retryVersions := append([]string(nil), requestedVersions...)
		cmtRunDroppedRetry(s, repoOwner, repoName, instanceType, branch, sha, runID, retryVersions, fullSuiteVersion, logger)
		return
	}

	if len(droppedVersions) > 0 {
		logger.WithFields(logrus.Fields{
			"dropped_versions": droppedVersions,
			"kept_versions":    validVersions,
		}).Error("CMT matrix is running with reduced server-version coverage; scheduling targeted retry")
		if len(validVersions) > 0 {
			s.logErrorToMattermost("CMT on %s/%s (%s): dropped server version(s) %s — provisioning failed. Running with %s. Will retry dropped versions and re-dispatch a follow-up CMT.",
				repoOwner, repoName, branch, strings.Join(droppedVersions, ", "), strings.Join(validVersions, ", "))
		} else {
			s.logErrorToMattermost("CMT on %s/%s (%s): no server version could be provisioned on first pass (%s). Will retry and re-dispatch if any recover.",
				repoOwner, repoName, branch, strings.Join(droppedVersions, ", "))
		}
		droppedCopy := append([]string(nil), droppedVersions...)
		cmtRunDroppedRetry(s, repoOwner, repoName, instanceType, branch, sha, runID, droppedCopy, fullSuiteVersion, logger)
	}

	if len(allInstances) == 0 {
		logger.Error("No CMT instances created on first pass; nothing to dispatch yet")
		return
	}

	cmtDispatchAndTrack(s, repoOwner, repoName, instanceType, branch, runID, validVersions, allInstances, logger)
}

// provisionCMTVersions provisions one topology per server version; failures drop that version.
// fullSuiteVersion is the originally requested latest (mobile full topology); desktop callers pass "".
//
// Mobile batches (sequential):
//  1) full-suite version — 5 servers (required for primary dispatch)
//  2) each remaining version — 1 smoke server
// Smoke provisioning never starts until the full-suite attempt has finished (success or fail).
func (s *Server) provisionCMTVersions(repoName, instanceType string, serverVersions []string, fullSuiteVersion string, logger logrus.FieldLogger) (allInstances []*E2EInstance, validVersions, droppedVersions []string) {
	if instanceType == "mobile" {
		acquireCtx, acquireCancel := s.cmtContextWithStop(cmtMobileProvisionAcquireTimeout)
		defer acquireCancel()
		if err := acquireCMTMobileProvision(acquireCtx); err != nil {
			logger.WithError(err).Error("Failed to acquire mobile CMT provision slot; dropping version set for later retry")
			dropped := make([]string, 0, len(serverVersions))
			for _, version := range serverVersions {
				version = strings.TrimPrefix(strings.TrimSpace(version), "v")
				if version != "" {
					dropped = append(dropped, version)
				}
			}
			return nil, nil, dropped
		}
		defer releaseCMTMobileProvision()
	}

	provisionCtx, provisionCancel := s.cmtContextWithStop(0)
	defer provisionCancel()

	fullSuiteVersion = strings.TrimPrefix(strings.TrimSpace(fullSuiteVersion), "v")

	ordered := make([]string, 0, len(serverVersions))
	if instanceType == "mobile" && fullSuiteVersion != "" {
		seen := make(map[string]bool, len(serverVersions))
		var rest []string
		for _, version := range serverVersions {
			normalized := strings.TrimPrefix(strings.TrimSpace(version), "v")
			if normalized == "" || seen[normalized] {
				continue
			}
			seen[normalized] = true
			if normalized == fullSuiteVersion {
				ordered = append(ordered, version)
			} else {
				rest = append(rest, version)
			}
		}
		ordered = append(ordered, rest...)
	} else {
		seen := make(map[string]bool, len(serverVersions))
		for _, version := range serverVersions {
			normalized := strings.TrimPrefix(strings.TrimSpace(version), "v")
			if normalized == "" || seen[normalized] {
				continue
			}
			seen[normalized] = true
			ordered = append(ordered, version)
		}
	}

	startedSmokeBatch := false
	for _, version := range ordered {
		version = strings.TrimSpace(version)
		if version == "" {
			continue
		}
		version = strings.TrimPrefix(version, "v")

		fullSuite := instanceType == "mobile" && version == fullSuiteVersion
		if instanceType == "mobile" && fullSuiteVersion != "" {
			if fullSuite {
				logger.WithField("version", version).Info("Mobile CMT batch 1/2: provisioning full-suite version (5 servers)")
			} else if !startedSmokeBatch {
				logger.Info("Mobile CMT batch 2/2: provisioning remaining versions as smoke (1 server each)")
				startedSmokeBatch = true
			}
		}

		logger.WithField("version", version).Info("Creating CMT instances for server version")

		var versionInstances []*E2EInstance
		if instanceType == "mobile" {
			var err error
			versionInstances, err = s.createMobileCMTInstances(provisionCtx, repoName, version, fullSuite, logger)
			if err != nil {
				logger.WithError(err).Errorf("Failed to create topology for version %s; dropping this version from the CMT matrix", version)
				droppedVersions = append(droppedVersions, version)
				continue
			}
		} else {
			instance, err := s.createSingleCMTInstance(provisionCtx, repoName, instanceType, version, "", logger)
			if err != nil {
				logger.WithError(err).Errorf("Failed to create instance for version %s; dropping this version from the CMT matrix", version)
				droppedVersions = append(droppedVersions, version)
				continue
			}
			versionInstances = []*E2EInstance{instance}
		}
		expectedInstances := 1
		if instanceType == "mobile" && fullSuite {
			expectedInstances = len(mobileE2EPlatforms)
		}
		if len(versionInstances) != expectedInstances {
			logger.Errorf("Incomplete CMT topology for version %s (got %d, want %d); dropping this version", version, len(versionInstances), expectedInstances)
			s.destroyE2EInstances(versionInstances, logger)
			droppedVersions = append(droppedVersions, version)
			continue
		}

		allInstances = append(allInstances, versionInstances...)
		validVersions = append(validVersions, version)
	}
	return allInstances, validVersions, droppedVersions
}

// retryDroppedCMTVersions re-provisions versions that failed the first pass and dispatches a follow-up matrix (one retry).
// Delay lives in cmtRunDroppedRetry — this function runs immediately when called.
func (s *Server) retryDroppedCMTVersions(repoOwner, repoName, instanceType, branch, sha string, runID int64, droppedVersions []string, fullSuiteVersion string, logger logrus.FieldLogger) {
	logger = logger.WithFields(logrus.Fields{
		"cmt_retry":        true,
		"dropped_versions": droppedVersions,
		"sha":              sha,
	})

	instances, recovered, stillDropped := cmtProvisionVersions(s, repoName, instanceType, droppedVersions, fullSuiteVersion, logger)
	if len(stillDropped) > 0 {
		logger.WithField("still_dropped", stillDropped).Error("CMT retry still could not provision some server versions")
		s.logErrorToMattermost("CMT retry on %s/%s (%s) still failed for version(s) %s — those versions remain uncovered for this CMT run.",
			repoOwner, repoName, branch, strings.Join(stillDropped, ", "))
	}
	if len(instances) == 0 {
		logger.Error("CMT retry recovered no versions; no follow-up matrix to dispatch")
		return
	}

	// If this retry was responsible for the full-suite version and it still failed, do not
	// dispatch smoke-only survivors (same rule as primary). Smoke-only retries after a
	// successful primary (full suite already running) are allowed — fullSuiteVersion is not
	// among droppedVersions in that case.
	fullSuiteNeeded := instanceType == "mobile" && fullSuiteVersion != "" && cmtVersionsContain(droppedVersions, fullSuiteVersion)
	if fullSuiteNeeded && !cmtVersionsContain(recovered, fullSuiteVersion) {
		msg := fmt.Sprintf("refusing to dispatch mobile CMT retry without full-suite version %s", fullSuiteVersion)
		logger.WithFields(logrus.Fields{
			"full_suite_version": fullSuiteVersion,
			"recovered_versions": recovered,
			"still_dropped":      stillDropped,
		}).Error(msg)
		s.logErrorToMattermost("CMT retry on %s/%s (%s): %s. Destroying smoke-only survivors.",
			repoOwner, repoName, branch, msg)
		s.destroyE2EInstances(instances, logger)
		return
	}

	logger.WithField("recovered_versions", recovered).Info("CMT retry recovered versions; dispatching follow-up matrix")
	s.logErrorToMattermost("CMT retry on %s/%s (%s) recovered version(s) %s — dispatching follow-up CMT matrix.",
		repoOwner, repoName, branch, strings.Join(recovered, ", "))
	cmtDispatchAndTrack(s, repoOwner, repoName, instanceType, branch, runID, recovered, instances, logger)
}

// dispatchAndTrackCMT builds CMT_MATRIX, dispatches compatibility-matrix-testing.yml, and tracks instances for cleanup.
func (s *Server) dispatchAndTrackCMT(repoOwner, repoName, instanceType, branch string, runID int64, versions []string, instances []*E2EInstance, logger logrus.FieldLogger) {
	logger.WithField("totalInstances", len(instances)).Info("CMT instances ready, dispatching test workflow")

	var cmtMatrixJSON string
	var buildErr error
	if instanceType == "desktop" {
		cmtMatrixJSON, buildErr = buildDesktopCMTMatrixJSON(versions, instances)
	} else {
		cmtMatrixJSON, buildErr = buildMobileCMTMatrixJSON(versions, instances)
	}
	if buildErr != nil {
		logger.WithError(buildErr).Error("Failed to build CMT_MATRIX JSON")
		s.destroyE2EInstances(instances, logger)
		return
	}

	dispatchLock := s.cmtDispatchMutex(repoName)
	dispatchLock.Lock()
	defer dispatchLock.Unlock()

	testRunID, err := s.dispatchCMTWorkflow(repoOwner, repoName, branch, cmtMatrixJSON, instanceType, logger)
	if err != nil {
		logger.WithError(err).Error("Failed to dispatch compatibility-matrix-testing.yml")
		s.destroyE2EInstances(instances, logger)
		return
	}

	if testRunID == 0 {
		// Dispatch succeeded but run id unresolved — leave instances for the periodic stale-scan.
		// Do not tear down: GH may already be running tests against these URLs.
		logger.WithField("trigger_run", runID).Warn("CMT dispatched but test run id unresolved; instances left to periodic stale-scan backstop")
		return
	}

	key := cmtInstanceKey(repoName, testRunID)
	s.e2eInstancesLock.Lock()
	s.e2eInstances[key] = instances
	s.e2eInstancesLock.Unlock()

	logger.WithFields(logrus.Fields{
		"tracking_key": key,
		"test_run_id":  testRunID,
		"trigger_run":  runID,
	}).Info("CMT workflow dispatched successfully; instances tracked for cleanup")
}

// createSingleCMTInstance creates one Mattermost cloud instance for a CMT server version.
func (s *Server) createSingleCMTInstance(ctx context.Context, repoName, instanceType, version, platform string, logger logrus.FieldLogger) (*E2EInstance, error) {
	sanitizedVersion := sanitizeForDNS(version)
	nameFn := func() string {
		nameParts := []string{instanceType, sanitizedVersion}
		if platform != "" {
			nameParts = append(nameParts, platform)
		}
		nameParts = append(nameParts, e2eUniqueSuffix())
		return e2eInstanceName(s.Config.DNSNameTestServer, nameParts...)
	}

	username := s.Config.E2EUsername
	password := s.getE2EPassword(instanceType)

	instance, err := s.createCloudInstallationWithRetry(ctx, nameFn, version, username, password, instanceType, logger)
	if instance != nil {
		instance.Platform = platform
	}
	return instance, err
}

// createMobileCMTInstances creates the mobile topology for a server version (full suite or single site-3 smoke).
func (s *Server) createMobileCMTInstances(ctx context.Context, repoName, version string, fullSuite bool, logger logrus.FieldLogger) ([]*E2EInstance, error) {
	platforms := []string{"site-3"}
	if fullSuite {
		platforms = mobileE2EPlatforms
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		instance *E2EInstance
		err      error
	}
	results := make([]result, len(platforms))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, platform := range platforms {
		wg.Add(1)
		go func(idx int, platform string) {
			defer wg.Done()
			instance, err := s.createSingleCMTInstance(ctx, repoName, "mobile", version, platform, logger)
			results[idx] = result{instance: instance, err: err}
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
			}
		}(i, platform)
	}
	wg.Wait()

	instances := make([]*E2EInstance, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			continue
		}
		instances = append(instances, result.instance)
	}
	if firstErr != nil {
		s.destroyE2EInstances(instances, logger)
		return nil, firstErr
	}
	return instances, nil
}

// dispatchCMTWorkflow dispatches compatibility-matrix-testing.yml and polls for the run id used to key cleanup.
func (s *Server) dispatchCMTWorkflow(repoOwner, repoName, branch, cmtMatrixJSON, instanceType string, logger logrus.FieldLogger) (int64, error) {
	client, err := s.newCMTGithubClient(logger)
	if err != nil {
		return 0, err
	}

	preDispatchRunIDs, snapErr := s.listExistingCMTRunIDs(repoOwner, repoName, branch, logger)
	if snapErr != nil {
		logger.WithError(snapErr).Warn("Failed to snapshot pre-dispatch CMT run ids; poll will only accept ids not seen before dispatch")
		preDispatchRunIDs = map[int64]bool{}
	}

	workflowInputs := map[string]interface{}{
		"CMT_MATRIX": cmtMatrixJSON,
	}
	if instanceType == "desktop" {
		workflowInputs["DESKTOP_VERSION"] = branch
	} else {
		workflowInputs["MOBILE_VERSION"] = branch
	}

	logger.WithFields(logrus.Fields{
		"ref":          branch,
		"instanceType": instanceType,
	}).Debug("Dispatching compatibility-matrix-testing.yml")

	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dispatchCancel()

	req, err := client.NewRequest("POST",
		fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches",
			url.PathEscape(repoOwner), url.PathEscape(repoName), url.PathEscape("compatibility-matrix-testing.yml")),
		map[string]interface{}{
			"ref":    branch,
			"inputs": workflowInputs,
		})
	if err != nil {
		return 0, fmt.Errorf("failed to create CMT workflow dispatch request: %w", err)
	}

	resp, err := client.Do(dispatchCtx, req, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to dispatch compatibility-matrix-testing.yml: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("unexpected status %d from compatibility-matrix-testing.yml dispatch: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	testRunID, err := s.pollDispatchedWorkflowRun(repoOwner, repoName, "compatibility-matrix-testing.yml", branch, preDispatchRunIDs, logger)
	if err != nil {
		logger.WithError(err).Warn("Dispatched compatibility-matrix-testing.yml but could not resolve test run id within poll deadline")
		return 0, nil
	}

	logger.WithField("test_run_id", testRunID).Info("compatibility-matrix-testing.yml dispatched successfully")
	return testRunID, nil
}

// listExistingCMTRunIDs returns recent run ids for compatibility-matrix-testing.yml on branch, snapshotted before dispatch.
func (s *Server) listExistingCMTRunIDs(repoOwner, repoName, branch string, logger logrus.FieldLogger) (map[int64]bool, error) {
	runs, err := s.listCMTRuns(repoOwner, repoName, "compatibility-matrix-testing.yml", branch)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]bool, len(runs))
	for _, run := range runs {
		ids[run.ID] = true
	}
	logger.WithField("pre_dispatch_run_count", len(ids)).Debug("Snapshotted pre-dispatch CMT run ids")
	return ids, nil
}

// cmtWorkflowRun is the minimal slice of the GitHub workflow_run object we need.
type cmtWorkflowRun struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

// listCMTRuns fetches up to the 10 most-recent workflow_dispatch runs for workflowFile on branch.
func (s *Server) listCMTRuns(repoOwner, repoName, workflowFile, branch string) ([]cmtWorkflowRun, error) {
	client, err := s.newCMTGithubClient(s.Logger)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	listURL := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&branch=%s&per_page=10",
		url.PathEscape(repoOwner), url.PathEscape(repoName), url.PathEscape(workflowFile), url.QueryEscape(branch))
	req, err := client.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow runs list request: %w", err)
	}
	var resp struct {
		WorkflowRuns []cmtWorkflowRun `json:"workflow_runs"`
	}
	if _, err = client.Do(ctx, req, &resp); err != nil {
		return nil, fmt.Errorf("failed to list workflow runs: %w", err)
	}
	return resp.WorkflowRuns, nil
}
