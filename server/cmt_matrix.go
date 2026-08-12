// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmtServer is one entry in CMT_MATRIX. Mobile entries carry a five-server topology.
type cmtServer struct {
	Version         string `json:"version"`
	URL             string `json:"url,omitempty"`
	AndroidSite1URL string `json:"android_site_1_url,omitempty"`
	AndroidSite2URL string `json:"android_site_2_url,omitempty"`
	IOSSite1URL     string `json:"ios_site_1_url,omitempty"`
	IOSSite2URL     string `json:"ios_site_2_url,omitempty"`
	Site3URL        string `json:"site_3_url,omitempty"`
	Latest          bool   `json:"latest,omitempty"`
}

// buildDesktopCMTMatrixJSON builds CMT_MATRIX: 3 fixed environment runners × N server versions.
func buildDesktopCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type cmtEnvironment struct {
		OS     string `json:"os"`
		Runner string `json:"runner"`
	}
	type desktopCMTMatrix struct {
		Environment []cmtEnvironment `json:"environment"`
		Server      []cmtServer      `json:"server"`
	}

	// macos-13 retired; macos-26 matches desktop PR E2E. ubuntu-latest = 24.04 (libasound2t64).
	matrix := desktopCMTMatrix{
		Environment: []cmtEnvironment{
			{OS: "linux", Runner: "ubuntu-latest"},
			{OS: "macos", Runner: "macos-26"},
			{OS: "windows", Runner: "windows-2022"},
		},
	}
	if len(instances) < len(versions) {
		return "", fmt.Errorf("desktop CMT: %d versions but only %d instances", len(versions), len(instances))
	}
	for i, version := range versions {
		matrix.Server = append(matrix.Server, cmtServer{Version: version, URL: instances[i].URL})
	}

	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("failed to marshal desktop CMT matrix: %w", err)
	}
	return string(b), nil
}

// mobileCMTBlockMatchesVersion requires every instance in block to carry the expected server version.
func mobileCMTBlockMatchesVersion(block []*E2EInstance, version string) error {
	want := strings.TrimPrefix(strings.TrimSpace(version), "v")
	for _, inst := range block {
		got := strings.TrimPrefix(strings.TrimSpace(inst.ServerVersion), "v")
		if got != want {
			return fmt.Errorf("mobile CMT instance version mismatch: want %s, got %s (platform %s)", want, got, inst.Platform)
		}
	}
	return nil
}

// expandMobileSmokeURLs replicates a smoke-server URL into every mobile CMT_MATRIX site field.
func expandMobileSmokeURLs(entry *cmtServer, url string) {
	entry.AndroidSite1URL = url
	entry.AndroidSite2URL = url
	entry.IOSSite1URL = url
	entry.IOSSite2URL = url
	entry.Site3URL = url
	entry.URL = url
}

// mobileCMTBlockHasFullTopology reports whether the next len(mobileE2EPlatforms) instances cover every platform.
func mobileCMTBlockHasFullTopology(block []*E2EInstance) bool {
	if len(block) < len(mobileE2EPlatforms) {
		return false
	}
	seen := make(map[string]bool, len(mobileE2EPlatforms))
	for _, inst := range block[:len(mobileE2EPlatforms)] {
		seen[inst.Platform] = true
	}
	for _, platform := range mobileE2EPlatforms {
		if !seen[platform] {
			return false
		}
	}
	return true
}

// fillMobileCMTFullSuiteEntry maps a five-server topology block onto a CMT_MATRIX entry.
func fillMobileCMTFullSuiteEntry(entry *cmtServer, block []*E2EInstance, version string) error {
	platformToURL := make(map[string]string, len(block))
	for _, inst := range block {
		platformToURL[inst.Platform] = inst.URL
	}
	for _, platform := range mobileE2EPlatforms {
		url, ok := platformToURL[platform]
		if !ok {
			return fmt.Errorf("mobile CMT missing instance for platform %s in version %s", platform, version)
		}
		switch platform {
		case "android-site-1":
			entry.AndroidSite1URL = url
		case "android-site-2":
			entry.AndroidSite2URL = url
		case "ios-site-1":
			entry.IOSSite1URL = url
		case "ios-site-2":
			entry.IOSSite2URL = url
		case "site-3":
			entry.Site3URL = url
		}
	}
	// Back-compat for release branches that still read matrix.server.url.
	entry.URL = entry.IOSSite1URL
	return nil
}

// buildMobileCMTMatrixJSON builds CMT_MATRIX for mobile. Highest semver prefers five distinct
// platform URLs when present; otherwise expands a single smoke URL. Latest=true only when that
// highest-semver entry was filled from a full five-platform topology (not smoke degradation).
func buildMobileCMTMatrixJSON(versions []string, instances []*E2EInstance) (string, error) {
	type mobileCMTMatrix struct {
		Server []cmtServer `json:"server"`
	}

	latestIdx := -1
	var latestVer cmtVersion
	for i, version := range versions {
		v, ok := parseCMTVersion(version)
		if !ok {
			continue
		}
		if latestIdx == -1 || latestVer.less(v) {
			latestVer = v
			latestIdx = i
		}
	}
	if latestIdx == -1 && len(versions) > 0 {
		latestIdx = len(versions) - 1
	}

	var matrix mobileCMTMatrix
	offset := 0
	for i, version := range versions {
		entry := cmtServer{Version: version}
		remaining := instances[offset:]
		isLatest := i == latestIdx

		if isLatest {
			if mobileCMTBlockHasFullTopology(remaining) {
				block := remaining[:len(mobileE2EPlatforms)]
				if err := mobileCMTBlockMatchesVersion(block, version); err != nil {
					return "", err
				}
				if err := fillMobileCMTFullSuiteEntry(&entry, block, version); err != nil {
					return "", err
				}
				offset += len(mobileE2EPlatforms)
				entry.Latest = true
			} else if len(remaining) >= 1 {
				if err := mobileCMTBlockMatchesVersion(remaining[:1], version); err != nil {
					return "", err
				}
				// Highest semver was provisioned as smoke only — expand URLs but do not set Latest.
				expandMobileSmokeURLs(&entry, remaining[0].URL)
				offset++
			} else {
				return "", fmt.Errorf("mobile CMT latest version %s requires instances, got 0 remaining", version)
			}
		} else {
			if len(remaining) < 1 {
				return "", fmt.Errorf("mobile CMT smoke version %s requires 1 instance, got 0 remaining", version)
			}
			if err := mobileCMTBlockMatchesVersion(remaining[:1], version); err != nil {
				return "", err
			}
			expandMobileSmokeURLs(&entry, remaining[0].URL)
			offset++
		}
		matrix.Server = append(matrix.Server, entry)
	}
	if offset != len(instances) {
		return "", fmt.Errorf("mobile CMT instance count mismatch: consumed %d of %d for %d versions", offset, len(instances), len(versions))
	}

	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("failed to marshal mobile CMT matrix: %w", err)
	}
	return string(b), nil
}
