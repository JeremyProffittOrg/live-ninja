package webapp

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/gofiber/fiber/v2"
)

const (
	// AndroidLatestObjectKey and AndroidAssetLinksObjectKey are stable pointers
	// written only after Android release CI has built and verified a signed APK.
	AndroidLatestObjectKey     = "static/models/downloads/android-latest.json"
	AndroidAssetLinksObjectKey = "static/models/downloads/android-assetlinks.json"

	androidPackageName    = "ninja.jeremy.liveninja"
	maxAndroidDocumentLen = 64 << 10
)

var errAndroidDocumentNotConfigured = errors.New("android distribution documents are not configured")

type androidLatestRelease struct {
	SchemaVersion     int    `json:"schemaVersion"`
	PackageName       string `json:"packageName"`
	VersionName       string `json:"versionName"`
	VersionCode       int64  `json:"versionCode"`
	URL               string `json:"url"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"sizeBytes"`
	CertificateSHA256 string `json:"certificateSha256"`
	PublishedAt       string `json:"publishedAt"`
	GitSHA            string `json:"gitSha"`
}

type assetLinksStatement struct {
	Relation []string         `json:"relation"`
	Target   assetLinksTarget `json:"target"`
}

type assetLinksTarget struct {
	Namespace              string   `json:"namespace"`
	PackageName            string   `json:"package_name"`
	SHA256CertFingerprints []string `json:"sha256_cert_fingerprints"`
}

// RegisterAndroidDistributionRoutes mounts the two public, pre-auth Android
// distribution documents. Both are backed by release-CI-generated S3 objects,
// so the certificate fingerprint always comes from the signer that produced
// the advertised APK.
func RegisterAndroidDistributionRoutes(app *fiber.App, deps *Deps) {
	app.Get("/v1/app/android/latest", handleAndroidLatest(deps))
	app.Get("/.well-known/assetlinks.json", handleAndroidAssetLinks(deps))
}

func handleAndroidLatest(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		key := AndroidLatestObjectKey
		if deps != nil {
			key = documentKey(deps.AndroidLatestKey, key)
		}
		body, err := readAndroidDocument(c, deps, key)
		if err != nil {
			return androidDocumentError(c, deps, "latest release", err)
		}

		var release androidLatestRelease
		if err := decodeSingleJSON(body, &release); err != nil {
			logAndroidDocumentError(deps, "latest release document is invalid", err)
			return errorJSON(c, fiber.StatusServiceUnavailable, "release_metadata_invalid",
				"The Android release metadata is temporarily unavailable.")
		}
		if err := validateAndroidLatest(release); err != nil {
			logAndroidDocumentError(deps, "latest release document is invalid", err)
			return errorJSON(c, fiber.StatusServiceUnavailable, "release_metadata_invalid",
				"The Android release metadata is temporarily unavailable.")
		}

		c.Set(fiber.HeaderCacheControl, "no-cache, no-store, must-revalidate")
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		return c.Send(body)
	}
}

func handleAndroidAssetLinks(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		key := AndroidAssetLinksObjectKey
		if deps != nil {
			key = documentKey(deps.AndroidAssetLinksKey, key)
		}
		body, err := readAndroidDocument(c, deps, key)
		if err != nil {
			return androidDocumentError(c, deps, "Digital Asset Links", err)
		}

		var statements []assetLinksStatement
		if err := decodeSingleJSON(body, &statements); err != nil {
			logAndroidDocumentError(deps, "Digital Asset Links document is invalid", err)
			return errorJSON(c, fiber.StatusServiceUnavailable, "assetlinks_invalid",
				"The Android app-link configuration is temporarily unavailable.")
		}
		if err := validateAssetLinks(statements); err != nil {
			logAndroidDocumentError(deps, "Digital Asset Links document is invalid", err)
			return errorJSON(c, fiber.StatusServiceUnavailable, "assetlinks_invalid",
				"The Android app-link configuration is temporarily unavailable.")
		}

		c.Set(fiber.HeaderCacheControl, "public, max-age=300, must-revalidate")
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		return c.Send(body)
	}
}

