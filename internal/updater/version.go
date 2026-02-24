package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// Version represents a parsed semantic version.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // e.g. "rc1", "beta2"
	Raw        string
}

// Parse parses a version string like "0.1.6", "v0.1.6", "0.2.0-rc1".
func Parse(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	v := Version{Raw: s}

	// Split off prerelease suffix
	if idx := strings.Index(s, "-"); idx >= 0 {
		v.Prerelease = s[idx+1:]
		s = s[:idx]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return v, fmt.Errorf("invalid version: %s", v.Raw)
	}

	var err error
	if v.Major, err = strconv.Atoi(parts[0]); err != nil {
		return v, fmt.Errorf("invalid major version: %w", err)
	}
	if v.Minor, err = strconv.Atoi(parts[1]); err != nil {
		return v, fmt.Errorf("invalid minor version: %w", err)
	}
	if len(parts) == 3 {
		if v.Patch, err = strconv.Atoi(parts[2]); err != nil {
			return v, fmt.Errorf("invalid patch version: %w", err)
		}
	}
	return v, nil
}

// IsDevBuild returns true if the version string is not a valid semver
// (e.g. a git short SHA like "a1b2c3d" or the literal "dev").
func IsDevBuild(version string) bool {
	_, err := Parse(version)
	return err != nil || version == "" || version == "dev"
}

// IsNewer returns true if other is a newer version than current.
func IsNewer(current, other Version) bool {
	if other.Major != current.Major {
		return other.Major > current.Major
	}
	if other.Minor != current.Minor {
		return other.Minor > current.Minor
	}
	if other.Patch != current.Patch {
		return other.Patch > current.Patch
	}
	// Stable release (no prerelease) is newer than a prerelease of the same version
	if current.Prerelease != "" && other.Prerelease == "" {
		return true
	}
	return false
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}
