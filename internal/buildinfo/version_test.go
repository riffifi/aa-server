package buildinfo

import "testing"

func TestReleaseMetadataAndDirtySourceDescription(t *testing.T) {
	previousVersion, previousCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = previousVersion, previousCommit })
	Version, Commit = "v1.2.3-dirty", "1234567890abcdef1234567890abcdef12345678"
	info := Current("sessions")
	if info.Version != Version || info.Commit != Commit || !info.Dirty || info.Protocol != 2 || info.Service != "sessions" {
		t.Fatalf("incorrect embedded build metadata: %+v", info)
	}
}
