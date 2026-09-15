package model

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	s3BucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3RegionRe = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
)

// NormalizeBackupS3 trims the fields, adds https:// to a bare endpoint,
// defaults the region and makes a non-empty prefix end with "/".
func NormalizeBackupS3(c BackupS3Settings) BackupS3Settings {
	c.Endpoint = strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	if c.Endpoint != "" && !strings.Contains(c.Endpoint, "://") {
		c.Endpoint = "https://" + c.Endpoint
	}
	c.Region = strings.ToLower(strings.TrimSpace(c.Region))
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.Prefix = strings.Trim(strings.TrimSpace(c.Prefix), "/")
	if c.Prefix != "" {
		c.Prefix += "/"
	}
	c.AccessKeyID = strings.TrimSpace(c.AccessKeyID)
	c.SecretAccessKey = strings.TrimSpace(c.SecretAccessKey)
	return c
}

// ValidateBackupS3 checks a normalized destination. Fields are prefixed
// "s3." so the UI can show them next to the inputs.
func ValidateBackupS3(c BackupS3Settings) Errs {
	e := Errs{}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			e.Add("s3.endpoint", "Use a URL like https://<account>.r2.cloudflarestorage.com, without a bucket or path")
		}
	}
	if !s3RegionRe.MatchString(c.Region) {
		e.Add("s3.region", "Use a region like us-east-1, eu-central-1 or auto")
	}
	if !s3BucketRe.MatchString(c.Bucket) {
		e.Add("s3.bucket", "Bucket names use 3–63 lowercase letters, digits, dots and dashes")
	}
	if strings.Contains(c.Prefix, "..") || strings.ContainsAny(c.Prefix, "\\\x00") {
		e.Add("s3.prefix", "Use a folder like relay/ or backups/relay/")
	}
	if c.AccessKeyID == "" {
		e.Add("s3.accessKeyId", "Access key ID is required")
	}
	if c.SecretAccessKey == "" {
		e.Add("s3.secretAccessKey", "Secret access key is required")
	}
	return e
}
