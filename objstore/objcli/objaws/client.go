package objaws

import (
	"bytes"
	"fmt"
	"io"
	"regexp"

	"github.com/couchbase/tools-common/log"
	"github.com/couchbase/tools-common/maths"
	"github.com/couchbase/tools-common/objstore/objcli"
	"github.com/couchbase/tools-common/objstore/objerr"
	"github.com/couchbase/tools-common/objstore/objval"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Client implements the 'objcli.Client' interface allowing the creation/management of objects stored in AWS S3.
type Client struct {
	serviceAPI serviceAPI
}

var _ objcli.Client = (*Client)(nil)

// NewClient returns a new client which uses the given 'serviceAPI', in general this should be the one created using the
// 's3.New' function exposed by the SDK.
func NewClient(serviceAPI serviceAPI) *Client {
	return &Client{serviceAPI: serviceAPI}
}

func (c *Client) GetObject(bucket, key string, br *objval.ByteRange) (*objval.Object, error) {
	if err := br.Valid(false); err != nil {
		return nil, err // Purposefully not wrapped
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	if br != nil {
		input.Range = aws.String(br.String())
	}

	resp, err := c.serviceAPI.GetObject(input)
	if err != nil {
		return nil, handleError(input.Bucket, input.Key, err)
	}

	attrs := objval.ObjectAttrs{
		Key:          key,
		Size:         *resp.ContentLength,
		LastModified: resp.LastModified,
	}

	object := &objval.Object{
		ObjectAttrs: attrs,
		Body:        resp.Body,
	}

	return object, nil
}

func (c *Client) GetObjectAttrs(bucket, key string) (*objval.ObjectAttrs, error) {
	input := &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	resp, err := c.serviceAPI.HeadObject(input)
	if err != nil {
		return nil, handleError(input.Bucket, input.Key, err)
	}

	attrs := &objval.ObjectAttrs{
		Key:          key,
		ETag:         *resp.ETag,
		Size:         *resp.ContentLength,
		LastModified: resp.LastModified,
	}

	return attrs, nil
}

func (c *Client) PutObject(bucket, key string, body io.ReadSeeker) error {
	input := &s3.PutObjectInput{
		Body:   body,
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	_, err := c.serviceAPI.PutObject(input)

	return handleError(input.Bucket, input.Key, err)
}

func (c *Client) AppendToObject(bucket, key string, data io.ReadSeeker) error {
	attrs, err := c.GetObjectAttrs(bucket, key)

	// As defined by the 'Client' interface, if the given object does not exist, we create it
	if objerr.IsNotFoundError(err) {
		return c.PutObject(bucket, key, data)
	}

	if err != nil {
		return fmt.Errorf("failed to get object attributes: %w", err)
	}

	if attrs.Size < MinUploadSize {
		return c.downloadAndAppend(bucket, attrs, data)
	}

	return c.createMPUThenCopyAndAppend(bucket, attrs, data)
}

// downloadAndAppend downloads an object, and appends the given data to it before uploading it back to S3; this should
// be used for objects which are less than 5MiB in size (i.e. under the multipart upload minium size).
func (c *Client) downloadAndAppend(bucket string, attrs *objval.ObjectAttrs, data io.ReadSeeker) error {
	object, err := c.GetObject(bucket, attrs.Key, nil)
	if err != nil {
		return fmt.Errorf("failed to get object: %w", err)
	}

	buffer := &bytes.Buffer{}

	_, err = io.Copy(buffer, io.MultiReader(object.Body, data))
	if err != nil {
		return fmt.Errorf("failed to download and append to object: %w", err)
	}

	err = c.PutObject(bucket, attrs.Key, bytes.NewReader(buffer.Bytes()))
	if err != nil {
		return fmt.Errorf("failed to upload updated object: %w", err)
	}

	return nil
}

// createMPUThenCopyAndAppend creates a multipart upload, then kicks off the copy and append operation.
func (c *Client) createMPUThenCopyAndAppend(bucket string, attrs *objval.ObjectAttrs, data io.ReadSeeker) error {
	id, err := c.CreateMultipartUpload(bucket, attrs.Key)
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}

	err = c.copyAndAppend(bucket, id, attrs, data)
	if err == nil {
		return nil
	}

	// NOTE: We've failed for some reason, we should try to cleanup after ourselves; the AWS client does not use the
	// given 'parts' argument, so we can omit it here
	if err := c.AbortMultipartUpload(bucket, id, attrs.Key); err != nil {
		log.Errorf(`(Objaws) Failed to abort multipart upload, it should be aborted manually | `+
			`{"id":"%s","key":"%s"}`, id, attrs.Key)
	}

	return err
}

// copyAndAppend copies all the data from the source object, then uploads a new part and completes the multipart upload.
// This has the affect of appending data to the object, without having to download, and re-upload.
func (c *Client) copyAndAppend(bucket, id string, attrs *objval.ObjectAttrs, data io.ReadSeeker) error {
	copied, err := c.UploadPartCopy(bucket, id, attrs.Key, attrs.Key, 1, &objval.ByteRange{End: attrs.Size - 1})
	if err != nil {
		return fmt.Errorf("failed to copy source object: %w", err)
	}

	appended, err := c.UploadPart(bucket, id, attrs.Key, 2, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	err = c.CompleteMultipartUpload(bucket, id, attrs.Key, copied, appended)
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	return nil
}

func (c *Client) DeleteObjects(bucket string, keys ...string) error {
	for start, end := 0, PageSize; start < len(keys); start, end = start+PageSize, end+PageSize {
		err := c.deleteObjects(bucket, keys[start:maths.Min(end, len(keys))]...)
		if err != nil {
			return err // Purposefully not wrapped
		}
	}

	return nil
}

func (c *Client) DeleteDirectory(bucket, prefix string) error {
	var err error

	callback := func(page *s3.ListObjectsV2Output, _ bool) bool {
		keys := make([]string, 0, len(page.Contents))

		for _, object := range page.Contents {
			keys = append(keys, *object.Key)
		}

		err = c.deleteObjects(bucket, keys...)

		return err == nil
	}

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	}

	// It's important we use an assignment expression here to avoid overwriting the error assigned by our callback
	if err := c.serviceAPI.ListObjectsV2Pages(input, callback); err != nil {
		return handleError(input.Bucket, nil, err)
	}

	return nil
}

// deleteObjects performs a batched delete operation for a single page (<=1000) of keys.
func (c *Client) deleteObjects(bucket string, keys ...string) error {
	input := &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &s3.Delete{Quiet: aws.Bool(true)},
	}

	for _, key := range keys {
		input.Delete.Objects = append(input.Delete.Objects, &s3.ObjectIdentifier{Key: aws.String(key)})
	}

	resp, err := c.serviceAPI.DeleteObjects(input)
	if err != nil {
		return handleError(input.Bucket, nil, err)
	}

	for _, err := range resp.Errors {
		if awsErr := awserr.New(*err.Code, *err.Message, nil); !isKeyNotFound(awsErr) {
			return handleError(input.Bucket, err.Key, awsErr)
		}
	}

	return nil
}

func (c *Client) IterateObjects(bucket, prefix string, include, exclude []*regexp.Regexp, fn objcli.IterateFunc) error {
	if include != nil && exclude != nil {
		return objcli.ErrIncludeAndExcludeAreMutuallyExclusive
	}

	var err error

	callback := func(page *s3.ListObjectsV2Output, _ bool) bool {
		err = c.iterateObjects(page.Contents, include, exclude, fn)
		return err == nil
	}

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	}

	// It's important we use an assignment expression here to avoid overwriting the error assigned by our callback
	if err := c.serviceAPI.ListObjectsV2Pages(input, callback); err != nil {
		return handleError(input.Bucket, nil, err)
	}

	return err
}

