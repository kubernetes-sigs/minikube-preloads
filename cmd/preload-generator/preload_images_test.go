/*
Copyright 2025 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import "testing"

const containerd2Config = `version = 3
root = '/var/lib/containerd'

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    snapshotter = 'overlayfs'

  [plugins.'io.containerd.cri.v1.runtime'.containerd]
      [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes]
        [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
          snapshotter = ''
`

const containerd17Config = `disabled_plugins = []
imports = ["/etc/containerd/config.toml"]
oom_score = 0
plugin_dir = ""
required_plugins = []
root = "/var/lib/containerd"
state = "/run/containerd"
temp = ""
version = 2

[plugins]

  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".containerd]
      snapshotter = "overlayfs"

      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]

        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          snapshotter = ""
`

func TestParseContainerdSnapshotterCases(t *testing.T) {
	tcs := []struct {
		name string
		cfg  string
		want string
	}{
		{name: "v2 config with single quotes", cfg: containerd2Config, want: "overlayfs"},
		{name: "v1.7 config with double quotes", cfg: containerd17Config, want: "overlayfs"},
		{name: "single line double quotes", cfg: "snapshotter = \"overlayfs\"\n", want: "overlayfs"},
	}
	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := parseContainerdSnapshotter(tc.cfg); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestPrioritizeVersions(t *testing.T) {
	input := []string{
		"v1.30.12",
		"v1.36.0",
		"v1.31.14",
		"v1.35.4",
		"v1.29.15",
		"v1.35.0-rc.1",
		"v1.30.14",
		"v1.32.1",
		"v1.31.13",
		"v1.28.0",
		"v1.32.0",
	}

	t.Run("recentMinors=5, priority3Limit=0", func(t *testing.T) {
		want := []string{
			"v1.36.0",
			"v1.35.4",
			"v1.35.0-rc.1",
			"v1.32.1",
			"v1.32.0",
			"v1.31.14",
			"v1.30.14",
			"v1.29.15",
			"v1.28.0",
			"v1.31.13",
			"v1.30.12",
		}
		got := prioritizeVersions(input, 5, 0)
		if len(got) != len(want) {
			t.Fatalf("got length %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("at index %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("recentMinors=5, priority3Limit=1", func(t *testing.T) {
		want := []string{
			"v1.36.0",
			"v1.35.4",
			"v1.35.0-rc.1",
			"v1.32.1",
			"v1.32.0",
			"v1.31.14",
			"v1.30.14",
			"v1.29.15",
			"v1.28.0",
			"v1.31.13",
		}
		got := prioritizeVersions(input, 5, 1)
		if len(got) != len(want) {
			t.Fatalf("got length %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("at index %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("recentMinors=3, priority3Limit=2", func(t *testing.T) {
		want := []string{
			"v1.36.0",
			"v1.35.4",
			"v1.35.0-rc.1",
			"v1.32.1",
			"v1.31.14",
			"v1.30.14",
			"v1.29.15",
			"v1.28.0",
			"v1.32.0",
			"v1.31.13",
		}
		got := prioritizeVersions(input, 3, 2)
		if len(got) != len(want) {
			t.Fatalf("got length %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("at index %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("recentMinors=0, priority3Limit=2", func(t *testing.T) {
		want := []string{
			"v1.36.0",
			"v1.35.4",
			"v1.32.1",
			"v1.31.14",
			"v1.30.14",
			"v1.29.15",
			"v1.28.0",
			"v1.35.0-rc.1",
			"v1.32.0",
		}
		got := prioritizeVersions(input, 0, 2)
		if len(got) != len(want) {
			t.Fatalf("got length %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("at index %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("recentMinors=5, ignorePrereleaseForMaxMinor", func(t *testing.T) {
		inputWithPrerelease := []string{
			"v1.36.0",
			"v1.35.4",
			"v1.37.0-rc.0", // pre-release of newer minor version
			"v1.32.1",
			"v1.32.0",
			"v1.31.14",
			"v1.31.13",
		}
		// Max stable minor is 36. Cutoff minor is 36 - 4 = 32.
		// Priority 1 (>= 1.32.0):
		//   v1.37.0-rc.0
		//   v1.36.0
		//   v1.35.4
		//   v1.32.1
		//   v1.32.0
		// Priority 2 (highest patch of older minor versions < 32):
		//   v1.31.14 (for 1.31)
		// Priority 3 (other patches of older minor versions < 32):
		//   v1.31.13 (for 1.31)
		want := []string{
			"v1.37.0-rc.0",
			"v1.36.0",
			"v1.35.4",
			"v1.32.1",
			"v1.32.0",
			"v1.31.14",
			"v1.31.13",
		}
		got := prioritizeVersions(inputWithPrerelease, 5, 0)
		if len(got) != len(want) {
			t.Fatalf("got length %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("at index %d: got %s, want %s", i, got[i], want[i])
			}
		}
	})
}


