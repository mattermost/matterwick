// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
)

// CheckLimitRateAndSleep checks the api rate and sleep if needed
func (s *Server) CheckLimitRateAndSleep() {
	s.Logger.Info("Checking the rate limit on Github and will sleep if need...")

	client := newGithubClient(s.Config.GithubAccessToken)
	rate, _, err := client.RateLimits(context.Background())
	if err != nil {
		s.Logger.WithError(err).Error("Error getting the rate limit")
		time.Sleep(30 * time.Second)
		return
	}
	s.Logger.WithFields(logrus.Fields{
		"Remaining Rate": rate.Core.Remaining,
		"Limit Rate":     rate.Core.Limit,
	}).Info("Current rate limit")
	if rate.Core.Remaining <= s.Config.GitHubTokenReserve {
		sleepDuration := time.Until(rate.Core.Reset.Time) + (time.Second * 10)
		if sleepDuration > 0 {
			s.Logger.WithFields(logrus.Fields{
				"Minimum":    s.Config.GitHubTokenReserve,
				"Sleep time": sleepDuration,
			}).Error("--Rate Limiting-- Tokens reached minimum reserve. Sleeping until reset in")
			time.Sleep(sleepDuration)
		}
	}
}

// CheckLimitRateAndAbortRequest checks the api rate and abort the request if needed
func (s *Server) CheckLimitRateAndAbortRequest() bool {
	s.Logger.Info("Checking the rate limit on Github and will abort request if need...")

	client := newGithubClient(s.Config.GithubAccessToken)
	rate, _, err := client.RateLimits(context.Background())
	if err != nil {
		s.Logger.WithError(err).Error("Error getting the rate limit")
		time.Sleep(30 * time.Second)
		return false
	}
	s.Logger.WithFields(logrus.Fields{
		"Remaining Rate": rate.Core.Remaining,
		"Limit Rate":     rate.Core.Limit,
	}).Info("Current rate limit")
	if rate.Core.Remaining <= s.Config.GitHubTokenReserve {
		s.Logger.Error("Request will be aborted...")
		return true
	}
	return false
}
