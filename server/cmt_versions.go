// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"sort"
	"strconv"
	"strings"
)

// cmtVersion is a parsed Mattermost release: major.minor.patch with optional -rcN.
type cmtVersion struct {
	major, minor, patch int
	rc                  int // 0 = stable, >0 = -rcN
	raw                 string
}

type cmtMinorKey struct{ major, minor int }

// parseCMTVersion parses "vX.Y.Z" or "vX.Y.Z-rcN" (leading "v" optional). Other prerelease suffixes return ok=false.
func parseCMTVersion(tag string) (cmtVersion, bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	base := raw
	rc := 0
	if i := strings.Index(base, "-rc"); i != -1 {
		n, err := strconv.Atoi(base[i+len("-rc"):])
		if err != nil {
			return cmtVersion{}, false
		}
		rc = n
		base = base[:i]
	} else if strings.Contains(base, "-") {
		return cmtVersion{}, false
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return cmtVersion{}, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	pat, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return cmtVersion{}, false
	}
	return cmtVersion{major: maj, minor: min, patch: pat, rc: rc, raw: raw}, true
}

// less sorts by (major, minor, patch, rc); stable (rc==0) sorts above -rcN for the same X.Y.Z.
func (a cmtVersion) less(b cmtVersion) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	ar, br := a.rc, b.rc
	if ar == 0 {
		ar = int(^uint(0) >> 1) // treat stable as the highest "rc" for the same patch
	}
	if br == 0 {
		br = int(^uint(0) >> 1)
	}
	return ar < br
}

const maxCMTServerVersions = 5

// maxMobileCMTServerVersions caps mobile at ESR + latest production + current RC (peak 5+1+1 servers).
const maxMobileCMTServerVersions = 3

const maxCMTESRLines = 2 // current + trailing ESR; older body-flagged ESRs are treated as EOL

// cmtVersionCapFor returns the version cap for instanceType (mobile tighter than desktop).
func cmtVersionCapFor(instanceType string) int {
	if instanceType == "mobile" {
		return maxMobileCMTServerVersions
	}
	return maxCMTServerVersions
}

// capCMTServerVersions keeps at most limit newest parseable semvers. Copies so Config.CMTServerVersions is not mutated.
func capCMTServerVersions(serverVersions []string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	if len(serverVersions) <= limit {
		return append([]string(nil), serverVersions...)
	}
	sorted := append([]string(nil), serverVersions...)
	sort.Slice(sorted, func(i, j int) bool {
		vi, oki := parseCMTVersion(sorted[i])
		vj, okj := parseCMTVersion(sorted[j])
		if oki != okj {
			return !oki // unparseable first so the newest tail keeps valid versions
		}
		if !oki {
			return sorted[i] < sorted[j]
		}
		return vi.less(vj)
	})
	return sorted[len(sorted)-limit:]
}

// spanCMTServerVersions reduces to at most limit entries while keeping both ends of the range.
func spanCMTServerVersions(serverVersions []string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	if len(serverVersions) <= limit {
		return serverVersions
	}

	parseable := make([]string, 0, len(serverVersions))
	for _, v := range serverVersions {
		if _, ok := parseCMTVersion(v); ok {
			parseable = append(parseable, v)
		}
	}
	if len(parseable) == 0 {
		return capCMTServerVersions(serverVersions, limit)
	}
	if len(parseable) <= limit {
		return parseable
	}

	sort.Slice(parseable, func(i, j int) bool {
		vi, _ := parseCMTVersion(parseable[i])
		vj, _ := parseCMTVersion(parseable[j])
		return vi.less(vj)
	})

	if limit == 1 {
		return []string{parseable[len(parseable)-1]}
	}

	last := len(parseable) - 1
	selected := make([]string, 0, limit)
	seen := make(map[int]bool, limit)
	for i := 0; i < limit; i++ {
		idx := (i*last + (limit-1)/2) / (limit - 1)
		if seen[idx] {
			continue
		}
		seen[idx] = true
		selected = append(selected, parseable[idx])
	}
	return selected
}

// cmtLatestServerVersion returns the highest parseable semver (bare, no v-). Falls back to last entry; "" if empty.
func cmtLatestServerVersion(versions []string) string {
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
	if latestIdx >= 0 {
		return latestVer.raw
	}
	if len(versions) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(versions[len(versions)-1]), "v")
}

// cmtServerVersions returns Config.CMTServerVersions if set, else auto-derived from GitHub releases.
func (s *Server) cmtServerVersions(instanceType string) []string {
	if len(s.Config.CMTServerVersions) > 0 {
		return s.Config.CMTServerVersions
	}
	if instanceType == "mobile" {
		return s.resolveMobileCMTServerVersions()
	}
	return s.resolveCMTServerVersions()
}

