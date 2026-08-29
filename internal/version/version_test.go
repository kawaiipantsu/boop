package version

import "testing"

func TestGetReportsRuntimeMetadata(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Fatal("Version must never be empty")
	}
	if info.GoVersion == "" {
		t.Fatal("GoVersion must be populated from runtime")
	}
	if info.Platform == "" {
		t.Fatal("Platform must be populated from runtime")
	}
}

func TestStringIncludesVersionAndCommit(t *testing.T) {
	info := Info{Version: "1.2.3", Commit: "abc1234", GoVersion: "go1.19", Platform: "linux/amd64"}
	got := info.String()
	for _, want := range []string{"boop v1.2.3", "abc1234", "linux/amd64"} {
		if !contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
	if contains(got, "dirty") {
		t.Error("clean build must not be reported as dirty")
	}
}

func TestStringMarksDirtyBuilds(t *testing.T) {
	info := Info{Version: "1.2.3", Commit: "abc1234", Dirty: true}
	if !contains(info.String(), "(dirty)") {
		t.Error("dirty build must be marked in output")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
