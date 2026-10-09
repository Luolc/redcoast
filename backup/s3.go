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
	"os"
	"sort"
	"strings"
	"time"
)

// Credentials are an IAM user's long-term access key.
type Credentials struct {
	AccessKeyID, SecretAccessKey string
}

// errExists is the 412 S3 returns when If-None-Match: * finds the key already there.
var errExists = errors.New("key exists")

// maxErrorBody bounds the S3 error document read for its code.
const maxErrorBody = 8 << 10

// putObject uploads the file at path to key with one PutObject request signed with
// Signature Version 4. If-None-Match: * makes S3 refuse the write with 412 when the key
// exists, so a backup never replaces another one in the unversioned bucket.
func putObject(ctx context.Context, client *http.Client, endpoint, region string, creds Credentials, key, path string, now time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/"+key, file)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("If-None-Match", "*")
	sign(req, hex.EncodeToString(hash.Sum(nil)), region, creds, now)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// Only the code is reported: some error documents echo the access key ID.
	var document struct{ Code string }
	_ = xml.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&document)
	err = fmt.Errorf("put %s: HTTP %d %s", key, resp.StatusCode, document.Code)
	if resp.StatusCode == http.StatusPreconditionFailed {
		err = fmt.Errorf("%w: %v", errExists, err)
	}
	return err
}

// sign adds the x-amz-date, x-amz-content-sha256 and Authorization headers of
// Signature Version 4 for S3, signing the host and every header already on req.
func sign(req *http.Request, payloadHash, region string, creds Credentials, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	scope := amzDate[:8] + "/" + region + "/s3/aws4_request"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + headers[name] + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonical.String(), signed, payloadHash}, "\n")
	requestHash := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	key := []byte("AWS4" + creds.SecretAccessKey)
	for _, part := range []string{amzDate[:8], region, "s3", "aws4_request", toSign} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(key))
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
