package ociconfigs

import "errors"

// ErrNoCache is returned when a file is requested but no local cache exists
// and the registry is unreachable.
var ErrNoCache = errors.New("no cached data available")

// ErrNotFound is returned when a logical name does not match any configured FileSpec.
var ErrNotFound = errors.New("file not found in config")

// ErrStaleCacheUsed is returned (non-fatally) when the remote digest check
// failed but valid cached files exist and were used instead.
var ErrStaleCacheUsed = errors.New("remote check failed; using stale cache")
