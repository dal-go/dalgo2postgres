package dalgo2postgres

import "runtime/debug"

// modulePath is this module's import path, as it appears in a consumer's
// build information.
const modulePath = "github.com/dal-go/dalgo2postgres"

// develVersion is reported when no module version is available, for example
// when the package is built from a working tree or exercised by its own tests.
const develVersion = "devel"

// Version is the dalgo2postgres module version, read from build information.
// It is the tag CI created for the release the consumer depends on.
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
