package buildinfo

import (
	"fmt"
	"regexp"
	"strconv"
)

var releaseVersionPattern = regexp.MustCompile(`^v([1-9][0-9]{3})\.([1-9]|1[0-2])\.(0|[1-9][0-9]*)$`)

// ReleaseVersion is a canonical Onebox vYYYY.M.REVISION release identity.
type ReleaseVersion struct {
	year     uint64
	month    uint64
	revision uint64
}

// ParseReleaseVersion parses the exact version form used by Onebox tags,
// binary provenance, and minimum-runner policy.
func ParseReleaseVersion(value string) (ReleaseVersion, error) {
	matches := releaseVersionPattern.FindStringSubmatch(value)
	if matches == nil {
		return ReleaseVersion{}, fmt.Errorf("%q must match vYYYY.M.REVISION with a four-digit year, an unpadded month from 1 through 12, and an unpadded non-negative revision", value)
	}
	year, err := strconv.ParseUint(matches[1], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release year: %w", err)
	}
	month, err := strconv.ParseUint(matches[2], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release month: %w", err)
	}
	revision, err := strconv.ParseUint(matches[3], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release revision: %w", err)
	}
	return ReleaseVersion{year: year, month: month, revision: revision}, nil
}

// CompareReleaseVersions returns -1, 0, or 1 when actual is older than, equal
// to, or newer than minimum.
func CompareReleaseVersions(actual, minimum ReleaseVersion) int {
	for _, pair := range [][2]uint64{
		{actual.year, minimum.year},
		{actual.month, minimum.month},
		{actual.revision, minimum.revision},
	} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}
