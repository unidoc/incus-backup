package conformance_test

import (
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/s3test/conformance"
	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

// A conformance suite that cannot tell a broken server from a good one proves nothing. Each case
// switches ONE deliberate defect on in the fake and requires the targeted check(s) to FAIL while an
// unrelated check still PASSES (so the failure is the suite noticing the defect, not a broken harness).
// If the fake is changed so that a behaviour silently rots, or the suite is weakened, this fails.
func TestSuiteFailsAgainstBrokenServers(t *testing.T) {
	cases := []struct {
		quirk  s3fake.Quirk
		failed []string // checks that must fail
	}{
		{s3fake.QuirkIgnoreIfNoneMatch, []string{"put/if-none-match-refuses-existing"}},
		{s3fake.QuirkNonAtomicPut, []string{"put/if-none-match-is-atomic-under-concurrency"}},
		{s3fake.QuirkAcceptAnySignature, []string{"auth/wrong-secret-refused", "auth/unknown-access-key-refused", "auth/anonymous-refused", "auth/tampered-request-refused"}},
		{s3fake.QuirkNoContinuation, []string{"list/pagination-with-continuation-tokens", "list/over-1000-keys"}},
		{s3fake.QuirkUnsortedListing, []string{"list/prefix"}},
		{s3fake.QuirkIgnoreRange, []string{"object/range-reads"}},
		{s3fake.QuirkTornPutVisible, []string{"put/torn-upload-leaves-no-object"}},
		{s3fake.QuirkIgnoreDelimiter, []string{"list/delimiter-common-prefixes", "list/pagination-over-common-prefixes"}},
		{s3fake.QuirkDeleteMissing404, []string{"object/delete-is-idempotent"}},
		{s3fake.QuirkIgnorePrefix, []string{"list/prefix"}},
		{s3fake.QuirkWrongETag, []string{"object/put-get-roundtrip-sizes"}},
		{s3fake.QuirkMissingKeyIs200, []string{"object/get-missing-is-nosuchkey"}},
		{s3fake.QuirkNoVersioningMarker, []string{"config/versioning"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.quirk), func(t *testing.T) {
			_, tgt := fakeTarget(t, s3fake.Options{Quirks: []s3fake.Quirk{tc.quirk}})
			for _, name := range tc.failed {
				ch, ok := conformance.Lookup(name)
				if !ok {
					t.Fatalf("no check %q", name)
				}
				fails := conformance.Run(tgt, ch)
				if len(fails) == 0 {
					t.Errorf("check %q PASSED against a server with the %q defect: the suite does not notice it", name, tc.quirk)
				} else {
					t.Logf("ok: %q fails as it must: %s", name, strings.Split(fails[0], "\n")[0])
				}
			}
			// the harness itself is fine: an unrelated check passes on the same broken server
			ctl, _ := conformance.Lookup("object/overwrite")
			if fails := conformance.Run(tgt, ctl); len(fails) != 0 {
				t.Errorf("control check failed on the broken server: %v", fails)
			}
		})
	}
}

// Declaring a feature unsupported does not skip its check: the service must refuse the feature with
// an S3 error. The fake supports everything, so declaring a feature on it must FAIL.
func TestDeclaredButSupportedFeatureFails(t *testing.T) {
	_, tgt := fakeTarget(t, s3fake.Options{})
	for _, f := range conformance.Features {
		tgt.Unsupported = []string{f}
		var ran int
		for _, ch := range conformance.Checks() {
			if ch.Feature != f {
				continue
			}
			ran++
			if fails := conformance.Run(tgt, ch); len(fails) == 0 {
				t.Errorf("feature %q declared unsupported but its check %q passed on a server that supports it", f, ch.Name)
			}
		}
		if ran == 0 {
			t.Errorf("no check is tagged with feature %q", f)
		}
	}
}