// resolveMobileCMTServerVersions picks newest ESR + newest non-ESR stable + current RC. Falls back to defaultCMTServerVersions.
func (s *Server) resolveMobileCMTServerVersions() []string {
	releaseSet, err := s.fetchCMTReleaseSet()
	if err != nil {
		s.Logger.WithError(err).Warn("[resolveMobileCMTServerVersions] Failed to classify releases; using default CMT versions")
		return defaultCMTServerVersions
	}

	minors := releaseSet.newestStableMinors()
	if len(minors) == 0 {
		s.Logger.Warn("[resolveMobileCMTServerVersions] No stable releases parsed; using default CMT versions")
		return defaultCMTServerVersions
	}

	seen := map[cmtMinorKey]bool{}
	chosen := make([]cmtVersion, 0, maxMobileCMTServerVersions)
	addStable := func(v cmtVersion) {
		key := cmtMinorKey{v.major, v.minor}
		if seen[key] {
			return
		}
		seen[key] = true
		chosen = append(chosen, v)
	}

	for _, v := range minors {
		if releaseSet.isESRLine(v) {
			addStable(v)
			break
		}
	}
	for _, v := range minors {
		if !releaseSet.isESRLine(v) {
			addStable(v)
			break
		}
	}
	// RC keeps its own slot even when it shares major/minor with a selected stable.
	if releaseSet.haveRC && minors[0].less(releaseSet.bestRC) {
		chosen = append(chosen, releaseSet.bestRC)
	}

	for _, v := range minors {
		if len(chosen) >= maxMobileCMTServerVersions {
			break
		}
		addStable(v)
	}

	sort.Slice(chosen, func(i, j int) bool { return chosen[i].less(chosen[j]) })
	if len(chosen) > maxMobileCMTServerVersions {
		chosen = chosen[len(chosen)-maxMobileCMTServerVersions:]
	}

	versions := make([]string, 0, len(chosen))
	for _, v := range chosen {
		versions = append(versions, v.raw)
	}
	s.Logger.WithField("versions", versions).Info("[resolveMobileCMTServerVersions] Auto-derived mobile CMT server version set (ESR + latest stable + RC)")
	return versions
}

// resolveCMTServerVersions picks newest maxCMTESRLines ESRs + latest 3 stable minors + current RC. Falls back to defaultCMTServerVersions.
func (s *Server) resolveCMTServerVersions() []string {
	releaseSet, err := s.fetchCMTReleaseSet()
	if err != nil {
		s.Logger.WithError(err).Warn("[resolveCMTServerVersions] Failed to classify releases; using default CMT versions")
		return defaultCMTServerVersions
	}

	latestStable := releaseSet.latestStable
	esrMinors := releaseSet.esrMinors
	bestRC := releaseSet.bestRC
	haveRC := releaseSet.haveRC

	if len(latestStable) == 0 {
		s.Logger.Warn("[resolveCMTServerVersions] No stable releases parsed; using default CMT versions")
		return defaultCMTServerVersions
	}

	minors := releaseSet.newestStableMinors()

	selected := map[cmtMinorKey]cmtVersion{}
	for i := 0; i < len(minors) && i < 3; i++ {
		selected[cmtMinorKey{minors[i].major, minors[i].minor}] = minors[i]
	}
	// Keep only newest maxCMTESRLines ESR minors; older body-flagged lines are EOL noise.
	esrChosen := make([]cmtVersion, 0, len(esrMinors))
	for k := range esrMinors {
		if v, ok := latestStable[k]; ok {
			esrChosen = append(esrChosen, v)
		}
	}
	sort.Slice(esrChosen, func(i, j int) bool { return esrChosen[j].less(esrChosen[i]) })
	keptESR := map[cmtMinorKey]bool{}
	for i := 0; i < len(esrChosen) && i < maxCMTESRLines; i++ {
		k := cmtMinorKey{esrChosen[i].major, esrChosen[i].minor}
		selected[k] = esrChosen[i]
		keptESR[k] = true
	}

	chosen := make([]cmtVersion, 0, len(selected)+1)
	for _, v := range selected {
		chosen = append(chosen, v)
	}
	if haveRC && minors[0].less(bestRC) {
		chosen = append(chosen, bestRC)
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].less(chosen[j]) })

	if len(chosen) > maxCMTServerVersions {
		chosen = capCMTVersionsPreferringESR(chosen, keptESR, maxCMTServerVersions)
	}

	versions := make([]string, 0, len(chosen))
	for _, v := range chosen {
		versions = append(versions, v.raw)
	}
	s.Logger.WithField("versions", versions).Info("[resolveCMTServerVersions] Auto-derived CMT server version set")
	return versions
}

// capCMTVersionsPreferringESR keeps at most maxN from an ascending list, dropping oldest non-ESR first.
func capCMTVersionsPreferringESR(chosen []cmtVersion, esrMinors map[cmtMinorKey]bool, maxN int) []cmtVersion {
	if len(chosen) <= maxN {
		return chosen
	}

	isESR := func(v cmtVersion) bool {
		return v.rc == 0 && esrMinors[cmtMinorKey{v.major, v.minor}]
	}

	kept := append([]cmtVersion(nil), chosen...)
	for len(kept) > maxN {
		dropIdx := -1
		for i, v := range kept {
			if !isESR(v) {
				dropIdx = i
				break
			}
		}
		if dropIdx < 0 {
			dropIdx = 0
		}
		kept = append(kept[:dropIdx], kept[dropIdx+1:]...)
	}
	return kept
}
