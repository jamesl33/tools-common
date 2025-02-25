package objgcp

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/google/uuid"
	"golang.org/x/exp/slices"
	"google.golang.org/api/iterator"

	"github.com/couchbase/tools-common/hofp"
	"github.com/couchbase/tools-common/log"
	"github.com/couchbase/tools-common/objstore/objcli"
	"github.com/couchbase/tools-common/objstore/objerr"
	"github.com/couchbase/tools-common/objstore/objval"
	"github.com/couchbase/tools-common/system"
)

// Client implements the 'objcli.Client' interface allowing the creation/management of objects stored in Google Storage.
type Client struct {
	serviceAPI serviceAPI
}

var _ objcli.Client = (*Client)(nil)

// NewClient returns a new client which uses the given storage client, in general this should be the one created using
// the 'storage.NewClient' function exposed by the SDK.
func NewClient(client *storage.Client) *Client {
	return &Client{serviceAPI: serviceClient{client}}
}

func (c *Client) Provider() objval.Provider {
	return objval.ProviderGCP
}

func (c *Client) GetObject(ctx context.Context, bucket, key string, br *objval.ByteRange) (*objval.Object, error) {
	if err := br.Valid(false); err != nil {
		return nil, err // Purposefully not wrapped
	}

	var offset, length int64 = 0, -1
	if br != nil {
		offset, length = br.ToOffsetLength(length)
	}

	reader, err := c.serviceAPI.Bucket(bucket).Object(key).NewRangeReader(ctx, offset, length)
	if err != nil {
		return nil, handleError(bucket, key, err)
	}

	remote := reader.Attrs()

	attrs := objval.ObjectAttrs{
		Key:          key,
		Size:         remote.Size,
		LastModified: aws.Time(remote.LastModified),
	}

	object := &objval.Object{
		ObjectAttrs: attrs,
		Body:        reader,
	}

	return object, nil
}

func (c *Client) GetObjectAttrs(ctx context.Context, bucket, key string) (*objval.ObjectAttrs, error) {
	remote, err := c.serviceAPI.Bucket(bucket).Object(key).Attrs(ctx)
	if err != nil {
		return nil, handleError(bucket, key, err)
	}

	attrs := &objval.ObjectAttrs{
		Key:          key,
		ETag:         remote.Etag,
		Size:         remote.Size,
		LastModified: &remote.Updated,
	}

	return attrs, nil
}

func (c *Client) PutObject(ctx context.Context, bucket, key string, body io.ReadSeeker) error {
	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	var (
		md5sum = md5.New()
		crc32c = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		// We always want to retry failed 'PutObject' requests, we generally have a lockfile which ensures (or we make
		// the assumption) that we have exclusive access to a given path prefix in GCP so we don't need to worry about
		// potentially overwriting objects.
		writer = c.serviceAPI.Bucket(bucket).Object(key).Retryer(storage.WithPolicy(storage.RetryAlways)).NewWriter(ctx)
	)

	_, err := aws.CopySeekableBody(io.MultiWriter(md5sum, crc32c), body)
	if err != nil {
		return fmt.Errorf("failed to calculate checksums: %w", err)
	}

	writer.SendMD5(md5sum.Sum(nil))
	writer.SendCRC(crc32c.Sum32())

	_, err = io.Copy(writer, body)
	if err != nil {
		return handleError(bucket, key, err)
	}

	return handleError(bucket, key, writer.Close())
}

