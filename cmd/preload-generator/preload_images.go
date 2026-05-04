/*
Copyright 2020 The Kubernetes Authors All rights reserved.

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

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/blang/semver/v4"
	"github.com/spf13/viper"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/download"
	"k8s.io/minikube/pkg/util"
)

const (
	profile      = "generate-preloaded-images-tar"
	minikubePath = "out/minikube"
)

var (
	dockerStorageDriver   = "overlay2"
	containerdSnapshotter = "overlayfs"
	podmanStorageDriver   = "overlay"
	containerRuntimes     = []string{"docker", "containerd", "cri-o"}
	k8sVersions           []string
	k8sVersion            = flag.String("k8s-version", "", "desired Kubernetes version, for example `v1.17.2`")
	noUpload              = flag.Bool("no-upload", false, "Do not upload tarballs to GCS")
	force                 = flag.Bool("force", false, "Generate the preload tarball even if it's already exists")
	limit                 = flag.Int("limit", 0, "Limit the number of tarballs to generate")
	armUpload             = flag.Bool("arm-upload", false, "Upload the arm64 preload tarballs to GCS")
	armPreloadsDir        = flag.String("arm-preloads-dir", "artifacts", "Directory containing the arm64 preload tarballs")
	preloadSrc            = flag.String("preload-src", "gcs", "Source to check for existing preloads (gcs|gh)")
	recentMinors          = flag.Int("recent-minors", 5, "Number of recent minor Kubernetes versions to prioritize and generate all patches for")
	priority3Limit        = flag.Int("priority-3-limit", 3, "Maximum number of Priority 3 versions (older patches of older minor versions) to keep. If 0, all of them will be kept.")
)

type preloadCfg struct {
	k8sVer  string
	runtime string
}

func (p preloadCfg) String() string {
	return fmt.Sprintf("%q/%q", p.runtime, p.k8sVer)
}

func preloadChecker(src string) (func(string, string) bool, error) {
	switch src {
	case "gcs":
		return download.PreloadExistsGCS, nil
	case "gh":
		return download.PreloadExistsGH, nil
	default:
		return nil, fmt.Errorf("invalid preload source %q, valid options are gcs or gh", src)
	}
}

func main() {
	flag.Parse()

	preloadExists, err := preloadChecker(*preloadSrc)
	if err != nil {
		log.Fatal(err)
	}

	if *armUpload {
		if err := uploadArmTarballs(*armPreloadsDir); err != nil {
			log.Fatal(err)
		}
		return
	}

	// used by pkg/minikube/download.PreloadExists()
	viper.Set("preload", "true")

	if *k8sVersion != "" {
		k8sVersions = []string{*k8sVersion}
	}

	if err := deleteMinikube(); err != nil {
		fmt.Printf("error cleaning up minikube at start up: %v \n", err)
	}

	k8sVersions, err := collectK8sVers()
	if err != nil {
		exit("Unable to get recent k8s versions: %v\n", err)
	}

	var toGenerate []preloadCfg
	var i int

out:
	for _, kv := range k8sVersions {
		for _, cr := range containerRuntimes {
			if *limit > 0 && i >= *limit {
				break out
			}
			// Since none/mock are the only exceptions, it does not matter what driver we choose.
			if !preloadExists(kv, cr) {
				toGenerate = append(toGenerate, preloadCfg{kv, cr})
				i++
				fmt.Printf("[%d] A preloaded tarball for k8s version %s - runtime %q does not exist.\n", i, kv, cr)
			} else if *force {
				// the tarball already exists, but '--force' is passed. we need to overwrite the file
				toGenerate = append(toGenerate, preloadCfg{kv, cr})
				i++
				fmt.Printf("[%d] A preloaded tarball for k8s version %s - runtime %q already exists. Going to overwrite it.\n", i, kv, cr)
			} else {
				fmt.Printf("A preloaded tarball for k8s version %s - runtime %q already exists, skipping generation.\n", kv, cr)
			}
		}
	}

	fmt.Printf("Going to generate preloads for %v\n", toGenerate)

	for _, cfg := range toGenerate {
		if err := makePreload(cfg); err != nil {
			exit(err.Error(), err)
		}
	}
}

func collectK8sVers() ([]string, error) {
	if k8sVersions == nil {
		recent, err := recentK8sVersions()
		if err != nil {
			return nil, err
		}
		k8sVersions = recent
	}
	versions := append([]string{
		constants.DefaultKubernetesVersion,
		constants.NewestKubernetesVersion,
		constants.OldestKubernetesVersion,
	}, k8sVersions...)
	unique := util.RemoveDuplicateStrings(versions)
	return prioritizeVersions(unique, *recentMinors, *priority3Limit), nil
}

func makePreload(cfg preloadCfg) error {
	kv, cr := cfg.k8sVer, cfg.runtime

	fmt.Printf("A preloaded tarball for k8s version %s - runtime %q doesn't exist, generating now...\n", kv, cr)
	tf := download.TarballName(kv, cr)

	defer func() {
		if err := deleteMinikube(); err != nil {
			fmt.Printf("error cleaning up minikube before finishing up: %v\n", err)
		}
	}()

	if err := generateTarball(kv, cr, tf); err != nil {
		return errors.Wrap(err, fmt.Sprintf("generating tarball for k8s version %s with %s", kv, cr))
	}

	if *noUpload {
		fmt.Printf("skip upload of %q\n", tf)
		return nil
	}
	if err := uploadTarball(tf, kv); err != nil {
		return errors.Wrap(err, fmt.Sprintf("uploading tarball for k8s version %s with %s", kv, cr))
	}
	return nil
}

var verifyDockerStorage = func() error {
	cmd := exec.Command("docker", "exec", profile, "docker", "info", "-f", "{{.Info.Driver}}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %v:\n%s", cmd.Args, err, stderr.String())
	}
	driver := strings.Trim(string(output), " \n")
	if driver != dockerStorageDriver {
		return fmt.Errorf("docker storage driver %s does not match requested %s", driver, dockerStorageDriver)
	}
	return nil
}

var verifyContainerdStorage = func() error {
	cmd := exec.Command("docker", "exec", profile, "sudo", "containerd", "config", "dump")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %v:\n%s", cmd.Args, err, stderr.String())
	}
	driver := parseContainerdSnapshotter(string(output))
	if driver != containerdSnapshotter {
		return fmt.Errorf("containerd snapshotter %s does not match requested %s", driver, containerdSnapshotter)
	}
	return nil
}

func parseContainerdSnapshotter(cfg string) string {
	var driver string
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "snapshotter") { // e.g. snapshotter = "overlayfs" or snapshotter = 'overlayfs'
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		val := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		if val == "" {
			continue
		}
		if driver == "" {
			driver = val
		}
	}
	return driver
}

var verifyPodmanStorage = func() error {
	cmd := exec.Command("docker", "exec", profile, "sudo", "podman", "info", "-f", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %v:\n%s", cmd.Args, err, stderr.String())
	}
	var info map[string]map[string]interface{}
	err = json.Unmarshal(output, &info)
	if err != nil {
		return err
	}
	driver := info["store"]["graphDriverName"]
	if driver != podmanStorageDriver {
		return fmt.Errorf("podman storage driver %s does not match requested %s", driver, podmanStorageDriver)
	}
	return nil
}

// exit will exit and clean up minikube
func exit(msg string, err error) {
	fmt.Printf("WithError(%s)=%v called from:\n%s", msg, err, debug.Stack())
	if err := deleteMinikube(); err != nil {
		fmt.Printf("error cleaning up minikube at start up: %v\n", err)
	}
	os.Exit(60)
}

type parsedVersion struct {
	raw string
	sem semver.Version
}

// prioritizeVersions sorts K8s version strings based on three priorities:
//
//   - Priority 1: The past 5 minor versions (including the newest found minor version)
//     and all their patches (e.g., if newest is v1.36, then v1.36.x to v1.32.x), ordered descending.
//
//   - Priority 2: The latest (highest) patch of any older minor version (e.g., v1.31.14, v1.30.14),
//     ordered descending by minor version.
//
// - Priority 3: All other patches of older minor versions, ordered descending.
func prioritizeVersions(versions []string, recentMinors int, priority3Limit int) []string {
	var parsed []parsedVersion
	for _, v := range versions {
		clean := strings.TrimPrefix(v, "v")
		sv, err := semver.Parse(clean)
		if err != nil {
			log.Printf("Warning: failed to parse version %q: %v", v, err)
			continue
		}
		parsed = append(parsed, parsedVersion{raw: v, sem: sv})
	}

	if len(parsed) == 0 {
		return versions
	}

	// Find max minor version
	var maxMinor uint64
	for _, pv := range parsed {
		if pv.sem.Major == 1 {
			if pv.sem.Minor > maxMinor {
				maxMinor = pv.sem.Minor
			}
		}
	}

	var cutoffMinor uint64
	if recentMinors > 0 && maxMinor >= uint64(recentMinors-1) {
		cutoffMinor = maxMinor - uint64(recentMinors-1)
	} else {
		cutoffMinor = 0
	}

	var priority1 []parsedVersion
	// Map from minor version to all its parsed versions (for minor < cutoffMinor)
	olderGroups := make(map[uint64][]parsedVersion)

	for _, pv := range parsed {
		if pv.sem.Major != 1 {
			olderGroups[pv.sem.Minor] = append(olderGroups[pv.sem.Minor], pv)
			continue
		}

		if pv.sem.Minor >= cutoffMinor {
			priority1 = append(priority1, pv)
		} else {
			olderGroups[pv.sem.Minor] = append(olderGroups[pv.sem.Minor], pv)
		}
	}

	// Sort Priority 1 descending
	sort.Slice(priority1, func(i, j int) bool {
		return priority1[i].sem.GT(priority1[j].sem)
	})

	// Process older groups to extract Priority 2 and Priority 3
	var priority2 []parsedVersion
	var priority3 []parsedVersion

	// Get older minor versions in descending order
	var olderMinors []uint64
	for m := range olderGroups {
		olderMinors = append(olderMinors, m)
	}
	sort.Slice(olderMinors, func(i, j int) bool {
		return olderMinors[i] > olderMinors[j]
	})

	for _, m := range olderMinors {
		group := olderGroups[m]
		// Sort group descending
		sort.Slice(group, func(i, j int) bool {
			return group[i].sem.GT(group[j].sem)
		})

		// First is the highest patch of this minor version
		priority2 = append(priority2, group[0])
		// The rest go to Priority 3
		if len(group) > 1 {
			priority3 = append(priority3, group[1:]...)
		}
	}

	// Sort Priority 3 descending
	sort.Slice(priority3, func(i, j int) bool {
		return priority3[i].sem.GT(priority3[j].sem)
	})

	if priority3Limit > 0 && len(priority3) > priority3Limit {
		priority3 = priority3[:priority3Limit]
	}

	// Concatenate all priorities
	var result []string
	for _, pv := range priority1 {
		result = append(result, pv.raw)
	}
	for _, pv := range priority2 {
		result = append(result, pv.raw)
	}
	for _, pv := range priority3 {
		result = append(result, pv.raw)
	}

	return result
}
