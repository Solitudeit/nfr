package version

import (
	"strconv"
	"strings"
)

// IsDev reports whether raw represents a local development build rather than a
// tagged release version.
func IsDev(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || s == "dev" || strings.HasSuffix(s, "-dirty") {
		return true
	}
	_, ok := parseSemver(s)
	return !ok
}

// IsPrerelease reports whether raw is a valid SemVer with a pre-release suffix
// such as v0.13.0-preview.3.
func IsPrerelease(raw string) bool {
	v, ok := parseSemver(raw)
	return ok && len(v.pre) > 0
}

// Compare compares two semantic version strings (with optional leading "v").
// Returns -1 if a < b, 0 if a == b, +1 if a > b. Unparseable values sort below
// valid versions.
func Compare(a, b string) int {
	va, oka := parseSemver(a)
	vb, okb := parseSemver(b)
	if !oka && !okb {
		return 0
	}
	if !oka {
		return -1
	}
	if !okb {
		return 1
	}
	if va.major != vb.major {
		return cmpInt(va.major, vb.major)
	}
	if va.minor != vb.minor {
		return cmpInt(va.minor, vb.minor)
	}
	if va.patch != vb.patch {
		return cmpInt(va.patch, vb.patch)
	}
	// Release without pre-release has higher precedence than pre-release.
	if len(va.pre) == 0 && len(vb.pre) == 0 {
		return 0
	}
	if len(va.pre) == 0 {
		return 1
	}
	if len(vb.pre) == 0 {
		return -1
	}
	n := len(va.pre)
	if len(vb.pre) < n {
		n = len(vb.pre)
	}
	for i := 0; i < n; i++ {
		pa, pb := va.pre[i], vb.pre[i]
		na, erra := strconv.Atoi(pa)
		nb, errb := strconv.Atoi(pb)
		switch {
		case erra == nil && errb == nil:
			if na != nb {
				return cmpInt(na, nb)
			}
		case erra == nil:
			return -1
		case errb == nil:
			return 1
		default:
			if pa != pb {
				if pa < pb {
					return -1
				}
				return 1
			}
		}
	}
	return cmpInt(len(va.pre), len(vb.pre))
}

// IsNewer reports whether candidate is strictly newer than current. When
// current is a dev build, IsNewer returns true for any valid tagged candidate
// only when allowDev is true.
func IsNewer(candidate, current string) bool {
	if _, ok := parseSemver(candidate); !ok {
		return false
	}
	if IsDev(current) {
		return false
	}
	return Compare(candidate, current) > 0
}

type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

func parseSemver(raw string) (semver, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return semver{}, false
	}
	// Strip build metadata after '+' or space (e.g. "v0.13.0 (commit ...)").
	if idx := strings.IndexAny(s, " +"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return semver{}, false
	}
	core := s
	var pre []string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		core = s[:idx]
		preRaw := s[idx+1:]
		if preRaw != "" {
			pre = strings.Split(preRaw, ".")
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil || maj < 0 {
		return semver{}, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil || min < 0 {
		return semver{}, false
	}
	pat := 0
	if len(parts) == 3 {
		pat, err = strconv.Atoi(parts[2])
		if err != nil || pat < 0 {
			return semver{}, false
		}
	}
	return semver{major: maj, minor: min, patch: pat, pre: pre}, true
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
