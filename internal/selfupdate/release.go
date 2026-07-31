package selfupdate

import (
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
)

// Version is overwritten at link time
var Version = "0.0.0-dev"

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
		return PlatformArtifact{}, classifyf(ClassManifestInvalid, "select artifact",
			"release %s has no artifact for platform %q", m.Version, platform)
	}
	return art, nil
}

// PlatformKey is the manifest key for the running binary's platform.
func PlatformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// ParseManifest decodes, normalizes and validates a manifest document.
//
// Callers must only ever run this on bytes whose signature already verified —
// parsing an unverified manifest means acting on attacker-controlled URLs.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, classify(ClassManifestInvalid, "parse manifest", err)
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
//
// The release service is expected to have validated the release before signing
// it, but the client validates again anyway: a signature proves the manifest
// came from the release pipeline, not that the pipeline got it right.
func (m *Manifest) Validate() error {
	const op = "validate manifest"

	if strings.TrimSpace(m.Version) == "" {
		return classify(ClassManifestInvalid, op, errors.New("version is empty"))
	}
	if _, err := parseSemver(m.Version); err != nil {
		return classify(ClassManifestInvalid, op, err)
	}
	if p := m.RolloutPercent(); p < 0 || p > 100 {
		return classifyf(ClassManifestInvalid, op, "rollout %d is outside 0-100", p)
	}
	if len(m.Platforms) == 0 {
		return classify(ClassManifestInvalid, op, errors.New("no platforms listed"))
	}
	for key, art := range m.Platforms {
		if err := art.validate(); err != nil {
			return classifyf(ClassManifestInvalid, op, "platform %q: %v", key, err)
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
	// the Checker rejects it unless insecure URLs are explicitly opted into.
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("url %q has unsupported scheme %q", a.URL, u.Scheme)
	}
	return nil
}

// Verifier checks detached Ed25519 signatures against a set of trusted public
// keys.
//
// It is a *set*, not a single key, deliberately: rotating away from a
// compromised key requires that already-deployed clients accept a signature
// from the replacement. A client that trusts exactly one key can never be
// migrated off it. See bakedInTrustedKeys below for the rotation procedure.
type Verifier struct {
	keys []ed25519.PublicKey
}

// NewVerifier builds a verifier over the given trust set. An empty set is
// rejected: a verifier that can never accept anything would silently disable
// updates forever rather than fail visibly.
func NewVerifier(keys ...ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, classify(ClassInternal, "new verifier", errors.New("trust set is empty"))
	}
	trusted := make([]ed25519.PublicKey, 0, len(keys))
	for i, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return nil, classifyf(ClassInternal, "new verifier",
				"key %d is %d bytes, want %d", i, len(k), ed25519.PublicKeySize)
		}
		trusted = append(trusted, k)
	}
	return &Verifier{keys: trusted}, nil
}

// Verify reports whether sig is a valid signature over message by any trusted
// key. Every failure is a ClassSignatureInvalid error — there is no partial
// success and no fallback path that accepts unverified bytes.
func (v *Verifier) Verify(message, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return classifyf(ClassSignatureInvalid, "verify signature",
			"signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	for _, k := range v.keys {
		if ed25519.Verify(k, message, sig) {
			return nil
		}
	}
	return classifyf(ClassSignatureInvalid, "verify signature",
		"no trusted key matched (%d in trust set)", len(v.keys))
}

