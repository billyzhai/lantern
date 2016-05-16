/*
Package errors defines error types used across Lantern project.
  // initialize globally
  errors.Initialize("appVersion", myErrorReporter, true)
  ...
  if n, err := Foo(); err != nil {
    errors.Wrap(err).Report() // or simply errors.Report(err)
  }

Wrap() method will try as much as possible to extract details from the error
passed in, if it's errors defined in Go standard library. For application
defined error type, at least the Go type name and what err.Error() returns will
be recorded.

Extra fields can be chained in any order, at any time.

  func Connect(addr string) *Error {
  	//...
    return errors.New("some error").ProxyAddr(addr).
	  WithOp("connect").With("some_counter", 1)
  }
  ...
  req *http.Request = ...
  if err := Connect(); err != nil {
	err.Request(req).With("proxy_all", true).Report()
  }

If logging=true when calling Initialize(), Report() will get a logger using
golog.LoggerFor("<package-name-in-which-the-error-is-created">) and call its
Error() method.

It's the caller's responsibility to avoid race condition accessing same error
instance from multiple goroutines.
*/
package errors

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/getlantern/context"
)

// New creates an Error with supplied description
func New(desc string) (e *Error) {
	e = buildError(desc, nil)
	e.attachStack(2)
	return
}

// Wrap creates an Error based on the information in an error instance.  It
// returns nil if the error passed in is nil, so we can simply call
// errors.Wrap(s.l.Close()) regardless there's an error or not. If the error is
// already wrapped, it is returned as is.
func Wrap(err error) *Error {
	return wrapSkipFrames(err, 1)
}

func wrapSkipFrames(err error, skip int) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	e := buildError(err.Error(), err)
	// always skip [Wrap, attachStack]
	e.attachStack(2 + skip)
	return e
}

// Error wraps system and application defined errors in unified structure for
// reporting and logging. It's not meant to be created directly. User New(),
// Wrap() and Report() instead.
type Error struct {
	data context.Map
}

// Fill implements the method from the context.Contextual interface.
func (e *Error) Fill(m context.Map) {
	for key, value := range e.data {
		m[key] = value
	}
}

// Op attaches a hint of the operation triggers this Error. Many error types
// returned by net and os package have Op pre-filled.
func (e *Error) Op(op string) *Error {
	e.data["error_op"] = op
	return e
}

// With attaches arbitrary field to the error. keys will be normalized as
// underscore_divided_words, so all characters except letters and numbers will
// be replaced with underscores, and all letters will be lowercased.
func (e *Error) With(key string, value interface{}) *Error {
	parts := strings.FieldsFunc(key, func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsNumber(c)
	})
	k := strings.ToLower(strings.Join(parts, "_"))
	switch actual := value.(type) {
	case string:
		e.data[k] = actual
	case int:
		e.data[k] = strconv.Itoa(actual)
	case bool:
		e.data[k] = strconv.FormatBool(actual)
	default:
		e.data[k] = fmt.Sprint(value)
	}
	return e
}

// Error satisfies the error interface
func (e *Error) Error() string {
	return e.data["error"].(string)
}

func (e *Error) attachStack(skip int) {
	// TODO: reenable this
	// caller := stack.Caller(skip)
	// e.data["p"]
	// e.Package = fmt.Sprintf("%+k", caller)
	// e.Func = fmt.Sprintf("%n", caller)
	// e.FileLine = fmt.Sprintf("%+v", caller)
	// e.Stack = stack.Trace().TrimBelow(caller).TrimRuntime()
}

func buildError(desc string, source error) *Error {
	e := &Error{
		// We initialize the data using the current context, which allows the error
		// to propagate contextual information to higher layers.
		data: context.AsMap(nil, false),
	}

	errorType := "errors.Error"
	if source != nil {
		op, goType, sourceDesc, extra := parseError(source)
		if desc == "" && source != nil {
			desc = sourceDesc
		}
		e.Op(op)
		errorType = goType
		if extra != nil {
			for key, value := range extra {
				e.data[key] = value
			}
		}
	}
	e.data["error"] = desc
	e.data["error_type"] = errorType

	return e
}

