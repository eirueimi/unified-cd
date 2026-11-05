package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// SigV4Params describes one request to sign with AWS Signature Version 4.
// Headers holds every header to sign, keyed by lower-case name, and must
// include "host". Path and Query must already be URI-encoded.
type SigV4Params struct {
	Method, Path, Query string
	Headers             map[string]string
	Body                []byte
	AccessKeyID         string
	SecretAccessKey     string
	Region, Service     string
	Time                time.Time
}

// SignV4 returns the value of the Authorization header for p. It signs exactly
// the headers in p.Headers. It is a minimal signer for the single request shape
// the agent needs; it does not normalise paths or sort repeated query keys.
func SignV4(p SigV4Params) string {
	names := make([]string, 0, len(p.Headers))
	values := make(map[string]string, len(p.Headers))
	for name, value := range p.Headers {
		lower := strings.ToLower(name)
		names = append(names, lower)
		values[lower] = value
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.Join(strings.Fields(values[name]), " ") + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	path := p.Path
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{p.Method, path, p.Query, canonicalHeaders.String(), signedHeaders, hexSHA256(p.Body)}, "\n")

	t := p.Time.UTC()
	date := t.Format("20060102")
	scope := date + "/" + p.Region + "/" + p.Service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + t.Format("20060102T150405Z") + "\n" + scope + "\n" + hexSHA256([]byte(canonical))

	key := hmacSHA256([]byte("AWS4"+p.SecretAccessKey), date)
	key = hmacSHA256(key, p.Region)
	key = hmacSHA256(key, p.Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	return "AWS4-HMAC-SHA256 Credential=" + p.AccessKeyID + "/" + scope + ", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
