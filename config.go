package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const Delimiter = "/"

type Duration time.Duration

func (d *Duration) UnmarshalText(data []byte) error {
	val, err := time.ParseDuration(string(data))
	*d = Duration(val)
	return err
}

type S3Credentials struct {
	ID     string
	Secret string
	Token  string
}

type S3Config struct {
	Region               string
	Endpoint             string
	Bucket               string
	BasePrefix           string   `toml:"base_prefix"`
	RequestPresignExpiry Duration `toml:"request_presign_expiry"`
	ForcePathStyle       bool     `toml:"force_path_style"`
	Credentials          *S3Credentials
}

// IndexSettings is the TOML-facing form of IndexConfig. Zero values select
// the defaults documented on IndexConfig.
type IndexSettings struct {
	MaxObjects      int      `toml:"max_objects"`
	Concurrency     int      `toml:"concurrency"`
	TagTimeout      Duration `toml:"tag_timeout"`
	ScanTimeout     Duration `toml:"scan_timeout"`
	RefreshInterval Duration `toml:"refresh_interval"`
	MaxTagBytes     int64    `toml:"max_tag_bytes"`
	MaxTagRequests  int      `toml:"max_tag_requests"`
}

func (s IndexSettings) indexConfig() IndexConfig {
	return IndexConfig{
		MaxObjects:      s.MaxObjects,
		Concurrency:     s.Concurrency,
		TagTimeout:      time.Duration(s.TagTimeout),
		ScanTimeout:     time.Duration(s.ScanTimeout),
		RefreshInterval: time.Duration(s.RefreshInterval),
		Limits: TagReadLimits{
			MaxBytes:    s.MaxTagBytes,
			MaxRequests: s.MaxTagRequests,
		},
	}
}

type Config struct {
	S3    S3Config
	Index IndexSettings
}

var errMissingBucket = errors.New("s3 bucket is required")

func newConfig(r io.Reader) (*Config, error) {
	cfg := &Config{
		S3: S3Config{
			RequestPresignExpiry: Duration(2 * time.Hour),
		},
	}
	dec := toml.NewDecoder(r)
	if err := dec.Decode(cfg); err != nil {
		return nil, err
	}
	if cfg.S3.Bucket == "" {
		return nil, errMissingBucket
	}
	if cfg.S3.BasePrefix != "" && !strings.HasSuffix(cfg.S3.BasePrefix, Delimiter) {
		cfg.S3.BasePrefix += Delimiter
	}
	// Negative values disable the corresponding index limit; leave them as-is.
	return cfg, nil
}

func NewConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return newConfig(f)
}