func parseError(err error) (op string, goType string, desc string, extra map[string]string) {
	extra = make(map[string]string)

	// interfaces
	if _, ok := err.(net.Error); ok {
		if opError, ok := err.(*net.OpError); ok {
			op = opError.Op
			if opError.Source != nil {
				extra["local_addr"] = opError.Source.String()
			}
			if opError.Addr != nil {
				extra["remote_addr"] = opError.Addr.String()
			}
			extra["network"] = opError.Net
			err = opError.Err
		}
		switch actual := err.(type) {
		case *net.AddrError:
			goType = "net.AddrError"
			desc = actual.Err
			extra["addr"] = actual.Addr
		case *net.DNSError:
			goType = "net.DNSError"
			desc = actual.Err
			extra["domain"] = actual.Name
			if actual.Server != "" {
				extra["dns_server"] = actual.Server
			}
		case *net.InvalidAddrError:
			goType = "net.InvalidAddrError"
			desc = actual.Error()
		case *net.ParseError:
			goType = "net.ParseError"
			desc = "invalid " + actual.Type
			extra["text_to_parse"] = actual.Text
		case net.UnknownNetworkError:
			goType = "net.UnknownNetworkError"
			desc = "unknown network"
		case syscall.Errno:
			goType = "syscall.Errno"
			desc = actual.Error()
		case *url.Error:
			goType = "url.Error"
			desc = actual.Err.Error()
			op = actual.Op
		default:
			goType = reflect.TypeOf(err).String()
			desc = err.Error()
		}
		return
	}
	if _, ok := err.(runtime.Error); ok {
		desc = err.Error()
		switch err.(type) {
		case *runtime.TypeAssertionError:
			goType = "runtime.TypeAssertionError"
		default:
			goType = reflect.TypeOf(err).String()
		}
		return
	}

	// structs
	switch actual := err.(type) {
	case *http.ProtocolError:
		desc = actual.ErrorString
		if name, ok := httpProtocolErrors[err]; ok {
			goType = name
		} else {
			goType = "http.ProtocolError"
		}
	case url.EscapeError, *url.EscapeError:
		goType = "url.EscapeError"
		desc = "invalid URL escape"
	case url.InvalidHostError, *url.InvalidHostError:
		goType = "url.InvalidHostError"
		desc = "invalid character in host name"
	case *textproto.Error:
		goType = "textproto.Error"
		desc = actual.Error()
	case textproto.ProtocolError, *textproto.ProtocolError:
		goType = "textproto.ProtocolError"
		desc = actual.Error()

	case tls.RecordHeaderError:
		goType = "tls.RecordHeaderError"
		desc = actual.Msg
		extra["header"] = hex.EncodeToString(actual.RecordHeader[:])
	case x509.CertificateInvalidError:
		goType = "x509.CertificateInvalidError"
		desc = actual.Error()
	case x509.ConstraintViolationError:
		goType = "x509.ConstraintViolationError"
		desc = actual.Error()
	case x509.HostnameError:
		goType = "x509.HostnameError"
		desc = actual.Error()
		extra["host"] = actual.Host
	case x509.InsecureAlgorithmError:
		goType = "x509.InsecureAlgorithmError"
		desc = actual.Error()
	case x509.SystemRootsError:
		goType = "x509.SystemRootsError"
		desc = actual.Error()
	case x509.UnhandledCriticalExtension:
		goType = "x509.UnhandledCriticalExtension"
		desc = actual.Error()
	case x509.UnknownAuthorityError:
		goType = "x509.UnknownAuthorityError"
		desc = actual.Error()
	case hex.InvalidByteError:
		goType = "hex.InvalidByteError"
		desc = "invalid byte"
	case *json.InvalidUTF8Error:
		goType = "json.InvalidUTF8Error"
		desc = "invalid UTF-8 in string"
	case *json.InvalidUnmarshalError:
		goType = "json.InvalidUnmarshalError"
		desc = actual.Error()
	case *json.MarshalerError:
		goType = "json.MarshalerError"
		desc = actual.Error()
	case *json.SyntaxError:
		goType = "json.SyntaxError"
		desc = actual.Error()
	case *json.UnmarshalFieldError:
		goType = "json.UnmarshalFieldError"
		desc = actual.Error()
	case *json.UnmarshalTypeError:
		goType = "json.UnmarshalTypeError"
		desc = actual.Error()
	case *json.UnsupportedTypeError:
		goType = "json.UnsupportedTypeError"
		desc = actual.Error()
	case *json.UnsupportedValueError:
		goType = "json.UnsupportedValueError"
		desc = actual.Error()

	case *os.LinkError:
		goType = "os.LinkError"
		desc = actual.Error()
	case *os.PathError:
		goType = "os.PathError"
		op = actual.Op
		desc = actual.Err.Error()
	case *os.SyscallError:
		goType = "os.SyscallError"
		op = actual.Syscall
		desc = actual.Err.Error()
	case *exec.Error:
		goType = "exec.Error"
		desc = actual.Err.Error()
	case *exec.ExitError:
		goType = "exec.ExitError"
		desc = actual.Error()
		// TODO: limit the length
		extra["stderr"] = string(actual.Stderr)
	case *strconv.NumError:
		goType = "strconv.NumError"
		desc = actual.Err.Error()
		extra["function"] = actual.Func
	case *time.ParseError:
		goType = "time.ParseError"
		desc = actual.Message
	default:
		desc = err.Error()
		if t, ok := miscErrors[err]; ok {
			goType = t
			return
		}
		goType = reflect.TypeOf(err).String()
	}
	return
}

