// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package model

// PluginInstallResult contains the result of attempting to install a plugin
type PluginInstallResult struct {
	PluginURL       string
	Success         bool
	InstallError    error
	EnableError     error
	ArtifactFound   bool
}
