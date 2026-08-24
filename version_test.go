package main

import (
	"bytes"
	"runtime/debug"
	"testing"
)

func TestVersionFromBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{
			name: "tagged module install",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}},
			ok:   true,
			want: "v1.2.3",
		},
		{
			name: "pseudo-version development build",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260820213018-aed1d7457018"}},
			ok:   true,
			want: "v0.0.0-20260820213018-aed1d7457018",
		},
		{
			name: "dirty development build",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3+dirty"}},
			ok:   true,
			want: "v1.2.3+dirty",
		},
		{
			name: "development build with revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef"},
				},
			},
			ok:   true,
			want: "devel (0123456789ab)",
		},
		{
			name: "development build without revision",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:   true,
			want: "devel",
		},
		{
			name: "missing build information",
			ok:   false,
			want: "devel",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := versionFromBuildInfo(test.info, test.ok); got != test.want {
				t.Fatalf("versionFromBuildInfo() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunPrintsBuildVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() exit = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "commitmsg "+buildVersion()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
