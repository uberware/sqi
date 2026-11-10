// SPDX-License-Identifier: AGPL-3.0-or-later

package preempt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// InstanceID reads the identifier of the cloud instance the worker runs on from
// the instance metadata service of vendor (aws, gcp or azure), with the same base
// URLs, endpoint override and client rules as [New]. The identifier is the one
// a scale-set or instance-group operation names:
//
//   - aws: the EC2 instance ID ("i-0abc..."), read after an IMDSv2 token.
//   - gcp: the instance name, which "gcloud ... delete-instances" takes.
//   - azure: the scale-set instance ID, the last segment of the VM's resource ID.
//
// Each request is bounded by the same 2 s timeout as a preemption poll. Any
// failure is an error; the caller decides what a missing ID costs.
func InstanceID(ctx context.Context, vendor, endpoint string, client *http.Client) (string, error) {
	if client == nil {
		client = defaultClient()
	}
	var base string
	var read func(context.Context, *http.Client, string) (string, error)
	switch vendor {
	case VendorAWS:
		base, read = awsAzureBase, awsInstanceID
	case VendorGCP:
		base, read = gcpBase, gcpInstanceID
	case VendorAzure:
		base, read = awsAzureBase, azureInstanceID
	default:
		return "", fmt.Errorf("preempt: unknown instance ID vendor %q (want aws, gcp or azure)", vendor)
	}
	if endpoint != "" {
		base = strings.TrimRight(endpoint, "/")
	}
	return read(ctx, client, base)
}

func awsInstanceID(ctx context.Context, client *http.Client, base string) (string, error) {
	token, err := awsToken(ctx, client, base)
	if err != nil {
		return "", err
	}
	return readID(ctx, client, base, "aws instance-id", "/latest/meta-data/instance-id",
		map[string]string{"X-aws-ec2-metadata-token": token})
}

func gcpInstanceID(ctx context.Context, client *http.Client, base string) (string, error) {
	return readID(ctx, client, base, "gcp instance name", "/computeMetadata/v1/instance/name",
		map[string]string{"Metadata-Flavor": "Google"})
}

func azureInstanceID(ctx context.Context, client *http.Client, base string) (string, error) {
	resourceID, err := readID(ctx, client, base, "azure resourceId",
		"/metadata/instance/compute/resourceId?api-version=2021-02-01&format=text",
		map[string]string{"Metadata": "true"})
	if err != nil {
		return "", err
	}
	id := resourceID[strings.LastIndex(resourceID, "/")+1:]
	if id == "" {
		return "", errors.New("azure resourceId: empty instance ID after the last slash")
	}
	return id, nil
}

// readID GETs path and returns the trimmed body, which must not be empty. what
// names the value in errors.
func readID(
	ctx context.Context, client *http.Client, base, what, path string, headers map[string]string,
) (string, error) {
	status, body, err := request(ctx, client, base, http.MethodGet, path, headers)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%s: unexpected status %d", what, status)
	}
	id := strings.TrimSpace(string(body))
	if id == "" {
		return "", fmt.Errorf("%s: empty answer", what)
	}
	return id, nil
}
