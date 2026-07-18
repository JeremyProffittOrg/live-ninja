package deliv

// ZipJob is the async invocation payload from Service.Zip (web function)
// to cmd/deliverables-zipper, and RunZipJob is the zipper's core:
// streaming the source S3 objects through archive/zip into the target
// object without buffering any archive in memory or on disk (io.Pipe
// between the zip writer goroutine and the S3 multipart uploader). The
// logic lives here — not in cmd — so unit tests exercise real zip bytes
// against fakes.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ZipSource is one object to bundle: its (already prefix-validated at
// enqueue time, re-validated at run time) S3 key and the entry name it
// gets inside the archive.
type ZipSource struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// ZipJob is the deliverables-zipper Lambda's event payload.
type ZipJob struct {
	UserID        string      `json:"userId"`
	DeliverableID string      `json:"deliverableId"`
	SK            string      `json:"sk"`  // index item sort key for the status write-back
	Key           string      `json:"key"` // target object key deliverables/<uid>/<id>/<name>.zip
	Sources       []ZipSource `json:"sources"`
}

// GetObjectAPI is the S3 read surface the zipper needs.
type GetObjectAPI interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// UploadAPI matches manager.Uploader.Upload (streaming multipart upload
// from an io.Reader of unknown length — exactly what the pipe feeds it).
type UploadAPI interface {
	Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

// Validate re-checks the job invariants inside the zipper itself: the
// target and every source key MUST sit under the job user's own
// deliverables/<uid>/ prefix. The zipper trusts nothing about the payload
// even though the enqueuing web function already validated it — a
// misrouted or forged event must fail closed here.
func (j *ZipJob) Validate() error {
	switch {
	case j.UserID == "":
		return errors.New("zipjob: userId is required")
	case j.DeliverableID == "" || j.SK == "":
		return errors.New("zipjob: deliverableId and sk are required")
	case !strings.HasPrefix(j.SK, "DELIV#"):
		return errors.New("zipjob: sk is not a deliverable sort key")
	case len(j.Sources) == 0:
		return errors.New("zipjob: at least one source is required")
	case len(j.Sources) > MaxZipSources:
		return fmt.Errorf("zipjob: more than %d sources", MaxZipSources)
	}
	if !keyWithinUser(j.UserID, j.Key) {
		return fmt.Errorf("zipjob: target key %q escapes user prefix", j.Key)
	}
	for _, src := range j.Sources {
		if !keyWithinUser(j.UserID, src.Key) {
			return fmt.Errorf("zipjob: source key %q escapes user prefix", src.Key)
		}
	}
	return nil
}

// RunZipJob validates the job and streams the archive into place,
// returning the final object size in bytes. Sizing note: the byte count
// is taken on the writer side of the pipe, which is exactly the byte
// stream the uploader persists.
func RunZipJob(ctx context.Context, getter GetObjectAPI, uploader UploadAPI, bucket string, job ZipJob) (int64, error) {
	if err := job.Validate(); err != nil {
		return 0, err
	}
	if getter == nil || uploader == nil || bucket == "" {
		return 0, errors.New("zipjob: getter, uploader, and bucket are required")
	}

	entryNames := uniqueEntryNames(job.Sources)
	now := time.Now().UTC()

	pr, pw := io.Pipe()
	cw := &countingWriter{w: pw}

	go func() {
		zw := zip.NewWriter(cw)
		err := func() error {
			for i, src := range job.Sources {
				out, err := getter.GetObject(ctx, &s3.GetObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(src.Key),
				})
				if err != nil {
					return fmt.Errorf("get %s: %w", src.Key, err)
				}
				w, err := zw.CreateHeader(&zip.FileHeader{
					Name:     entryNames[i],
					Method:   zip.Deflate,
					Modified: now,
				})
				if err != nil {
					_ = out.Body.Close()
					return fmt.Errorf("zip entry %s: %w", entryNames[i], err)
				}
				if _, err := io.Copy(w, out.Body); err != nil {
					_ = out.Body.Close()
					return fmt.Errorf("stream %s: %w", src.Key, err)
				}
				if err := out.Body.Close(); err != nil {
					return fmt.Errorf("close %s: %w", src.Key, err)
				}
			}
			return zw.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err) // surfaces on the uploader's read side
			return
		}
		_ = pw.Close()
	}()

	if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(job.Key),
		Body:        pr,
		ContentType: aws.String("application/zip"),
	}); err != nil {
		_ = pr.CloseWithError(err) // unblock the writer goroutine
		return 0, fmt.Errorf("zipjob: upload %s: %w", job.Key, err)
	}
	return cw.n, nil
}

// uniqueEntryNames dedupes archive entry names (two deliverables may both
// be called "notes.md") by inserting " (n)" before the extension, and
// sanitizes each name so a stored display name can never smuggle a path.
func uniqueEntryNames(sources []ZipSource) []string {
	used := make(map[string]int, len(sources))
	names := make([]string, len(sources))
	for i, src := range sources {
		base := SanitizeFilename(src.Name)
		n := used[base]
		used[base] = n + 1
		if n == 0 {
			names[i] = base
			continue
		}
		ext := ""
		stem := base
		if dot := strings.LastIndex(base, "."); dot > 0 {
			stem, ext = base[:dot], base[dot:]
		}
		names[i] = fmt.Sprintf("%s (%d)%s", stem, n+1, ext)
	}
	return names
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
