package deliv

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGetter serves zip sources from an in-memory map.
type fakeGetter struct {
	objects map[string][]byte
	errKey  string // GetObject on this key fails
}

func (f *fakeGetter) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(params.Key)
	if key == f.errKey {
		return nil, errors.New("get denied")
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such key " + key)
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// fakeUploader drains the streaming body exactly like the real multipart
// uploader would, capturing the bytes for zip verification.
type fakeUploader struct {
	key         string
	contentType string
	body        []byte
	err         error
}

func (f *fakeUploader) Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	if f.err != nil {
		// Real uploader behavior on failure: stop reading and return.
		return nil, f.err
	}
	b, err := io.ReadAll(input.Body)
	if err != nil {
		// A pipe CloseWithError from the writer goroutine surfaces here.
		return nil, err
	}
	f.key = aws.ToString(input.Key)
	f.contentType = aws.ToString(input.ContentType)
	f.body = b
	return &manager.UploadOutput{}, nil
}

func validJob() ZipJob {
	return ZipJob{
		UserID:        "u1",
		DeliverableID: "z1",
		SK:            "DELIV#2026-07-17T12:00:00Z#z1",
		Key:           "deliverables/u1/z1/bundle.zip",
		Sources: []ZipSource{
			{Key: "deliverables/u1/a/notes.md", Name: "notes.md"},
			{Key: "deliverables/u1/b/notes.md", Name: "notes.md"}, // duplicate display name
			{Key: "deliverables/u1/c/data.csv", Name: "data.csv"},
		},
	}
}

func TestRunZipJobProducesValidZip(t *testing.T) {
	getter := &fakeGetter{objects: map[string][]byte{
		"deliverables/u1/a/notes.md": []byte("first notes"),
		"deliverables/u1/b/notes.md": []byte("second notes"),
		"deliverables/u1/c/data.csv": []byte("a,b\n1,2\n"),
	}}
	up := &fakeUploader{}

	size, err := RunZipJob(context.Background(), getter, up, "deliv-bucket", validJob())
	require.NoError(t, err)
	assert.Equal(t, "deliverables/u1/z1/bundle.zip", up.key)
	assert.Equal(t, "application/zip", up.contentType)
	assert.Equal(t, int64(len(up.body)), size, "reported size must match the uploaded byte stream")

	zr, err := zip.NewReader(bytes.NewReader(up.body), int64(len(up.body)))
	require.NoError(t, err, "uploaded bytes must be a well-formed zip")
	require.Len(t, zr.File, 3)

	want := map[string]string{
		"notes.md":     "first notes",
		"notes (2).md": "second notes", // duplicate names deduped, extension kept
		"data.csv":     "a,b\n1,2\n",
	}
	for _, f := range zr.File {
		expected, ok := want[f.Name]
		require.True(t, ok, "unexpected entry %q", f.Name)
		rc, err := f.Open()
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.Equal(t, expected, string(got), "entry %q", f.Name)
		delete(want, f.Name)
	}
	assert.Empty(t, want, "every source must appear in the archive")
}

func TestZipJobValidate(t *testing.T) {
	mutate := func(f func(*ZipJob)) ZipJob {
		j := validJob()
		f(&j)
		return j
	}
	bad := []ZipJob{
		mutate(func(j *ZipJob) { j.UserID = "" }),
		mutate(func(j *ZipJob) { j.DeliverableID = "" }),
		mutate(func(j *ZipJob) { j.SK = "" }),
		mutate(func(j *ZipJob) { j.SK = "SETTINGS" }),
		mutate(func(j *ZipJob) { j.Sources = nil }),
		mutate(func(j *ZipJob) { j.Key = "deliverables/u2/z1/bundle.zip" }),     // target escapes
		mutate(func(j *ZipJob) { j.Key = "wakewords/u1/z1/bundle.zip" }),        // wrong namespace
		mutate(func(j *ZipJob) { j.Sources[0].Key = "deliverables/u2/a/x.md" }), // source escapes
		mutate(func(j *ZipJob) {
			j.Sources = make([]ZipSource, MaxZipSources+1)
			for i := range j.Sources {
				j.Sources[i] = ZipSource{Key: "deliverables/u1/s/x.md", Name: "x.md"}
			}
		}),
	}
	for i, j := range bad {
		assert.Error(t, j.Validate(), "case %d must fail validation", i)
	}
	good := validJob()
	assert.NoError(t, good.Validate())
}

func TestRunZipJobRefusesInvalidJobBeforeAnyIO(t *testing.T) {
	getter := &fakeGetter{objects: map[string][]byte{}}
	up := &fakeUploader{}
	job := validJob()
	job.Key = "deliverables/other/z1/bundle.zip"

	_, err := RunZipJob(context.Background(), getter, up, "deliv-bucket", job)
	require.Error(t, err)
	assert.Empty(t, up.key, "nothing may be uploaded for an invalid job")
}

func TestRunZipJobSourceReadFailure(t *testing.T) {
	getter := &fakeGetter{
		objects: map[string][]byte{
			"deliverables/u1/a/notes.md": []byte("ok"),
			"deliverables/u1/c/data.csv": []byte("ok"),
		},
		errKey: "deliverables/u1/b/notes.md",
	}
	up := &fakeUploader{}

	_, err := RunZipJob(context.Background(), getter, up, "deliv-bucket", validJob())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deliverables/u1/b/notes.md",
		"the failing source key must be identifiable in the error")
}

func TestRunZipJobUploadFailure(t *testing.T) {
	getter := &fakeGetter{objects: map[string][]byte{
		"deliverables/u1/a/notes.md": []byte("x"),
		"deliverables/u1/b/notes.md": []byte("y"),
		"deliverables/u1/c/data.csv": []byte("z"),
	}}
	up := &fakeUploader{err: errors.New("multipart upload failed")}

	_, err := RunZipJob(context.Background(), getter, up, "deliv-bucket", validJob())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multipart upload failed")
}

func TestUniqueEntryNames(t *testing.T) {
	names := uniqueEntryNames([]ZipSource{
		{Name: "a.md"},
		{Name: "a.md"},
		{Name: "a.md"},
		{Name: "b"},
		{Name: "b"},
		{Name: "../evil.md"}, // stored display names are re-sanitized
	})
	assert.Equal(t, []string{"a.md", "a (2).md", "a (3).md", "b", "b (2)", "evil.md"}, names)
}
