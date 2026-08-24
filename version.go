package main

import "runtime/debug"

const developmentVersion = "devel"

// buildVersion returns the version embedded by the Go toolchain. `go install`
// records the requested module version here, so release tags do not need a
// matching source-code change. Local builds are identified as development
// builds and include the available VCS revision for traceability.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	return versionFromBuildInfo(info, ok)
}

func versionFromBuildInfo(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return developmentBuildVersion(info)
	}
	return info.Main.Version
}

func developmentBuildVersion(info *debug.BuildInfo) string {
	if info == nil {
		return developmentVersion
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return developmentVersion + " (" + shortRevision(setting.Value) + ")"
		}
	}
	return developmentVersion
}

func shortRevision(revision string) string {
	const revisionLength = 12
	if len(revision) <= revisionLength {
		return revision
	}
	return revision[:revisionLength]
}
