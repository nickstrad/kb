//go:build !harnessfake

package cli

import "testing"

// TestNoFakeModelInReleaseBuilds: without the harnessfake tag the fake model does not exist, so
// a release binary refuses fake:<dim> like any unknown model (DEFAULTS #189).
func TestNoFakeModelInReleaseBuilds(t *testing.T) {
	if harnessModel != nil {
		t.Fatal("harnessModel is set in a build without the harnessfake tag")
	}
	if knownModel("fake:16") == nil {
		t.Error("knownModel accepts fake:16 in a release build")
	}
}