func (c *Client) AppendToObject(ctx context.Context, bucket, key string, data io.ReadSeeker) error {
	attrs, err := c.GetObjectAttrs(ctx, bucket, key)

	// As defined by the 'Client' interface, if the given object does not exist, we create it
	if objerr.IsNotFoundError(err) || attrs.Size == 0 {
		return c.PutObject(ctx, bucket, key, data)
	}

	if err != nil {
		return fmt.Errorf("failed to get object attributes: %w", err)
	}

	id, err := c.CreateMultipartUpload(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("failed to start multipart upload: %w", err)
	}

	intermediate, err := c.UploadPart(ctx, bucket, id, key, 2, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	part := objval.Part{ID: key, Number: 1, Size: attrs.Size}

	err = c.CompleteMultipartUpload(ctx, bucket, id, key, part, intermediate)
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	return nil
}

func (c *Client) DeleteObjects(ctx context.Context, bucket string, keys ...string) error {
	pool := hofp.NewPool(hofp.Options{
		Context:   ctx,
		Size:      system.NumWorkers(len(keys)),
		LogPrefix: "(objgcp)",
	})

	del := func(ctx context.Context, key string) error {
		var (
			// We correctly handle the case where the object doesn't exist and should have exclusive access to the path
			// prefix in GCP, always retry.
			handle = c.serviceAPI.Bucket(bucket).Object(key).Retryer(storage.WithPolicy(storage.RetryAlways))
			err    = handle.Delete(ctx)
		)

		if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return handleError(bucket, key, err)
		}

		return nil
	}

	queue := func(key string) error {
		return pool.Queue(func(ctx context.Context) error { return del(ctx, key) })
	}

	for _, key := range keys {
		if queue(key) != nil {
			break
		}
	}

	return pool.Stop()
}

func (c *Client) DeleteDirectory(ctx context.Context, bucket, prefix string) error {
	fn := func(attrs *objval.ObjectAttrs) error {
		return c.DeleteObjects(ctx, bucket, attrs.Key)
	}

	return c.IterateObjects(ctx, bucket, prefix, "", nil, nil, fn)
}

func (c *Client) IterateObjects(ctx context.Context, bucket, prefix, delimiter string, include,
	exclude []*regexp.Regexp, fn objcli.IterateFunc,
) error {
	if include != nil && exclude != nil {
		return objcli.ErrIncludeAndExcludeAreMutuallyExclusive
	}

	query := &storage.Query{
		Prefix:     prefix,
		Delimiter:  delimiter,
		Projection: storage.ProjectionNoACL,
	}

	err := query.SetAttrSelection([]string{
		"Name",
		"Etag",
		"Size",
		"Updated",
	})
	if err != nil {
		return fmt.Errorf("failed to set attribute selection: %w", err)
	}

	it := c.serviceAPI.Bucket(bucket).Objects(ctx, query)

	for {
		remote, err := it.Next()

		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			return fmt.Errorf("failed to get next object: %w", err)
		}

		if objcli.ShouldIgnore(remote.Name, include, exclude) {
			continue
		}

		var (
			key     = remote.Prefix
			size    int64
			updated *time.Time
		)

		// If "key" is empty this isn't a directory stub, treat it as a normal object
		if key == "" {
			key = remote.Name
			size = remote.Size
			updated = &remote.Updated
		}

		attrs := &objval.ObjectAttrs{
			Key:          key,
			Size:         size,
			LastModified: updated,
		}

		// If the caller has returned an error, stop iteration, and return control to them
		if err = fn(attrs); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) CreateMultipartUpload(ctx context.Context, bucket, key string) (string, error) {
	return uuid.NewString(), nil
}

func (c *Client) ListParts(ctx context.Context, bucket, id, key string) ([]objval.Part, error) {
	var (
		prefix = partPrefix(id, key)
		parts  = make([]objval.Part, 0)
	)

	fn := func(attrs *objval.ObjectAttrs) error {
		parts = append(parts, objval.Part{
			ID:   attrs.Key,
			Size: attrs.Size,
		})

		return nil
	}

	err := c.IterateObjects(ctx, bucket, prefix, "/", nil, nil, fn)
	if err != nil {
		return nil, handleError(bucket, key, err)
	}

	return parts, nil
}

func (c *Client) UploadPart(
	ctx context.Context, bucket, id, key string, number int, body io.ReadSeeker,
) (objval.Part, error) {
	size, err := aws.SeekerLen(body)
	if err != nil {
		return objval.Part{}, fmt.Errorf("failed to determine body length: %w", err)
	}

	intermediate := partKey(id, key)

	err = c.PutObject(ctx, bucket, intermediate, body)
	if err != nil {
		return objval.Part{}, err // Purposefully not wrapped
	}

	return objval.Part{ID: intermediate, Number: number, Size: size}, nil
}

