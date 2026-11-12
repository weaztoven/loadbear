package version

import "testing"

func TestStringFormatsBuildInformation(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = oldVersion, oldCommit, oldBuildDate
	})
	Version = "v1.2.3"
	Commit = "abc123"
	BuildDate = "2026-11-11T00:00:00Z"
	const want = "v1.2.3 (commit abc123, built 2026-11-11T00:00:00Z)"
	if got := String(); got != want {
		t.Fatalf("String() = %q; want %q", got, want)
	}
}
