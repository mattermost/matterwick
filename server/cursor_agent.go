// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/internal/cloudtools"
	"github.com/mattermost/matterwick/internal/cursor"
	"github.com/mattermost/matterwick/internal/spinwick"
	"github.com/mattermost/matterwick/model"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// cursorAgentMarkerWindow is how long before the label event a Cursor agent's request marker may have been posted.
	cursorAgentMarkerWindow = 5 * time.Minute
	// cursorAgentMarkerSkew allows for small clock differences between GitHub's timestamps and Matterwick's.
	cursorAgentMarkerSkew = 30 * time.Second
	// cursorFollowUpTimeout bounds one delivery to a Cursor agent, including retries while the agent is busy.
	cursorFollowUpTimeout = 3 * time.Minute
	// cursorLookupTimeout bounds the GitHub calls that find the requesting agent.
	cursorLookupTimeout = 30 * time.Second
	// cursorAppSlug is the slug of the Cursor GitHub app that creates the check runs.
	cursorAppSlug = "cursor"

	cursorCredentialsSentLine = "**Credentials:** Sent to the Cursor agent that requested this server."
)

const cursorAgentIDRegexp = `bc-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

var (
	// cursorAgentMarker matches the hidden marker a Cursor agent posts in a PR comment just before it applies
	// the label: <!-- cursor-agent-request: bc-... -->.
	cursorAgentMarker = regexp.MustCompile(`<!--\s*cursor-agent-request:\s*(` + cursorAgentIDRegexp + `)\s*-->`)
	// cursorAgentLink matches the agent ID in the link of a Cursor check run: https://cursor.com/agents/bc-...
	cursorAgentLink = regexp.MustCompile(`/agents/(` + cursorAgentIDRegexp + `)(?:[/?#]|$)`)
)

// cursorDeliveryEnabled reports whether results may be delivered to Cursor agents.
func (s *Server) cursorDeliveryEnabled() bool {
	return s.Config.CursorAPIKey != "" && len(s.Config.CursorAgentCheckNames) > 0
}

// rememberCursorAgentRequest looks up whether a Cursor agent, and not a person, applied the HA label, and stores
// the agent's ID under ownerID for when the build finishes. A label applied by a person leaves nothing stored, so
// the build behaves exactly as it always has. Any lookup problem is logged and treated the same way.
func (s *Server) rememberCursorAgentRequest(pr *model.PullRequest, ownerID, sender string, labelTime time.Time) {
	s.setCursorAgentRequest(ownerID, "")
	if !s.cursorDeliveryEnabled() {
		return
	}

	logger := s.Logger.WithFields(logrus.Fields{"repo_name": pr.RepoName, "pr": pr.Number})
	agentID, err := s.findRequestingCursorAgent(pr, sender, labelTime, logger)
	if err != nil {
		logger.WithError(err).Warn("Unable to look up the requesting Cursor agent, so the logins go to the Mattermost credentials channel")
		return
	}
	if agentID == "" {
		logger.Info("No Cursor agent requested this SpinWick, so the logins go to the Mattermost credentials channel")
		return
	}

	logger.WithField("cursor_agent", agentID).Info("SpinWick was requested by a Cursor agent")
	s.setCursorAgentRequest(ownerID, agentID)
}

func (s *Server) setCursorAgentRequest(ownerID, agentID string) {
	s.cursorAgentRequestsLock.Lock()
	defer s.cursorAgentRequestsLock.Unlock()

	if agentID == "" {
		delete(s.cursorAgentRequests, ownerID)
		return
	}
	if s.cursorAgentRequests == nil {
		s.cursorAgentRequests = make(map[string]string)
	}
	s.cursorAgentRequests[ownerID] = agentID
}

// takeCursorAgentRequest returns the stored agent ID for ownerID, if any, and forgets it.
func (s *Server) takeCursorAgentRequest(ownerID string) string {
	s.cursorAgentRequestsLock.Lock()
	defer s.cursorAgentRequestsLock.Unlock()

	agentID := s.cursorAgentRequests[ownerID]
	delete(s.cursorAgentRequests, ownerID)
	return agentID
}

// findRequestingCursorAgent returns the ID of the Cursor agent that asked for a SpinWick, or "" if the label was
// not applied by an agent. The agent leaves a marker comment, authored by the same user who applied the label, just
// before applying it. The marker is only a claim: the ID is accepted only if it is the agent behind a Cursor check
// run, with one of the configured names, on the pull request's head commit.
func (s *Server) findRequestingCursorAgent(pr *model.PullRequest, sender string, labelTime time.Time, logger logrus.FieldLogger) (string, error) {
	if sender == "" {
		return "", nil
	}

	client, err := s.newCMTGithubClient(logger)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorLookupTimeout)
	defer cancel()

	agentID, err := newestCursorAgentMarker(ctx, client, pr, sender, labelTime)
	if err != nil || agentID == "" {
		return "", err
	}

	known, err := s.cursorCheckBelongsToAgent(ctx, client, pr, agentID)
	if err != nil {
		return "", err
	}
	if !known {
		logger.WithField("cursor_agent", agentID).Warn("Found a Cursor request marker, but no Cursor check on the head commit belongs to that agent. Ignoring it")
		return "", nil
	}
	return agentID, nil
}

