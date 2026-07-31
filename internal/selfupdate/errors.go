package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
)

type ErrorClass string

const (
	ClassNetwork          ErrorClass = "network"
	ClassManifestInvalid  ErrorClass = "manifest_invalid"
	ClassSignatureInvalid ErrorClass = "signature_invalid"
	ClassHashMismatch     ErrorClass = "hash_mismatch"
	ClassDecompression    ErrorClass = "decompression"
	ClassDiskFull         ErrorClass = "disk_full"
	ClassPermissionDenied ErrorClass = "permission_denied"
	ClassSwapFailed       ErrorClass = "swap_failed"
	ClassLocked           ErrorClass = "locked"
	ClassInternal         ErrorClass = "internal"
)

func (c ErrorClass) IsTamperSignal() bool {
	return c == ClassSignatureInvalid || c == ClassHashMismatch
}

// Error is an update failure tagged with the class to report for it.
type Error struct {
	Class ErrorClass
	Op    string
	Err   error
}

func (e *Error) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// classify tags err with a class and the operation that produced it.
func classify(class ErrorClass, op string, err error) *Error {
	return &Error{Class: class, Op: op, Err: err}
}

// classifyf is classify with a formatted cause.
func classifyf(class ErrorClass, op, format string, args ...any) *Error {
	return &Error{Class: class, Op: op, Err: fmt.Errorf(format, args...)}
}

// ClassOf returns the class to report for err, inferring one from the
// underlying syscall or net error when it was not tagged explicitly. An
// untagged, unrecognized error is "internal" rather than something more
// specific, so a mis-inference never masquerades as a tamper signal.
func ClassOf(err error) ErrorClass {
	if err == nil {
		return ""
	}

	var classified *Error
	if errors.As(err, &classified) {
		return classified.Class
	}

	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ClassDiskFull
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ClassPermissionDenied
	case isNetwork(err):
		return ClassNetwork
	default:
		return ClassInternal
	}
}

func isNetwork(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded)
}
