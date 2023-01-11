// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/matterwick/server"
	"github.com/pkg/errors"
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "config-matterwick.json", "")
	flag.Parse()

	config, err := server.GetConfig(configFile)
	if err != nil {
		fmt.Println(errors.Wrap(err, "unable to load server config"))
		os.Exit(1)
	}
	server.SetupLogging(config)

	mlog.Info("Loaded config", mlog.String("filename", configFile))

	s := server.New(config)

	s.Start()
	defer s.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)
	<-sig
}