// newestCursorAgentMarker returns the agent ID in the newest request marker posted by sender shortly before labelTime.
func newestCursorAgentMarker(ctx context.Context, client *github.Client, pr *model.PullRequest, sender string, labelTime time.Time) (string, error) {
	earliest := labelTime.Add(-cursorAgentMarkerWindow)
	latest := labelTime.Add(cursorAgentMarkerSkew)
	since := earliest.Add(-time.Minute)

	opts := &github.IssueListCommentsOptions{
		Since:       &since,
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var newestID string
	var newestAt time.Time
	for {
		comments, resp, err := client.Issues.ListComments(ctx, pr.RepoOwner, pr.RepoName, pr.Number, opts)
		if err != nil {
			return "", errors.Wrap(err, "unable to list the pull request comments")
		}

		for _, comment := range comments {
			if !strings.EqualFold(comment.GetUser().GetLogin(), sender) {
				continue
			}
			created := comment.GetCreatedAt()
			if created.Before(earliest) || created.After(latest) {
				continue
			}
			match := cursorAgentMarker.FindStringSubmatch(comment.GetBody())
			if match == nil {
				continue
			}
			if newestID == "" || created.After(newestAt) {
				newestID, newestAt = match[1], created
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return newestID, nil
}

// cursorCheckBelongsToAgent reports whether a Cursor check run named in the configuration exists on the pull
// request's head commit with a link that points to agentID.
func (s *Server) cursorCheckBelongsToAgent(ctx context.Context, client *github.Client, pr *model.PullRequest, agentID string) (bool, error) {
	for _, name := range s.Config.CursorAgentCheckNames {
		checkName := name
		result, _, err := client.Checks.ListCheckRunsForRef(ctx, pr.RepoOwner, pr.RepoName, pr.Sha, &github.ListCheckRunsOptions{
			CheckName:   &checkName,
			ListOptions: github.ListOptions{PerPage: 100},
		})
		if err != nil {
			return false, errors.Wrapf(err, "unable to list the check runs named %q", name)
		}

		for _, run := range result.CheckRuns {
			if run.GetApp().GetSlug() != cursorAppSlug || run.GetName() != name {
				continue
			}
			if match := cursorAgentLink.FindStringSubmatch(run.GetDetailsURL()); match != nil && strings.EqualFold(match[1], agentID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// sendResultToCursorAgent delivers text to the agent as a follow-up message.
func (s *Server) sendResultToCursorAgent(agentID, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cursorFollowUpTimeout)
	defer cancel()

	_, err := cursor.NewClient(s.Config.CursorAPIBaseURL, s.Config.CursorAPIKey).SendFollowUp(ctx, agentID, text)
	return err
}

// deliverSpinwickSuccess reports a finished build. If a Cursor agent requested the server, it gets the server URL
// and logins and nothing is posted to the Mattermost credentials channel. In every other case, including a failed
// delivery, the existing behaviour applies.
func (s *Server) deliverSpinwickSuccess(pr *model.PullRequest, installation *cloudModel.InstallationDTO, sysadminPassword, userPassword, extraInfo, ownerID string, logger logrus.FieldLogger) {
	if agentID := s.takeCursorAgentRequest(ownerID); agentID != "" {
		if s.sendSpinwickSuccessToCursorAgent(pr, installation, sysadminPassword, userPassword, extraInfo, agentID, logger) {
			return
		}
		logger.WithField("cursor_agent", agentID).Warn("Could not deliver the logins to the Cursor agent, so they go to the Mattermost credentials channel")
	}
	s.sendSpinwickSuccessToMattermost(pr, installation, sysadminPassword, userPassword, extraInfo, logger)
}

// sendSpinwickSuccessToCursorAgent returns true when the agent received the logins.
func (s *Server) sendSpinwickSuccessToCursorAgent(pr *model.PullRequest, installation *cloudModel.InstallationDTO, sysadminPassword, userPassword, extraInfo, agentID string, logger logrus.FieldLogger) bool {
	spinwickURL := fmt.Sprintf("https://%s", cloudtools.GetInstallationDNSFromDNSRecords(installation))
	message := "Status: ready\n\n" + s.spinwickCredentialsMessage(pr, installation, spinwickURL, sysadminPassword, userPassword, extraInfo, false)

	if err := s.sendResultToCursorAgent(agentID, message); err != nil {
		logger.WithError(err).WithField("cursor_agent", agentID).Error("Unable to send the SpinWick logins to the Cursor agent")
		return false
	}

	logger.WithField("cursor_agent", agentID).Info("Sent the SpinWick logins to the Cursor agent")
	s.sendSpinwickPublicComment(pr, installation, extraInfo, cursorCredentialsSentLine)
	return true
}

// deliverSpinwickFailure tells the requesting Cursor agent, if there is one, that the build failed, so it is not
// left waiting for logins that will never come.
func (s *Server) deliverSpinwickFailure(pr *model.PullRequest, request *spinwick.Request, ownerID string, logger logrus.FieldLogger) {
	agentID := s.takeCursorAgentRequest(ownerID)
	if agentID == "" {
		return
	}

	if err := s.sendResultToCursorAgent(agentID, spinwickFailureMessage(pr, request)); err != nil {
		logger.WithError(err).WithField("cursor_agent", agentID).Error("Unable to tell the Cursor agent that the SpinWick build failed")
		return
	}
	logger.WithField("cursor_agent", agentID).Info("Told the Cursor agent that the SpinWick build failed")
}

// spinwickFailureMessage is the follow-up sent to an agent when its build fails. It never contains credentials.
func spinwickFailureMessage(pr *model.PullRequest, request *spinwick.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Status: failed\n\nThe test server for PR #%d (%s/%s) could not be created.\n", pr.Number, pr.RepoOwner, pr.RepoName)
	if request.Error != nil {
		fmt.Fprintf(&b, "\n**Error:** %s\n", request.Error.Error())
	}
	if request.Aborted {
		b.WriteString("\nThe request was aborted, so no server was built.\n")
	}
	if request.InstallationID != "" && request.InstallationID != "n/a" {
		fmt.Fprintf(&b, "\n**Installation ID:** `%s`\n", request.InstallationID)
	}
	b.WriteString("\nThe SpinWick label has been removed from the pull request.")
	return b.String()
}

const (
	installationDeletionTimeout  = 10 * time.Minute
	installationDeletionInterval = 10 * time.Second
)

// waitForInstallationDeletion blocks until the installation has been deleted.
func (s *Server) waitForInstallationDeletion(installationID string, logger logrus.FieldLogger) error {
	ctx, cancel := context.WithTimeout(context.Background(), installationDeletionTimeout)
	defer cancel()

	return pollInstallationDeleted(ctx, installationDeletionInterval, func() (string, error) {
		installation, err := s.CloudClient.GetInstallation(installationID, &cloudModel.GetInstallationRequest{})
		if err != nil {
			return "", err
		}
		if installation == nil {
			return cloudModel.InstallationStateDeleted, nil
		}
		logger.WithFields(logrus.Fields{"installation_id": installationID, "state": installation.State}).Debug("Waiting for the installation to be deleted")
		return installation.State, nil
	})
}

// pollInstallationDeleted calls getState every interval until the installation is deleted, its deletion fails, a
// lookup fails, or ctx ends.
func pollInstallationDeleted(ctx context.Context, interval time.Duration, getState func() (string, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		state, err := getState()
		if err != nil {
			return errors.Wrap(err, "unable to check whether the previous installation was deleted")
		}
		switch state {
		case cloudModel.InstallationStateDeleted:
			return nil
		case cloudModel.InstallationStateDeletionFailed:
			return errors.New("the previous installation could not be deleted")
		}

		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for the previous installation to be deleted")
		case <-ticker.C:
		}
	}
}