// NOTE: Google storage does not support byte range copying, therefore, only the entire object may be copied; this may
// be done by either not providing a byte range, or providing a byte range for the entire object.
func (c *Client) UploadPartCopy(
	ctx context.Context, bucket, id, dst, src string, number int, br *objval.ByteRange,
) (objval.Part, error) {
	if err := br.Valid(false); err != nil {
		return objval.Part{}, err // Purposefully not wrapped
	}

	attrs, err := c.GetObjectAttrs(ctx, bucket, src)
	if err != nil {
		return objval.Part{}, fmt.Errorf("failed to get object attributes: %w", err)
	}

	// If the user has provided a byte range, ensure that it's for the entire object
	if br != nil && !(br.Start == 0 && br.End == attrs.Size-1) {
		return objval.Part{}, objerr.ErrUnsupportedOperation
	}

	var (
		intermediate = partKey(id, dst)
		srcHdle      = c.serviceAPI.Bucket(bucket).Object(src)
		// Copying is non-destructive from the source perspective and we don't mind potentially "overwriting" the
		// destination object, always retry.
		dstHdle = c.serviceAPI.Bucket(bucket).Object(intermediate).Retryer(storage.WithPolicy(storage.RetryAlways))
	)

	_, err = dstHdle.CopierFrom(srcHdle).Run(ctx)
	if err != nil {
		return objval.Part{}, handleError(bucket, intermediate, err)
	}

	return objval.Part{ID: intermediate, Size: attrs.Size}, nil
}

func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, id, key string, parts ...objval.Part) error {
	converted := make([]string, 0, len(parts))

	for _, part := range parts {
		converted = append(converted, part.ID)
	}

	err := c.complete(ctx, bucket, key, converted...)
	if err != nil {
		return err
	}

	// Object composition may use the source object in the output, ensure that we don't delete it by mistake
	if idx := slices.Index(converted, key); idx >= 0 {
		converted = slices.Delete(converted, idx, idx+1)
	}

	c.cleanup(ctx, bucket, converted...)

	return nil
}

// complete recursively composes the object in chunks of 32 eventually resulting in a single complete object.
func (c *Client) complete(ctx context.Context, bucket, key string, parts ...string) error {
	if len(parts) <= MaxComposable {
		return c.compose(ctx, bucket, key, parts...)
	}

	intermediate := partKey(uuid.NewString(), key)
	defer c.cleanup(ctx, bucket, intermediate)

	err := c.compose(ctx, bucket, intermediate, parts[:MaxComposable]...)
	if err != nil {
		return err
	}

	return c.complete(ctx, bucket, key, append([]string{intermediate}, parts[MaxComposable:]...)...)
}

// compose the given parts into a single object.
func (c *Client) compose(ctx context.Context, bucket, key string, parts ...string) error {
	handles := make([]objectAPI, 0, len(parts))

	for _, part := range parts {
		handles = append(handles, c.serviceAPI.Bucket(bucket).Object(part))
	}

	var (
		// Object composition is non-destructive from the source perspective and we don't mind potentially "overwriting"
		// the destination object, always retry.
		dst    = c.serviceAPI.Bucket(bucket).Object(key).Retryer(storage.WithPolicy(storage.RetryAlways))
		_, err = dst.ComposerFrom(handles...).Run(ctx)
	)

	return handleError(bucket, key, err)
}

// cleanup attempts to remove the given keys, logging them if we receive an error.
func (c *Client) cleanup(ctx context.Context, bucket string, keys ...string) {
	err := c.DeleteObjects(ctx, bucket, keys...)
	if err == nil {
		return
	}

	log.Errorf(`(Objaws) Failed to cleanup intermediate keys, they should be removed manually `+
		`| {"keys":[%s],"error":"%s"}`, strings.Join(keys, ","), err)
}

func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, id, key string) error {
	return c.DeleteDirectory(ctx, bucket, partPrefix(id, key))
}
