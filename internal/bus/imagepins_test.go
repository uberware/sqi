// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// pinnedImage matches a nats-server image pinned by tag and digest.
var pinnedImage = regexp.MustCompile(`^nats:(\d+\.\d+\.\d+)@sha256:[0-9a-f]{64}$`)

// makefileVar returns the value of a `NAME := value` line in makefile.
func makefileVar(t *testing.T, makefile, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:=\s*(\S+)\s*$`)
	m := re.FindStringSubmatch(makefile)
	if m == nil {
		t.Fatalf("Makefile defines no %s := ...", name)
	}
	return m[1]
}

// TestNATSImagePins keeps the two nats-server images `make
// test-nats-cluster` runs on in step with what they stand for, and with CI:
//
//   - both are pinned by tag and digest;
//   - the current image is the nats-server sqi embeds (go.mod), so the
//     embedded and external modes are tested against the same broker code;
//   - the floor image is in the oldest series sqi accepts and at or above
//     its minimum patch, so the floor CI tests is the floor sqi enforces;
//   - the nats-cluster-integration job runs exactly these two images, and
//     no other;
//   - the reference Compose file defaults to the current image, digest
//     included.
func TestNATSImagePins(t *testing.T) {
	root := filepath.Join("..", "..")
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}

	tag := func(name string) (image, version string) {
		image = makefileVar(t, string(makefile), name)
		m := pinnedImage.FindStringSubmatch(image)
		if m == nil {
			t.Fatalf("%s = %q, want nats:<x.y.z>@sha256:<digest>", name, image)
		}
		return image, m[1]
	}
	floor, floorVersion := tag("NATS_IMAGE_FLOOR")
	current, currentVersion := tag("NATS_IMAGE_CURRENT")

	if currentVersion != natsserver.VERSION {
		t.Errorf("NATS_IMAGE_CURRENT is nats-server %s, but go.mod embeds %s; move the image with the module",
			currentVersion, natsserver.VERSION)
	}
	if err := CheckBrokerVersion(floorVersion); err != nil {
		t.Errorf("NATS_IMAGE_FLOOR is nats-server %s, which sqi refuses: %v", floorVersion, err)
	}
	lowest, _ := lowestAndHighestSeries()
	if want := "2." + strconv.Itoa(lowest) + "."; !strings.HasPrefix(floorVersion, want) {
		t.Errorf("NATS_IMAGE_FLOOR is nats-server %s, want the oldest supported series %sx (MinBrokerVersion %s)",
			floorVersion, want, MinBrokerVersion)
	}

	matrix := ciJobImages(t, string(ci), "nats-cluster-integration")
	slices.Sort(matrix)
	want := []string{floor, current}
	slices.Sort(want)
	if !slices.Equal(matrix, want) {
		t.Errorf(".github/workflows/ci.yml's nats-cluster-integration matrix runs %v, want exactly %v", matrix, want)
	}

	compose, err := os.ReadFile(filepath.Join(root, "deploy", "docker-compose.nats-cluster.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`image: \$\{NATS_IMAGE:-([^}]+)\}`).FindAllStringSubmatch(string(compose), -1)
	if len(m) == 0 {
		t.Fatal("deploy/docker-compose.nats-cluster.yml has no image: ${NATS_IMAGE:-<default>}")
	}
	for _, d := range m {
		if d[1] != current {
			t.Errorf("deploy/docker-compose.nats-cluster.yml defaults to %s, want NATS_IMAGE_CURRENT %s", d[1], current)
		}
	}
}

// TestNATSImagePins_HA keeps the nats-server image of the high-availability
// rig at the current image: the reference HA Compose file's nodes and the
// image the ha-integration job pulls for `make test-ha`, which runs on the
// Makefile's NATS_IMAGE (the current image by default).
func TestNATSImagePins_HA(t *testing.T) {
	root := filepath.Join("..", "..")
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	current := makefileVar(t, string(makefile), "NATS_IMAGE_CURRENT")

	compose, err := os.ReadFile(filepath.Join(root, "deploy", "docker-compose.ha.yml"))
	if err != nil {
		t.Fatal(err)
	}
	images := regexp.MustCompile(`(?m)^\s+image: (nats:\S+)\s*$`).FindAllStringSubmatch(string(compose), -1)
	if len(images) == 0 {
		t.Fatal("deploy/docker-compose.ha.yml names no nats image")
	}
	for _, m := range images {
		if m[1] != current {
			t.Errorf("deploy/docker-compose.ha.yml runs %s, want NATS_IMAGE_CURRENT %s", m[1], current)
		}
	}

	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	pulls := regexp.MustCompile(`(?m)^\s+docker pull (nats:\S+)\s*$`).FindAllStringSubmatch(ciJob(t, string(ci), "ha-integration"), -1)
	if len(pulls) != 1 || pulls[0][1] != current {
		got := make([]string, 0, len(pulls))
		for _, m := range pulls {
			got = append(got, m[1])
		}
		t.Errorf(".github/workflows/ci.yml's ha-integration job pulls %v, want exactly NATS_IMAGE_CURRENT %s", got, current)
	}
}

// ciJob returns the job named job of the workflow ci: the lines from its key
// to the next job's.
func ciJob(t *testing.T, ci, job string) string {
	t.Helper()
	start := strings.Index(ci, "\n  "+job+":\n")
	if start < 0 {
		t.Fatalf(".github/workflows/ci.yml has no job %s", job)
	}
	body := ci[start+1:]
	if next := regexp.MustCompile(`\n  [A-Za-z0-9_-]+:\n`).FindStringIndex(body[len(job)+3:]); next != nil {
		body = body[:len(job)+3+next[0]]
	}
	return body
}

// ciJobImages returns every `image:` value in the job named job of the
// workflow ci.
func ciJobImages(t *testing.T, ci, job string) []string {
	t.Helper()
	var images []string
	for _, m := range regexp.MustCompile(`(?m)^\s+image: (\S+)\s*$`).FindAllStringSubmatch(ciJob(t, ci, job), -1) {
		images = append(images, m[1])
	}
	return images
}