// ParsePublicKey decodes a standard-base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, classify(ClassInternal, "parse public key", errors.New("empty"))
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, classify(ClassInternal, "parse public key", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, classifyf(ClassInternal, "parse public key",
			"decoded to %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePublicKeys decodes a comma-separated list of base64 public keys,
// skipping blank entries so a trailing comma in a build flag is harmless.
func ParsePublicKeys(list string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, field := range strings.Split(list, ",") {
		if strings.TrimSpace(field) == "" {
			continue
		}
		k, err := ParsePublicKey(field)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// DecodeSignature decodes the contents of a detached `.sig` file: one
// base64-encoded Ed25519 signature, with surrounding whitespace tolerated
// because most tooling appends a trailing newline.
func DecodeSignature(fileContents []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(fileContents)))
	if err != nil {
		return nil, classify(ClassSignatureInvalid, "decode signature", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, classifyf(ClassSignatureInvalid, "decode signature",
			"decoded to %d bytes, want %d", len(raw), ed25519.SignatureSize)
	}
	return raw, nil
}

// TrustedKeysBase64 is the release trust set, injected at link time:
//
//	go build -ldflags "-X self-update/internal/selfupdate.TrustedKeysBase64=$PUBKEY"
//
// Multiple keys are comma separated. This is deliberately a build-time input
// and never a runtime one: a public key read from a config file, an environment
// variable or the network is a public key an attacker can replace, which
// reduces signature verification to theatre.
var TrustedKeysBase64 = ""

// bakedInTrustedKeys is the trust set committed to source. Keeping keys here as
// well as in TrustedKeysBase64 is what makes rotation survivable — see the
// procedure below.
//
// Rotating a signing key. The release service owns the keys; the client's part
// is to trust the incoming one before the service starts using it:
//
//  1. Get the new public key from whoever runs the release service, add it to
//     this slice keeping the outgoing key in place, and ship that build.
//  2. Wait until effectively every client is running a build that trusts both
//     keys. Until that point, releases must still be signed by the old key.
//  3. Only then does the release service switch to signing with the new key.
//  4. Once no supported client trusts the old key alone, remove it here.
//
// Skipping step 2 strands every client that has not yet updated: it will reject
// all future releases as unsigned and can never update itself out of that state
// without manual reinstallation.
var bakedInTrustedKeys = []string{
	// No keys are committed to this repository. Supply the trust set with
	// -ldflags, or add the project's release public keys here.
}

// TrustedVerifier returns a Verifier over the compile-time trust set.
//
// It fails when the trust set is empty rather than returning a permissive
// verifier. A build that shipped with no keys would otherwise have to choose at
// runtime between "reject everything" and "accept anything", and the second is
// a remote code execution vector; refusing at construction makes the mistake
// visible on the first check instead.
func TrustedVerifier() (*Verifier, error) {
	const op = "load trusted keys"

	// TrustedKeysBase64 is a comma-separated list; split it into fields only
	// when it holds something, so an unset build flag contributes no entries
	// rather than one spurious empty string.
	var configured []string
	if strings.TrimSpace(TrustedKeysBase64) != "" {
		configured = strings.Split(TrustedKeysBase64, ",")
	}

	var encoded []string
	seen := make(map[string]bool)
	for _, k := range append(append([]string{}, bakedInTrustedKeys...), configured...) {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		encoded = append(encoded, k)
	}
	if len(encoded) == 0 {
		return nil, classify(ClassInternal, op, errors.New(
			"no trusted public keys are compiled in; build with "+
				`-ldflags "-X self-update/internal/selfupdate.TrustedKeysBase64=<base64 key>"`))
	}

	keys, err := ParsePublicKeys(strings.Join(encoded, ","))
	if err != nil {
		return nil, err
	}
	return NewVerifier(keys...)
}

// InRolloutCohort reports whether this install is in the cohort for a release
// at the given rollout percentage.
//
// Crash-loop detection (see rollback.go) catches a bad update on one machine
// after the fact; it does nothing to stop a bad release reaching the whole
// fleet at once. Staged rollout is the other half: publish at 10%, watch the
// telemetry, then ramp.
//
// Two properties make this usable, and both are tested:
//
//   - Deterministic in (installID, version). A client that re-rolled on every
//     poll would drift into any cohort eventually, so a 10% rollout would reach
//     everyone given enough hours.
//   - Monotonic in percent. Raising the percentage only ever adds clients, so
//     ramping a release never makes it disappear from a client that already saw
//     it.
//
// Keying on the version as well as the install ID matters: keyed on the ID
// alone, the same unlucky 10% of the fleet would be the canary for every
// release forever.
func InRolloutCohort(installID, releaseVersion string, percent int) bool {
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	sum := sha256.Sum256([]byte(installID + "|" + releaseVersion))
	bucket := binary.BigEndian.Uint64(sum[:8]) % 100
	return bucket < uint64(percent)
}
