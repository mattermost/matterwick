package server

import (
	"fmt"
	"strings"

	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

func (s *Server) logErrorToMattermost(msg string, args ...interface{}) {
	if s.Config.MattermostWebhookURL == "" {
		s.Logger.Warn("No Mattermost webhook URL set: unable to send message")
		return
	}

	webhookMessage := fmt.Sprintf(msg, args...)
	s.Logger.WithField("message", webhookMessage).Debug("Sending Mattermost message")

	if s.Config.MattermostWebhookFooter != "" {
		webhookMessage += "\n---\n" + s.Config.MattermostWebhookFooter
	}

	webhookRequest := &WebhookRequest{Username: "MatterWick", Text: webhookMessage}

	if err := s.sendToWebhook(webhookRequest); err != nil {
		s.Logger.WithError(err).Error("Unable to post to Mattermost webhook")
	}
}

func (s *Server) logPrettyErrorToMattermost(msg string, pr *model.PullRequest, err error, additionalFields map[string]string, logger logrus.FieldLogger) {
	if s.Config.MattermostWebhookURL == "" {
		logger.Warn("No Mattermost webhook URL set: unable to send message")
		return
	}

	logger.WithField("message", msg).Debug("Sending Mattermost message")

	fullMessage := fmt.Sprintf("%s\n---\nError: %s\nRepository: %s/%s\nPull Request: %d [ status=%s ]\nURL: %s\n",
		msg,
		err,
		pr.RepoOwner, pr.RepoName,
		pr.Number, pr.State,
		pr.URL,
	)
	for key, value := range additionalFields {
		fullMessage = fullMessage + fmt.Sprintf("%s: %s\n", key, value)
	}
	fullMessage = fullMessage + s.Config.MattermostWebhookFooter

	webhookRequest := &WebhookRequest{Username: "MatterWick", Text: fullMessage}

	if err := s.sendToWebhook(webhookRequest); err != nil {
		logger.WithError(err).Error("Unable to post to Mattermost webhook")
	}
}

// NewBool return a bool pointer
func NewBool(b bool) *bool { return &b }

// NewInt return an int pointer
func NewInt(n int) *int { return &n }

// NewInt64 return an int64 pointer
func NewInt64(n int64) *int64 { return &n }

// NewInt32 return an int32 pointer
func NewInt32(n int32) *int32 { return &n }

// NewString return a string pointer
func NewString(s string) *string { return &s }

// splitCommaSeparated splits a comma separated string into a slice of strings.
// It handles quoted strings and trims whitespace from the results.
func splitCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}

	// Remove outer quotes if present
	s = strings.TrimSpace(s)
	if (strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'")) ||
		(strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) {
		s = s[1 : len(s)-1]
	}

	// Split and clean the parts
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}

	return result
}

// parseEnvArg parses a string argument in the comma separated format "VAR1=VAL1,VAR2=VAL2" into a cloudModel.EnvVarMap
func parseEnvArg(arg string) (cloudModel.EnvVarMap, error) {
	if arg == "" {
		return nil, fmt.Errorf("invalid empty argument")
	}

	kvs := splitCommaSeparated(arg)
	if len(kvs) == 0 {
		return nil, fmt.Errorf("no key/val pairs found")
	}

	m := cloudModel.EnvVarMap{}
	for _, kv := range kvs {
		fields := strings.SplitN(kv, "=", 2)

		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid key/val pair: %q", kv)
		}

		k := strings.TrimSpace(fields[0])

		if k == "" {
			return nil, fmt.Errorf("invalid key/val pair: %q", kv)
		}

		v := fields[1]

		if _, ok := m[k]; ok {
			return nil, fmt.Errorf("duplicate key: %q", k)
		}

		m[k] = cloudModel.EnvVar{Value: v}
	}

	return m, nil
}