// iterateObjects iterates over the given page (<=1000) of objects executing the given function for each object which
// has not been explicitly ignored by the user.
func (c *Client) iterateObjects(objects []*s3.Object, include, exclude []*regexp.Regexp, fn objcli.IterateFunc) error {
	for _, object := range objects {
		if objcli.ShouldIgnore(*object.Key, include, exclude) {
			continue
		}

		attrs := &objval.ObjectAttrs{
			Key:          *object.Key,
			Size:         *object.Size,
			LastModified: object.LastModified,
		}

		// If the caller has returned an error, stop iteration, and return control to them
		if err := fn(attrs); err != nil {
			return err // Purposefully not wrapped
		}
	}

	return nil
}

func (c *Client) CreateMultipartUpload(bucket, key string) (string, error) {
	input := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	resp, err := c.serviceAPI.CreateMultipartUpload(input)
	if err != nil {
		return "", handleError(input.Bucket, input.Key, err)
	}

	return *resp.UploadId, nil
}

func (c *Client) UploadPart(bucket, id, key string, number int, body io.ReadSeeker) (objval.Part, error) {
	input := &s3.UploadPartInput{
		Body:       body,
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		PartNumber: aws.Int64(int64(number)),
		UploadId:   aws.String(id),
	}

	output, err := c.serviceAPI.UploadPart(input)
	if err != nil {
		return objval.Part{}, handleError(input.Bucket, input.Key, err)
	}

	return objval.Part{ID: *output.ETag, Number: number}, nil
}

// UploadPartCopy copies the provided byte range from the given 'src' object into a multipart upload for the given 'dst'
// object; this operation is specific to AWS and is required for implementing 'AppendToObject'
//
// NOTE: This function is not exposed by the 'objcli.Client' interface because it's not supported/required by all cloud
// providers.
func (c *Client) UploadPartCopy(bucket, id, dst, src string, number int, br *objval.ByteRange) (objval.Part, error) {
	if err := br.Valid(true); err != nil {
		return objval.Part{}, err // Purposefully not wrapped
	}

	input := &s3.UploadPartCopyInput{
		Bucket:          aws.String(bucket),
		CopySource:      aws.String(src),
		CopySourceRange: aws.String(br.String()),
		Key:             aws.String(dst),
		PartNumber:      aws.Int64(int64(number)),
		UploadId:        aws.String(id),
	}

	output, err := c.serviceAPI.UploadPartCopy(input)
	if err != nil {
		return objval.Part{}, handleError(input.Bucket, input.Key, err)
	}

	return objval.Part{ID: *output.CopyPartResult.ETag, Number: number}, nil
}

func (c *Client) CompleteMultipartUpload(bucket, id, key string, parts ...objval.Part) error {
	converted := make([]*s3.CompletedPart, len(parts))

	for index, part := range parts {
		converted[index] = &s3.CompletedPart{ETag: aws.String(part.ID), PartNumber: aws.Int64(int64(part.Number))}
	}

	input := &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(id),
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: converted},
	}

	_, err := c.serviceAPI.CompleteMultipartUpload(input)

	return err
}

func (c *Client) AbortMultipartUpload(bucket, id, key string, _ ...objval.Part) error {
	input := &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(id),
	}

	_, err := c.serviceAPI.AbortMultipartUpload(input)

	return err
}
