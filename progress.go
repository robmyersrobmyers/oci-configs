package ociconfigs

// ProgressEvent describes the transfer state of a single file during a download.
type ProgressEvent struct {
	// Name is the logical name of the file being transferred.
	Name string
	// Total is the total number of bytes to transfer; 0 if unknown.
	Total int64
	// Completed is the number of bytes transferred so far.
	Completed int64
	// Done is true when the transfer has finished successfully.
	Done bool
	// Err is non-nil if the transfer failed.
	Err error
}

// ProgressFunc is called with progress updates during file downloads.
// It may be called from multiple goroutines concurrently.
type ProgressFunc func(ProgressEvent)
