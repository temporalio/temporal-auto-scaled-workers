package region

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidRegionID(t *testing.T) {
	for _, id := range []string{"aws-us-east-1", "gcp-us-central1", "azure-eastus2"} {
		assert.True(t, ValidRegionID(id), id)
	}
	for _, id := range []string{"", " aws-us-east-1", "AWS-us-east-1", "aws_us_east_1", "aws-us-east-1-", "-aws", "aws--us", "aws us"} {
		assert.False(t, ValidRegionID(id), id)
	}
}
