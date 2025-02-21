package server

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-github/v32/github"
	cloudModel "github.com/mattermost/mattermost-cloud/model"
	"github.com/mattermost/matterwick/model"
	"github.com/sirupsen/logrus"
)

const (
	slashCommandSpinWick  = "/spinwick"
	slashCommandShrugWick = "/shrugwick"
)

type (
	spinWickCreateHandlerFn       func(envMap cloudModel.EnvVarMap, size string)
	spinWickUpdateHandlerFn       func(envMap cloudModel.EnvVarMap)
	spinWickDeleteHandlerFn       func()
	spinWickSlashCommandsHandlers struct {
		createHandler spinWickCreateHandlerFn
		updateHandler spinWickUpdateHandlerFn
		deleteHandler spinWickDeleteHandlerFn
	}
	spinWickSlashCommandArgs struct {
		envMap cloudModel.EnvVarMap
		size   string
	}
)

func (s *Server) handleSlashCommand(cmd string, ev *github.IssueCommentEvent) {
	s.Logger.WithField("cmd", cmd).Info("handling slash command")

	if os.Getenv("MATTERWICK_LOCAL_TESTING") != "true" {
		// Ensure user sending the command has permissions to do so.
		if ok := s.checkUserPermission(ev.GetSender().GetLogin(), ev.GetRepo().GetOwner().GetLogin()); !ok {
			s.Logger.Error("no permission")
			return
		}
	}

	args := strings.Fields(cmd)
	if len(args) == 0 {
		s.Logger.WithField("cmd", cmd).Error("no args")
		return
	}

	githubPR, err := s.getPullRequestFromIssue(ev.GetIssue(), ev.GetRepo())
	if err != nil {
		logger.WithError(err).Error("failed to get GitHub PR")
		return
	}

	pr, err := s.GetPullRequestFromGithub(githubPR)
	if err != nil {
		logger.WithError(err).Error("failed to get PR")
		return
	}

	spinWickHandlers := spinWickSlashCommandsHandlers{
		createHandler: func(envMap cloudModel.EnvVarMap, size string) {
			spinwick := model.NewSpinwick(pr.RepoName, pr.Number, s.Config.DNSNameTestServer)
			s.envMapsLock.Lock()
			s.envMaps[spinwick.RepeatableID] = envMap
			s.envMapsLock.Unlock()

			label := s.Config.SetupSpinWick
			if size == "miniHA" {
				label = s.Config.SetupSpinWickHA
			}
			s.addLabel(pr.RepoOwner, pr.RepoName, pr.Number, label)
		},
		updateHandler: func(envMap cloudModel.EnvVarMap) {
			spinwick := model.NewSpinwick(pr.RepoName, pr.Number, s.Config.DNSNameTestServer)
			s.envMapsLock.Lock()
			s.envMaps[spinwick.RepeatableID] = envMap
			s.envMapsLock.Unlock()

			s.handleSynchronizeSpinwick(pr, spinwick.RepeatableID, true)
		},
		deleteHandler: func() {
			for _, label := range pr.Labels {
				if s.isSpinWickLabel(label) {
					s.removeLabel(pr.RepoOwner, pr.RepoName, pr.Number, label)
				}
			}
		},
	}

	switch args[0] {
	case slashCommandSpinWick:
		output, err := s.handleSpinWickSlashCommand(args[1:], spinWickHandlers)
		if err != nil {
			s.Logger.WithError(err).Error("failed to handle spinwick command")
		}
		if output != "" {
			s.sendGitHubComment(ev.GetRepo().GetOwner().GetLogin(),
				ev.GetRepo().GetName(),
				ev.GetIssue().GetNumber(), fmt.Sprintf("```\n%s\n```", output))
		}
	case slashCommandShrugWick:
		s.handleShrugWick(ev)
	default:
		s.Logger.WithField("cmd", cmd).Error("invalid slash command")
	}
}

func (s *Server) parseSpinwickSlashCommandArgs(args []string, isUpdate bool) (spinWickSlashCommandArgs, string, error) {
	var parsedArgs spinWickSlashCommandArgs

	var outBuf bytes.Buffer
	flagset := flag.NewFlagSet("spinwick", flag.ContinueOnError)
	flagset.SetOutput(&outBuf)

	var env string
	var clearEnv string
	var size string
	flagset.StringVar(&env, "env", "", "An optional comma-separated list of environment variables. Example: VAR1=VAl1,VAR2=VAL2")
	if isUpdate {
		flagset.StringVar(&clearEnv, "clear-env", "", "An optional comma-separated list of environment variables to clear. Example: VAR1,VAR2")
	} else {
		flagset.StringVar(&size, "size", "miniSingleton", "Size of the Mattermost installation e.g. 'miniSingleton' or 'miniHA'")
	}

	err := flagset.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return parsedArgs, outBuf.String(), err
	} else if err != nil {
		return parsedArgs, outBuf.String(), fmt.Errorf("failed to parse args: %w", err)
	}

	s.Logger.WithField("env", env).Info("parsed env vars")

	envMap := make(cloudModel.EnvVarMap)
	if env != "" {
		envMap, err = parseEnvArg(env)
		if err != nil {
			return parsedArgs, err.Error(), fmt.Errorf("failed to parse env vars: %w", err)
		}
	}

	if clearEnv != "" {
		keys := splitCommaSeparated(clearEnv)
		for _, k := range keys {
			envMap[k] = cloudModel.EnvVar{}
		}
	}

	parsedArgs.envMap = envMap
	parsedArgs.size = size

	return parsedArgs, "", nil
}

var spinwickSlashCommandUsageString = `Usage: /spinwick <command> [args]

Available commands:
  create  Create a new Mattermost spinwick installation
  update  Update the existing Mattermost spinwick installation
  delete  Delete the existing Mattermost spinwick installation
`

func (s *Server) handleSpinWickSlashCommand(args []string, handlers spinWickSlashCommandsHandlers) (string, error) {
	if len(args) == 0 {
		// return help
		return spinwickSlashCommandUsageString, nil
	}

	switch args[0] {
	case "create":
		s.Logger.WithField("args", args).Info("handling spinwick create command")

		parsedArgs, output, err := s.parseSpinwickSlashCommandArgs(args[1:], false)
		if err != nil {
			return output, fmt.Errorf("failed to parse spinwick command args: %w", err)
		}

		if handlers.createHandler == nil {
			return "", fmt.Errorf("nil handler")
		}

		s.Logger.WithFields(logrus.Fields{
			"envMap": parsedArgs.envMap,
			"size":   parsedArgs.size,
		}).Info("going to create spinwick")

		handlers.createHandler(parsedArgs.envMap, parsedArgs.size)
	case "update":
		s.Logger.WithField("args", args).Info("handling spinwick update command")

		parsedArgs, output, err := s.parseSpinwickSlashCommandArgs(args[1:], true)
		if err != nil {
			return output, fmt.Errorf("failed to parse spinwick command args: %w", err)
		}

		if handlers.updateHandler == nil {
			return "", fmt.Errorf("nil handler")
		}

		s.Logger.WithFields(logrus.Fields{
			"envMap": parsedArgs.envMap,
		}).Info("going to update spinwick")

		handlers.updateHandler(parsedArgs.envMap)
	case "delete":
		s.Logger.WithField("args", args).Info("handling spinwick delete command")

		if handlers.deleteHandler == nil {
			return "", fmt.Errorf("nil handler")
		}

		s.Logger.Info("going to delete spinwick")

		handlers.deleteHandler()
	default:
		return spinwickSlashCommandUsageString, fmt.Errorf("invalid command %q", args[0])
	}

	return "", nil
}
