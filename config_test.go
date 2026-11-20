package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConfig(t *testing.T) {
	testCases := []struct {
		in       string
		expected *Config
		err      string
	}{
		{
			in:  "",
			err: "s3 bucket is required",
		},
		{
			in:  "x",
			err: "toml",
		},
		{
			in: `[s3]
				 bucket = "foo"`,
			expected: &Config{
				S3: S3Config{
					Bucket:               "foo",
					RequestPresignExpiry: Duration(2 * time.Hour),
				},
			},
		},
		{
			in: `[s3]
				 bucket = "foo"
				 request_presign_expiry = "1h"`,
			expected: &Config{
				S3: S3Config{
					Bucket:               "foo",
					RequestPresignExpiry: Duration(time.Hour),
				},
			},
		},
		{
			in: `[s3]
				 bucket = "foo"

				 [index]
				 max_objects = 100
				 concurrency = 2
				 tag_timeout = "3s"
				 scan_timeout = "1m"
				 refresh_interval = "10m"
				 max_tag_bytes = 1024
				 max_tag_requests = 8`,
			expected: &Config{
				S3: S3Config{
					Bucket:               "foo",
					RequestPresignExpiry: Duration(2 * time.Hour),
				},
				Index: IndexSettings{
					MaxObjects:      100,
					Concurrency:     2,
					TagTimeout:      Duration(3 * time.Second),
					ScanTimeout:     Duration(time.Minute),
					RefreshInterval: Duration(10 * time.Minute),
					MaxTagBytes:     1024,
					MaxTagRequests:  8,
				},
			},
		},
	}
	for i, tc := range testCases {
		t.Run(fmt.Sprintf("case %d", i), func(st *testing.T) {
			cfg, err := newConfig(strings.NewReader(tc.in))
			if tc.err != "" {
				assert.Contains(st, err.Error(), tc.err)
			} else {
				assert.NoError(st, err)
			}
			assert.EqualValues(st, tc.expected, cfg)
		})
	}
}