var httpProtocolErrors = map[error]string{
	http.ErrHeaderTooLong:        "http.ErrHeaderTooLong",
	http.ErrShortBody:            "http.ErrShortBody",
	http.ErrNotSupported:         "http.ErrNotSupported",
	http.ErrUnexpectedTrailer:    "http.ErrUnexpectedTrailer",
	http.ErrMissingContentLength: "http.ErrMissingContentLength",
	http.ErrNotMultipart:         "http.ErrNotMultipart",
	http.ErrMissingBoundary:      "http.ErrMissingBoundary",
}

var miscErrors = map[error]string{
	bufio.ErrInvalidUnreadByte: "bufio.ErrInvalidUnreadByte",
	bufio.ErrInvalidUnreadRune: "bufio.ErrInvalidUnreadRune",
	bufio.ErrBufferFull:        "bufio.ErrBufferFull",
	bufio.ErrNegativeCount:     "bufio.ErrNegativeCount",
	bufio.ErrTooLong:           "bufio.ErrTooLong",
	bufio.ErrNegativeAdvance:   "bufio.ErrNegativeAdvance",
	bufio.ErrAdvanceTooFar:     "bufio.ErrAdvanceTooFar",
	bufio.ErrFinalToken:        "bufio.ErrFinalToken",

	http.ErrWriteAfterFlush:    "http.ErrWriteAfterFlush",
	http.ErrBodyNotAllowed:     "http.ErrBodyNotAllowed",
	http.ErrHijacked:           "http.ErrHijacked",
	http.ErrContentLength:      "http.ErrContentLength",
	http.ErrBodyReadAfterClose: "http.ErrBodyReadAfterClose",
	http.ErrHandlerTimeout:     "http.ErrHandlerTimeout",
	http.ErrLineTooLong:        "http.ErrLineTooLong",
	http.ErrMissingFile:        "http.ErrMissingFile",
	http.ErrNoCookie:           "http.ErrNoCookie",
	http.ErrNoLocation:         "http.ErrNoLocation",
	http.ErrSkipAltProtocol:    "http.ErrSkipAltProtocol",

	io.EOF:              "io.EOF",
	io.ErrClosedPipe:    "io.ErrClosedPipe",
	io.ErrNoProgress:    "io.ErrNoProgress",
	io.ErrShortBuffer:   "io.ErrShortBuffer",
	io.ErrShortWrite:    "io.ErrShortWrite",
	io.ErrUnexpectedEOF: "io.ErrUnexpectedEOF",

	os.ErrInvalid:    "os.ErrInvalid",
	os.ErrPermission: "os.ErrPermission",
	os.ErrExist:      "os.ErrExist",
	os.ErrNotExist:   "os.ErrNotExist",

	exec.ErrNotFound: "exec.ErrNotFound",

	x509.ErrUnsupportedAlgorithm: "x509.ErrUnsupportedAlgorithm",
	x509.IncorrectPasswordError:  "x509.IncorrectPasswordError",

	hex.ErrLength: "hex.ErrLength",
}
