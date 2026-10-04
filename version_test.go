package dalgo2postgres

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	reader := func(info *debug.BuildInfo, ok bool) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return info, ok }
	}
	tests := []struct {
		name string
		read func() (*debug.BuildInfo, bool)
		want string
	}{
		{"no build info", reader(nil, false), develVersion},
		{"nil build info", reader(nil, true), develVersion},
		{"main module tagged", reader(&debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.2.3"}}, true), "v0.2.3"},
		{"main module devel", reader(&debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, true), develVersion},
		{"main module empty", reader(&debug.BuildInfo{Main: debug.Module{Path: modulePath}}, true), develVersion},
		{"dependency", reader(&debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: "other/mod", Version: "v9.9.9"}, {Path: modulePath, Version: "v0.4.0"}},
		}, true), "v0.4.0"},
		{"dependency replaced", reader(&debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: modulePath, Version: "v0.4.0", Replace: &debug.Module{Version: "v0.5.0"}}},
		}, true), "v0.5.0"},
		{"dependency devel", reader(&debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: modulePath, Version: "(devel)"}},
		}, true), develVersion},
		{"dependency replaced by local path", reader(&debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app"},
			Deps: []*debug.Module{{Path: modulePath, Version: "v0.4.0", Replace: &debug.Module{Path: "../dalgo2postgres"}}},
		}, true), develVersion},
		{"dependency absent", reader(&debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}}, true), develVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.read); got != tt.want {
				t.Fatalf("resolveVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAdapterReportsVersion ties the adapter identity to Version, the only
// place the variable is used, so hard-coding a string in Adapter() fails here.
func TestAdapterReportsVersion(t *testing.T) {
	adapter := (&Database{}).Adapter()
	if adapter.Name() != "dalgo2postgres" || adapter.Version() != Version || adapter.Version() == "" {
		t.Fatalf("Adapter() = %s %s, want dalgo2postgres %q", adapter.Name(), adapter.Version(), Version)
	}
}
