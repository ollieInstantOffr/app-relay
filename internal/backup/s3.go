package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

// s3Client is a small S3 API client (Signature V4) for AWS S3 and
// S3-compatible storage such as Cloudflare R2, Backblaze B2, Wasabi and MinIO.
// Only what backups need: put, get, delete and list objects.
type s3Client struct {
	scheme    string
	host      string // endpoint host[:port], without the bucket
	region    string
	bucket    string
	prefix    string
	accessKey string
	secretKey string
	pathStyle bool
	http      *http.Client
	now       func() time.Time
}

// RemoteObject is a backup archive found in the bucket.
type RemoteObject struct {
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	BackupID     string    `json:"backupId,omitempty"` // set when a local snapshot row points at it
}

var emptySHA256 = func() string { s := sha256.Sum256(nil); return hex.EncodeToString(s[:]) }()

func newS3Client(c model.BackupS3Settings) (*s3Client, error) {
	c = model.NormalizeBackupS3(c)
	if c.Bucket == "" || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("S3 destination is not fully configured")
	}
	cl := &s3Client{
		scheme: "https", region: c.Region, bucket: c.Bucket, prefix: c.Prefix,
		accessKey: c.AccessKeyID, secretKey: c.SecretAccessKey, pathStyle: c.PathStyle,
		http: &http.Client{Timeout: 10 * time.Minute}, now: time.Now,
	}
	if c.Endpoint == "" {
		cl.host = "s3." + c.Region + ".amazonaws.com"
	} else {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid S3 endpoint %q", c.Endpoint)
		}
		cl.scheme, cl.host = u.Scheme, u.Host
	}
	return cl, nil
}

// Key returns the object key of a backup file name.
func (c *s3Client) Key(name string) string { return c.prefix + name }

func (c *s3Client) Put(ctx context.Context, key string, f *os.File) error {
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, key, nil, f, size, hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *s3Client) putBytes(ctx context.Context, key string, data []byte) error {
	sum := sha256.Sum256(data)
	resp, err := c.do(ctx, http.MethodPut, key, nil, strings.NewReader(string(data)), int64(len(data)), hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get returns the object body; the caller closes it.
func (c *s3Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0, emptySHA256)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *s3Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, 0, emptySHA256)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type listBucketResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns the backup archives below the prefix, newest first.
// maxKeys > 0 stops after the first page of that size (connection tests).
func (c *s3Client) List(ctx context.Context, maxKeys int) ([]RemoteObject, error) {
	out := []RemoteObject{}
	token := ""
	for page := 0; page < 50; page++ {
		q := url.Values{"list-type": {"2"}}
		if c.prefix != "" {
			q.Set("prefix", c.prefix)
		}
		if maxKeys > 0 {
			q.Set("max-keys", fmt.Sprint(maxKeys))
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0, emptySHA256)
		if err != nil {
			return nil, err
		}
		var res listBucketResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("S3 list: unexpected response: %w", err)
		}
		for _, o := range res.Contents {
			name := strings.TrimPrefix(o.Key, c.prefix)
			if strings.Contains(name, "/") || !strings.HasSuffix(name, fileExt) {
				continue
			}
			out = append(out, RemoteObject{Key: o.Key, Name: name, Size: o.Size, LastModified: o.LastModified})
		}
		if maxKeys > 0 || !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out, nil
}

// do signs and sends one request. Non-2xx responses become errors.
func (c *s3Client) do(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	host := c.host
	path := "/" + uriEncode(key, false)
	if c.pathStyle {
		path = "/" + c.bucket
		if key != "" {
			path += "/" + uriEncode(key, false)
		}
	} else {
		host = c.bucket + "." + c.host
	}
	rawQuery := canonicalQuery(query)
	u := c.scheme + "://" + host + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	c.sign(req, host, path, rawQuery, payloadHash)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("S3: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, s3Error(resp)
}

func (c *s3Client) sign(req *http.Request, host, path, rawQuery, payloadHash string) {
	t := c.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{
		req.Method,
		path,
		rawQuery,
		"host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + c.region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	k := hmacSHA256([]byte("AWS4"+c.secretKey), day)
	k = hmacSHA256(k, c.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncode encodes like the SigV4 spec: everything except unreserved
// characters, and "/" unless encodeSlash is false.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && !encodeSlash:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

type s3ErrorBody struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// s3Error turns an S3 error response into a readable error.
func s3Error(resp *http.Response) error {
	var e s3ErrorBody
	_ = xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	hint := ""
	switch e.Code {
	case "InvalidAccessKeyId":
		hint = "the access key ID is wrong"
	case "SignatureDoesNotMatch":
		hint = "the secret access key is wrong"
	case "NoSuchBucket":
		hint = "the bucket doesn't exist (check the name, region and endpoint)"
	case "AuthorizationHeaderMalformed", "IllegalLocationConstraintException", "PermanentRedirect":
		hint = "the region doesn't match the bucket"
	case "AccessDenied":
		hint = "the key isn't allowed to do this in the bucket"
	}
	if e.Code == "" {
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusTemporaryRedirect:
			hint = "the region or endpoint doesn't match the bucket"
		case http.StatusNotFound:
			hint = "not found (check the bucket name and endpoint)"
		case http.StatusForbidden:
			hint = "access denied"
		}
	}
	msg := fmt.Sprintf("S3 returned %d", resp.StatusCode)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if hint != "" {
		msg += ": " + hint
	} else if e.Message != "" {
		msg += ": " + e.Message
	}
	return errors.New(msg)
}
