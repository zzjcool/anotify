package server

import "testing"

func TestEmbeddedStaticBuildMode(t *testing.T) {
	fsys, err := embeddedStatic()
	if embedDist {
		if err != nil {
			t.Fatalf("embeddist build: %v", err)
		}
		if fsys == nil {
			t.Fatal("embeddist build returned nil filesystem")
		}
		return
	}
	if err == nil || fsys != nil {
		t.Fatal("default build must not embed dist; production uses -tags embeddist")
	}
}
