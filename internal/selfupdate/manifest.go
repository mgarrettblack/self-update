package selfupdate

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
)

// CurrentVersion is overwritten at link time
const CurrentVersion = "0.0.0-dev"

// Semver is a parsed semantic version
type Semver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
	Build      string
}

func (v Semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// parseSemver parses a semver string, tolerating a leading "v".
func parseSemver(s string) (Semver, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return Semver{}, fmt.Errorf("version %q: empty", orig)
	}

	var v Semver
	if core, build, ok := strings.Cut(s, "+"); ok {
		if build == "" {
			return Semver{}, fmt.Errorf("version %q: empty build metadata", orig)
		}
		v.Build, s = build, core
	}
	if core, pre, ok := strings.Cut(s, "-"); ok {
		if pre == "" {
			return Semver{}, fmt.Errorf("version %q: empty prerelease", orig)
		}
		v.Prerelease, s = pre, core
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH, got %d component(s)", orig, len(parts))
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := parseNumericIdentifier(p)
		if err != nil {
			return Semver{}, fmt.Errorf("version %q: %w", orig, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

func parseNumericIdentifier(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty numeric component")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("numeric component %q has a leading zero", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("numeric component %q is not a number", s)
		}
	}
	return strconv.Atoi(s)
}

// compareSemver returns -1, 0 or 1 as a is less than, equal to, or greater than b.
func compareSemver(a, b Semver) int {
	for _, p := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if c := cmp.Compare(p[0], p[1]); c != 0 {
			return c
		}
	}
	return comparePrerelease(a.Prerelease, b.Prerelease)
}

// IsNewer reports whether candidate is strictly newer than current
func IsNewer(candidate, current string) (bool, error) {
	pc, err := parseSemver(candidate)
	if err != nil {
		return false, err
	}
	pr, err := parseSemver(current)
	if err != nil {
		return false, err
	}
	return compareSemver(pc, pr) > 0, nil
}

// comparePrerelease implements semver §11.4: a version with a prerelease has
// lower precedence than the same version without one, and identifiers are
// compared field by field.
func comparePrerelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}

	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	// Equal so far: the version with more identifiers has higher precedence.
	return cmp.Compare(len(as), len(bs))
}

func compareIdentifier(a, b string) int {
	an, aNum := allDigits(a)
	bn, bNum := allDigits(b)
	switch {
	case aNum && bNum:
		return cmp.Compare(an, bn)
	case aNum: // numeric identifiers always rank below alphanumeric ones
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func allDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Manifest is the signed release description served as manifest.json.
type Manifest struct {
	Version   string                      `json:"version"`
	Rollout   *int                        `json:"rollout,omitempty"`
	Platforms map[string]PlatformArtifact `json:"platforms"`
}

// PlatformArtifact is one downloadable build. The hash and size describe the
// compressed artifact — the exact bytes that cross the wire.
type PlatformArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// RolloutPercent returns the cohort percentage for this release.
func (m *Manifest) RolloutPercent() int {
	if m.Rollout == nil {
		return defaultRolloutPercentage
	}
	return *m.Rollout
}

// Artifact looks up the build for a platform key such as "darwin-arm64".
func (m *Manifest) Artifact(platform string) (PlatformArtifact, error) {
	art, ok := m.Platforms[platform]
	if !ok {
		return PlatformArtifact{}, fmt.Errorf("select artifact: release %s has no artifact for platform %q", m.Version, platform)
	}
	return art, nil
}

// PlatformKey is the manifest key for the running binary's platform.
func PlatformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// ParseManifest decodes, normalizes and validates a manifest document.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	for key, art := range m.Platforms {
		art.SHA256 = strings.ToLower(strings.TrimSpace(art.SHA256))
		m.Platforms[key] = art
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest is internally coherent.
func (m *Manifest) Validate() error {
	const op = "validate manifest"

	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%s: version is empty", op)
	}
	if _, err := parseSemver(m.Version); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if p := m.RolloutPercent(); p < 0 || p > 100 {
		return fmt.Errorf("%s: rollout %d is outside 0-100", op, p)
	}
	if len(m.Platforms) == 0 {
		return fmt.Errorf("%s: no platforms listed", op)
	}
	for key, art := range m.Platforms {
		if err := art.validate(); err != nil {
			return fmt.Errorf("%s: platform %q: %v", op, key, err)
		}
	}
	return nil
}

func (a PlatformArtifact) validate() error {
	if len(a.SHA256) != 64 {
		return fmt.Errorf("sha256 is %d chars, want 64", len(a.SHA256))
	}
	for _, r := range a.SHA256 {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("sha256 contains non-hex character %q", r)
		}
	}
	if a.Size <= 0 {
		return fmt.Errorf("size %d must be positive", a.Size)
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("url %q: %w", a.URL, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("url %q is not absolute", a.URL)
	}
	// http is accepted here only so a release host can be exercised locally;
	// requireHTTPS rejects it unless SELFUPDATE_ALLOW_HTTP is set.
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("url %q has unsupported scheme %q", a.URL, u.Scheme)
	}
	return nil
}
