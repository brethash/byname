package backup

import (
	"bytes"
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Uploader writes objects to one bucket.
type S3Uploader struct {
	client *s3.Client
	bucket string
}

// NewS3 builds an uploader from the default AWS credential chain. On the Pi
// the systemd unit points AWS_SHARED_CREDENTIALS_FILE at a LoadCredential=
// file, so the key never sits in the environment or a world-readable path.
func NewS3(ctx context.Context, bucket, region string) (*S3Uploader, error) {
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &S3Uploader{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

// Put implements Uploader. Objects are written with SSE-S3; the bucket's
// versioning keeps every earlier upload even if a key is overwritten.
func (u *S3Uploader) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               aws.String(u.bucket),
		Key:                  aws.String(key),
		Body:                 bytes.NewReader(body),
		ContentType:          aws.String(contentType),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	return err
}
