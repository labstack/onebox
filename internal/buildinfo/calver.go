package buildinfo

import (
	"fmt"
	"regexp"
	"strconv"
)

var releaseVersionPattern = regexp.MustCompile(`^v([1-9][0-9])\.([1-9]|1[0-2])\.([1-9][0-9]*)$`)

// ReleaseVersion is a canonical Onebox vYY.M.SEQUENCE release identity.
type ReleaseVersion struct {
	year     uint64
	month    uint64
	sequence uint64
}

// ParseReleaseVersion parses the exact version form used by Onebox tags,
// binary provenance, and minimum-runner policy.
func ParseReleaseVersion(value string) (ReleaseVersion, error) {
	matches := releaseVersionPattern.FindStringSubmatch(value)
	if matches == nil {
		return ReleaseVersion{}, fmt.Errorf("%q must match vYY.M.SEQUENCE with a year from 10 through 99, an unpadded month from 1 through 12, and a positive sequence", value)
	}
	year, err := strconv.ParseUint(matches[1], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release year: %w", err)
	}
	month, err := strconv.ParseUint(matches[2], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release month: %w", err)
	}
	sequence, err := strconv.ParseUint(matches[3], 10, 64)
	if err != nil {
		return ReleaseVersion{}, fmt.Errorf("parse release sequence: %w", err)
	}
	return ReleaseVersion{year: 2000 + year, month: month, sequence: sequence}, nil
}

// CompareReleaseVersions returns -1, 0, or 1 when actual is older than, equal
// to, or newer than minimum.
func CompareReleaseVersions(actual, minimum ReleaseVersion) int {
	for _, pair := range [][2]uint64{
		{actual.year, minimum.year},
		{actual.month, minimum.month},
		{actual.sequence, minimum.sequence},
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
