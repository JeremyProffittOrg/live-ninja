package webapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCertificateFingerprint = "01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF"
const testPlayCertificateFingerprint = "FE:DC:BA:98:76:54:32:10:FE:DC:BA:98:76:54:32:10:FE:DC:BA:98:76:54:32:10:FE:DC:BA:98:76:54:32:10"

const testAPKDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type androidDocumentS3 struct {
	objects map[string][]byte
	err     error
	bucket  string
	key     string
}

func (f *androidDocumentS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.bucket = aws.ToString(in.Bucket)
	f.key = aws.ToString(in.Key)
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.objects[f.key]
	if !ok {
		return nil, errors.New("missing fake object")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func validLatestDocument() []byte {
	return []byte(`{
		"schemaVersion": 1,
		"packageName": "ninja.jeremy.liveninja",
		"versionName": "0.2.1-hal",
		"versionCode": 4,
		"url": "https://live.jeremy.ninja/static/models/downloads/liveninja-0.2.1-hal-4-` + testAPKDigest + `.apk",
		"sha256": "` + testAPKDigest + `",
		"sizeBytes": 123456,
		"certificateSha256": "` + testCertificateFingerprint + `",
		"publishedAt": "2026-07-25T12:34:56Z",
		"gitSha": "deadbee",
		"futureOptionalField": "preserved"
	}`)
}

func TestAndroidLatestRouteReturnsValidatedReleaseMetadata(t *testing.T) {
	fake := &androidDocumentS3{objects: map[string][]byte{AndroidLatestObjectKey: validLatestDocument()}}
	app := fiber.New()
	RegisterAndroidDistributionRoutes(app, &Deps{
		AndroidArtifacts:       fake,
		AndroidArtifactsBucket: "assets-test",
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/app/android/latest", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-cache, no-store, must-revalidate", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "assets-test", fake.bucket)
	assert.Equal(t, AndroidLatestObjectKey, fake.key)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, string(validLatestDocument()), string(body))
}

func TestAndroidLatestRouteFailsClosedOnInvalidMetadata(t *testing.T) {
	fake := &androidDocumentS3{objects: map[string][]byte{
		AndroidLatestObjectKey: []byte(`{
			"schemaVersion":1,
			"packageName":"ninja.jeremy.liveninja",
			"versionName":"1.0.0",
			"versionCode":10,
			"url":"http://attacker.example/app.apk",
			"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"sizeBytes":1,
			"certificateSha256":"` + testCertificateFingerprint + `",
			"publishedAt":"2026-07-25T12:34:56Z",
			"gitSha":"deadbee"
		}`),
	}}
	app := fiber.New()
	RegisterAndroidDistributionRoutes(app, &Deps{
		AndroidArtifacts:       fake,
		AndroidArtifactsBucket: "assets-test",
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/app/android/latest", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestAndroidAssetLinksRouteReturnsSignerDocument(t *testing.T) {
	document := []byte(`[{
		"relation":["delegate_permission/common.handle_all_urls"],
		"target":{
			"namespace":"android_app",
			"package_name":"ninja.jeremy.liveninja",
			"sha256_cert_fingerprints":["` + testCertificateFingerprint + `"]
		}
	}]`)
	fake := &androidDocumentS3{objects: map[string][]byte{AndroidAssetLinksObjectKey: document}}
	app := fiber.New()
	RegisterAndroidDistributionRoutes(app, &Deps{
		AndroidArtifacts:       fake,
		AndroidArtifactsBucket: "assets-test",
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "public, max-age=300, must-revalidate", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, string(document), string(body))
}

func TestValidateAssetLinksAcceptsSideloadAndPlaySigners(t *testing.T) {
	statement := assetLinksStatement{
		Relation: []string{"delegate_permission/common.handle_all_urls"},
		Target: assetLinksTarget{
			Namespace:   "android_app",
			PackageName: androidPackageName,
			SHA256CertFingerprints: []string{
				testCertificateFingerprint,
				testPlayCertificateFingerprint,
			},
		},
	}
	require.NoError(t, validateAssetLinks([]assetLinksStatement{statement}))

	statement.Target.SHA256CertFingerprints = append(
		statement.Target.SHA256CertFingerprints,
		"AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA",
	)
	require.Error(t, validateAssetLinks([]assetLinksStatement{statement}))
}

func TestValidateAssetLinksRejectsDuplicateSigners(t *testing.T) {
	statement := assetLinksStatement{
		Relation: []string{"delegate_permission/common.handle_all_urls"},
		Target: assetLinksTarget{
			Namespace:   "android_app",
			PackageName: androidPackageName,
			SHA256CertFingerprints: []string{
				testCertificateFingerprint,
				testCertificateFingerprint,
			},
		},
	}
	require.Error(t, validateAssetLinks([]assetLinksStatement{statement}))
}

func TestAndroidDistributionRoutesReportUnconfigured(t *testing.T) {
	app := fiber.New()
	RegisterAndroidDistributionRoutes(app, &Deps{})

	for _, path := range []string{"/v1/app/android/latest", "/.well-known/assetlinks.json"} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, path)
	}
}
