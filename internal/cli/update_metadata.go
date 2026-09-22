package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func fetchLatestRelease(ctx context.Context, environment updateEnvironment) (updateRelease, error) {
	if err := environment.urlPolicy.validate(environment.metadataURL); err != nil {
		return updateRelease{}, contextualUpdateFailure("update_check_failed", "invalid release metadata URL", err)
	}
	payload, err := downloadUpdateResource(
		ctx, environment.client, environment.metadataURL, maximumReleaseMetadata,
	)
	if err != nil {
		return updateRelease{}, contextualUpdateFailure("update_check_failed", "check latest GitHub release", err)
	}
	var release updateRelease
	if err := json.Unmarshal(payload, &release); err != nil {
		return updateRelease{}, updateFailure("update_check_failed", "decode latest GitHub release: %v", err)
	}
	if release.TagName == "" || release.HTMLURL == "" {
		return updateRelease{}, updateFailure("update_check_failed", "latest GitHub release metadata is incomplete")
	}
	if err := environment.urlPolicy.validate(release.HTMLURL); err != nil {
		return updateRelease{}, contextualUpdateFailure("update_check_failed", "invalid release page URL", err)
	}
	return release, nil
}

func compareReleaseVersions(current string, releaseTag string) (string, int, error) {
	currentParts, _, err := parseReleaseVersion(current)
	if err != nil {
		return "", 0, updateFailure("update_check_failed", "installed version is invalid: %v", err)
	}
	releaseParts, normalizedRelease, err := parseReleaseVersion(releaseTag)
	if err != nil {
		return "", 0, updateFailure("update_check_failed", "latest release version is invalid: %v", err)
	}
	for index := range currentParts {
		if currentParts[index] > releaseParts[index] {
			return normalizedRelease, 1, nil
		}
		if currentParts[index] < releaseParts[index] {
			return normalizedRelease, -1, nil
		}
	}
	return normalizedRelease, 0, nil
}

func parseReleaseVersion(value string) ([3]int, string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, "", fmt.Errorf("expected MAJOR.MINOR.PATCH, got %q", value)
	}
	var parsed [3]int
	for index, part := range parts {
		if part == "" || (len(part) > 1 && strings.HasPrefix(part, "0")) {
			return [3]int{}, "", fmt.Errorf("invalid numeric component %q", part)
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, "", fmt.Errorf("invalid numeric component %q", part)
		}
		parsed[index] = number
	}
	return parsed, value, nil
}

func releaseAssetURLs(assets []updateAsset, archiveName string) (string, string, string, error) {
	var archiveURL string
	var checksumURL string
	var signatureURL string
	for _, asset := range assets {
		switch asset.Name {
		case archiveName:
			if archiveURL != "" {
				return "", "", "", updateFailure("update_package_invalid", "release contains duplicate %s assets", archiveName)
			}
			archiveURL = asset.DownloadURL
		case "SHA256SUMS":
			if checksumURL != "" {
				return "", "", "", updateFailure("update_package_invalid", "release contains duplicate SHA256SUMS assets")
			}
			checksumURL = asset.DownloadURL
		case "SHA256SUMS.sig":
			if signatureURL != "" {
				return "", "", "", updateFailure("update_package_invalid", "release contains duplicate SHA256SUMS.sig assets")
			}
			signatureURL = asset.DownloadURL
		}
	}
	if archiveURL == "" || checksumURL == "" || signatureURL == "" {
		return "", "", "", updateFailure(
			"update_package_missing", "latest release is missing %s, SHA256SUMS, or SHA256SUMS.sig", archiveName,
		)
	}
	return archiveURL, checksumURL, signatureURL, nil
}

func downloadUpdateResource(
	ctx context.Context,
	client *http.Client,
	resourceURL string,
	maximumBytes int64,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "MailCLI/"+version)
	response, err := client.Do(request)
	if err != nil {
		return nil, sanitizeUpdateRequestError(err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("HTTP %s", response.Status), closeErr)
	}
	if response.ContentLength > maximumBytes {
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("response exceeds %d bytes", maximumBytes), closeErr)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximumBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maximumBytes)
	}
	return payload, nil
}
