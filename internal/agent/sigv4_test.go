package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// AWS SigV4 test-suite vector "get-vanilla".
func TestSignV4_GetVanilla(t *testing.T) {
	got := SignV4(SigV4Params{
		Method:          "GET",
		Path:            "/",
		Headers:         map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z"},
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		Service:         "service",
		Time:            time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC),
	})
	assert.Equal(t, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, "+
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31", got)
}
