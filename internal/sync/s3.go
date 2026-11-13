package sync

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

var ErrObjectNotFound = errors.New("s3 object not found")

type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

type S3Client struct {
	cfg        S3Config
	httpClient *http.Client
	baseURL    *url.URL
}

func NewS3Client(cfg S3Config) (*S3Client, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 endpoint %q: %w", cfg.Endpoint, err)
	}

	return &S3Client{
		cfg:        cfg,
		baseURL:    u,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// SetHTTPClient overrides the HTTP client (useful for unit testing with httptest).
func (c *S3Client) SetHTTPClient(client *http.Client) {
	c.httpClient = client
}

func (c *S3Client) Put(ctx context.Context, key string, data []byte) error {
	req, err := c.newRequest(ctx, http.MethodPut, key, nil, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	c.signRequest(req, data)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3 put %s failed (status %d): %s", key, resp.StatusCode, string(body))
	}
	return nil
}

func (c *S3Client) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	c.signRequest(req, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("s3 get %s failed (status %d): %s", key, resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

func (c *S3Client) Delete(ctx context.Context, key string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	c.signRequest(req, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3 delete %s failed (status %d): %s", key, resp.StatusCode, string(body))
	}
	return nil
}

func (c *S3Client) Head(ctx context.Context, key string) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return false, err
	}
	c.signRequest(req, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("s3 head %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("s3 head %s unexpected status %d", key, resp.StatusCode)
}

func (c *S3Client) newRequest(ctx context.Context, method, key string, query url.Values, data []byte) (*http.Request, error) {
	u := *c.baseURL
	// Path-style S3: /{bucket}/{key}
	cleanKey := strings.TrimPrefix(key, "/")
	if c.cfg.Bucket != "" {
		u.Path = path.Join(u.Path, c.cfg.Bucket, cleanKey)
	} else {
		u.Path = path.Join(u.Path, cleanKey)
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var bodyReader io.Reader
	if len(data) > 0 {
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	req.Host = c.baseURL.Host
	return req, nil
}

// signRequest signs the request with AWS Signature Version 4.
func (c *S3Client) signRequest(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	payloadHash := sha256Hex(payload)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers
	headersToSign := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	sort.Strings(headersToSign)

	var canonicalHeaders strings.Builder
	for _, h := range headersToSign {
		var val string
		if h == "host" {
			val = req.Host
		} else {
			val = req.Header.Get(h)
		}
		canonicalHeaders.WriteString(h + ":" + strings.TrimSpace(val) + "\n")
	}
	signedHeaders := strings.Join(headersToSign, ";")

	// Canonical URI
	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, c.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := getSignatureKey(c.cfg.SecretKey, dateStamp, c.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(key, dateStamp, regionName, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+key), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(regionName))
	kService := hmacSHA256(kRegion, []byte(serviceName))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}