func readAndroidDocument(c *fiber.Ctx, deps *Deps, key string) ([]byte, error) {
	if deps == nil || deps.AndroidArtifacts == nil || strings.TrimSpace(deps.AndroidArtifactsBucket) == "" {
		return nil, errAndroidDocumentNotConfigured
	}
	out, err := deps.AndroidArtifacts.GetObject(c.Context(), &s3.GetObjectInput{
		Bucket: aws.String(deps.AndroidArtifactsBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	if out.Body == nil {
		return nil, errors.New("S3 returned an empty body")
	}
	defer out.Body.Close()

	body, err := io.ReadAll(io.LimitReader(out.Body, maxAndroidDocumentLen+1))
	if err != nil {
		return nil, fmt.Errorf("read S3 object: %w", err)
	}
	if len(body) > maxAndroidDocumentLen {
		return nil, errors.New("S3 object exceeds size limit")
	}
	return body, nil
}

func androidDocumentError(c *fiber.Ctx, deps *Deps, name string, err error) error {
	switch {
	case errors.Is(err, errAndroidDocumentNotConfigured):
		return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured",
			"Android distribution is not configured.")
	case isS3NotFound(err):
		return errorJSON(c, fiber.StatusNotFound, "release_not_available",
			"No signed Android release is available yet.")
	default:
		logAndroidDocumentError(deps, name+" document read failed", err)
		return errorJSON(c, fiber.StatusServiceUnavailable, "release_metadata_unavailable",
			"The Android release metadata is temporarily unavailable.")
	}
}

func logAndroidDocumentError(deps *Deps, message string, err error) {
	if deps == nil || deps.Log == nil {
		return
	}
	if err == nil {
		deps.Log.Error(message)
		return
	}
	deps.Log.Error(message, "error", err.Error())
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return true
	case "AccessDenied":
		// This Lambda deliberately has GetObject on the two exact document
		// keys, but not broad s3:ListBucket. S3 therefore masks a missing key
		// as AccessDenied rather than NoSuchKey. Once release CI creates the
		// object, the exact-key GetObject grant succeeds normally.
		return true
	default:
		return false
	}
}

func documentKey(configured, fallback string) string {
	if key := strings.TrimSpace(configured); key != "" {
		return key
	}
	return fallback
}

func decodeSingleJSON(body []byte, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateAndroidLatest(release androidLatestRelease) error {
	if release.SchemaVersion != 1 {
		return errors.New("unsupported schemaVersion")
	}
	if release.PackageName != androidPackageName {
		return errors.New("unexpected packageName")
	}
	if strings.TrimSpace(release.VersionName) == "" || len(release.VersionName) > 64 || release.VersionCode < 1 {
		return errors.New("invalid version")
	}
	apkURL, err := url.Parse(release.URL)
	if err != nil || apkURL.Scheme != "https" || apkURL.Host != "live.jeremy.ninja" ||
		apkURL.User != nil || !strings.HasPrefix(apkURL.Path, "/static/models/downloads/") ||
		!strings.HasSuffix(apkURL.Path, ".apk") || apkURL.RawQuery != "" || apkURL.Fragment != "" {
		return errors.New("invalid APK URL")
	}
	if release.SizeBytes < 1 {
		return errors.New("invalid APK size")
	}
	if !validHexDigest(release.SHA256, 32) {
		return errors.New("invalid APK SHA-256")
	}
	if !strings.HasSuffix(apkURL.Path, "-"+strings.ToLower(release.SHA256)+".apk") {
		return errors.New("APK URL is not content-addressed by its SHA-256")
	}
	if !validCertificateFingerprint(release.CertificateSHA256) {
		return errors.New("invalid certificate SHA-256")
	}
	if _, err := time.Parse(time.RFC3339, release.PublishedAt); err != nil {
		return errors.New("invalid publishedAt")
	}
	if len(release.GitSHA) < 7 || len(release.GitSHA) > 64 || !validHexString(release.GitSHA) {
		return errors.New("invalid gitSha")
	}
	return nil
}

func validateAssetLinks(statements []assetLinksStatement) error {
	if len(statements) != 1 {
		return errors.New("expected exactly one statement")
	}
	statement := statements[0]
	if len(statement.Relation) != 1 || statement.Relation[0] != "delegate_permission/common.handle_all_urls" {
		return errors.New("unexpected relation")
	}
	if statement.Target.Namespace != "android_app" || statement.Target.PackageName != androidPackageName {
		return errors.New("unexpected Android target")
	}
	fingerprints := statement.Target.SHA256CertFingerprints
	if len(fingerprints) < 1 || len(fingerprints) > 2 {
		return errors.New("expected one or two certificate fingerprints")
	}
	seen := make(map[string]struct{}, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if !validCertificateFingerprint(fingerprint) {
			return errors.New("invalid certificate fingerprint")
		}
		normalized := strings.ToUpper(fingerprint)
		if _, exists := seen[normalized]; exists {
			return errors.New("duplicate certificate fingerprint")
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

func validHexDigest(value string, bytes int) bool {
	if len(value) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validHexString(value string) bool {
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return false
		}
	}
	return value != ""
}

func validCertificateFingerprint(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 32 {
		return false
	}
	for _, part := range parts {
		if len(part) != 2 {
			return false
		}
		if _, err := hex.DecodeString(part); err != nil {
			return false
		}
	}
	return value == strings.ToUpper(value)
}
