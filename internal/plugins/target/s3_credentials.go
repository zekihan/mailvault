package target

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

var errS3Credentials = errors.New("credentials_ref must resolve to JSON with nonempty access_key_id and secret_access_key, and optional session_token")

func s3CredentialProvider(ref string) (aws.CredentialsProvider, error) {
	if ref == "" {
		return nil, nil
	}
	var values struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token"`
	}
	decoder := json.NewDecoder(strings.NewReader(ref))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, errS3Credentials
	}
	if decoder.Decode(new(interface{})) != io.EOF || values.AccessKeyID == "" || values.SecretAccessKey == "" {
		return nil, errS3Credentials
	}
	return credentials.NewStaticCredentialsProvider(values.AccessKeyID, values.SecretAccessKey, values.SessionToken), nil
}
