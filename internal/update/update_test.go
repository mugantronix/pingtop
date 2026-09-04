package update

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestIsNewer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{"equal versions", "1.0.0", "1.0.0", false},
		{"patch bump", "1.0.0", "1.0.1", true},
		{"minor bump", "1.0.0", "1.1.0", true},
		{"major bump", "1.0.0", "2.0.0", true},
		{"current newer than latest", "1.1.0", "1.0.0", false},
		{"leading v stripped", "v1.0.0", "v1.0.1", true},
		{"numeric compare not lexicographic", "1.9.0", "1.10.0", true},
		{"reverse numeric compare", "1.10.0", "1.9.0", false},
		{"dev build always outdated", "dev", "1.0.0", true},
		{"empty current always outdated", "", "1.0.0", true},
		{"both empty: no update", "", "", false},
		{"malformed latest treated as no update", "1.0.0", "not-a-version", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNewer(tc.current, tc.latest); got != tc.want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello world")
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	goodSums := []byte(got + "  pingtop_1.0.0_windows_amd64.zip\nabc123  other-file.zip\n")

	if err := verifyChecksum(data, "pingtop_1.0.0_windows_amd64.zip", goodSums); err != nil {
		t.Errorf("expected checksum match, got error: %v", err)
	}

	badSums := []byte("deadbeef  pingtop_1.0.0_windows_amd64.zip\n")
	if err := verifyChecksum(data, "pingtop_1.0.0_windows_amd64.zip", badSums); err == nil {
		t.Error("expected checksum mismatch error, got nil")
	}

	missingSums := []byte("abc123  some-other-file.zip\n")
	if err := verifyChecksum(data, "pingtop_1.0.0_windows_amd64.zip", missingSums); err == nil {
		t.Error("expected 'no checksum entry' error, got nil")
	}
}
