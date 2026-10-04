package dalgo2postgres

import "runtime/debug"

// modulePath is this module's import path, as it appears in a consumer's
// build information.
const modulePath = "github.com/dal-go/dalgo2postgres"

// develVersion is reported when no module version is available, for example
// when the package is built from a working tree or exercised by its own tests.
const develVersion = "devel"

// Version is the module version recorded in the consuming binary's build
// information (a tag or a pseudo-version), or "devel" when none is recorded,
// for example in a local-path replace or a working-tree build. Treat it as
// read-only.
var Version = resolveVersion(debug.ReadBuildInfo)

// resolveVersion returns the version of this module found in the build
// information supplied by read, or develVersion when none is recorded.
func resolveVersion(read func() (*debug.BuildInfo, bool)) string {
	info, ok := read()
	if !ok || info == nil {
		return develVersion
	}
	if info.Main.Path == modulePath {
		return versionOrDevel(info.Main.Version)
	}
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			return versionOrDevel(dep.Replace.Version)
		}
		return versionOrDevel(dep.Version)
	}
	return develVersion
}

func versionOrDevel(v string) string {
	if v == "" || v == "(devel)" {
		return develVersion
	}
	return v
}
