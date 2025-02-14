package log

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"strings"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/getsentry/sentry-go"
)

// DefaultWrapper defines the default one to be used in log.Wrapper.
//
// It affects two places:
//
// 1. When using nil-safe calls on log.Wrapper on a nil log.Wrapper.
//
// 2. When unmarshaling from text (yaml) and the text is empty.
var DefaultWrapper Wrapper = ErrorWithSentryWrapper()

// Wrapper defines a simple interface to wrap logging functions.
//
// As principles, library code should:
//
// 1. Not do any logging.
// The library code should communicate errors back to the caller,
// and let the caller decide how to deal with them
// (log them, ignore them, panic, etc.)
//
// 2. In some rare cases, 1 is not possible,
// for example the error might happen in a background goroutine.
// In those cases some logging is necessary,
// but those should be kept at minimal,
// and the library code should provide control to the caller on how to do those
// logging.
//
// This interface is meant to solve Principle 2 above.
// In reality we might actually use different logging libraries in different
// services, and they are not always compatible with each other.
// Wrapper is a simple common ground that it's easy to wrap whatever logging
// library we use into.
//
// With that in mind, this interface should only be used by library code,
// when the case satisfies all of the following 3 criteria:
//
// 1. A bad thing happened.
//
// 2. This is unexpected.
// For expected errors,
// the library should either handle it by itself (e.g. retry),
// or communicate it back to the caller and let them handle it.
//
// 3. This is also recoverable.
// Unrecoverable errors should also be communicated back to the caller to handle.
//
// Baseplate services should use direct logging functions for their logging
// needs, instead of using Wrapper interface.
//
// For production code using baseplate libraries,
// Baseplate services should use ErrorWithSentryWrapper in most cases,
// as whenever the Wrapper is called that's something bad and unexpected
// happened and the service owner should be aware of.
// Non-Baseplate services should use error level in whatever logging library
// they use.
//
// metricsbp.LogWrapper also provides an implementation that emits metrics when
// it's called. It can be wrapped on top of other log.Wrapper implementations.
//
// For unit tests of library code using Wrapper,
// TestWrapper is provided that would fail the test when Wrapper is called.
//
// Not all Wrapper implementations take advantage of context object passed in,
// but the caller should always pass it into Wrapper if they already have one,
// or just use context.Background() if they don't have one.
type Wrapper func(ctx context.Context, msg string)

// Log is the nil-safe way of calling a log.Wrapper.
//
// If w is nil, DefaultWrapper will be used instead.
func (w Wrapper) Log(ctx context.Context, msg string) {
	if w == nil {
		w = DefaultWrapper
	}
	w(ctx, msg)
}

// ToThriftLogger wraps Wrapper into thrift.Logger.
func (w Wrapper) ToThriftLogger() thrift.Logger {
	if w == nil {
		w = DefaultWrapper
	}
	ctx := context.Background()
	return func(msg string) {
		w(ctx, msg)
	}
}

// UnmarshalText implements encoding.TextUnmarshaler.
//
// It makes Wrapper possible to be used directly in yaml and other config files.
//
// Please note that this currently only support limited implementations:
//
// - empty: DefaultWrapper.
//
// - "nop": NopWrapper.
//
// - "std": StdWrapper with default stdlib logger
// (log.New(os.Stderr, "", log.LstdFlags)).
//
// - "zap": ZapWrapper on default level (Info) with no kv pairs.
//
// - "zap:level:k1=v1,k2=v2...": ZapWrapper with given level and kv pairs, with
// the ":k=v..." part being optional. For example "zap:error" means ZapWrapper on
// Error level with no kv pairs, "zap:info:key1=value1" means ZapWrapper on Info
// level with "key1":"value1" pair.
//
// - "sentry": ErrorWithSentryWrapper.
//
// See the example on how to extend it to support other implementations.
func (w *Wrapper) UnmarshalText(text []byte) error {
	s := string(text)

	// Special handling for "zap:level" case
	const zapLevelPrefix = "zap:"
	if strings.HasPrefix(s, zapLevelPrefix) {
		split := strings.Split(s, ":")
		if len(split) > 3 {
			return fmt.Errorf(`log.Wrapper.UnmarshalText: malformed input: too many ":": %q`, s)
		}
		var pairs map[string]interface{}
		if len(split) > 2 {
			kvs := strings.Split(split[2], ",")
			pairs = make(map[string]interface{}, len(kvs))
			for _, kv := range kvs {
				kv = strings.TrimSpace(kv)
				index := strings.Index(kv, "=")
				if index < 0 {
					return fmt.Errorf(`log.Wrapper.UnmarshalText: malformed input: no "=" in kv pair %q`, kv)
				}
				key := kv[:index]
				val := kv[index+1:]
				if _, ok := pairs[key]; ok {
					return fmt.Errorf("log.Wrapper.UnmarshalText: malformed input: key %q appeared at least twice", key)
				}
				pairs[key] = val
			}
		}
		*w = ZapWrapper(ZapWrapperArgs{
			Level:   Level(strings.ToLower(split[1])),
			KVPairs: pairs,
		})
		return nil
	}

	switch s {
	default:
		return fmt.Errorf("unsupported log.Wrapper config: %q", text)
	case "":
		*w = DefaultWrapper
	case "nop":
		*w = NopWrapper
	case "std":
		*w = StdWrapper(stdlog.New(os.Stderr, "", stdlog.LstdFlags))
	case "zap":
		*w = ZapWrapper(ZapWrapperArgs{
			Level: Level(""),
		})
	case "sentry":
		*w = ErrorWithSentryWrapper()
	}
	return nil
}

var _ encoding.TextUnmarshaler = (*Wrapper)(nil)

// WrapToThriftLogger wraps a Wrapper into thrift.Logger.
func WrapToThriftLogger(w Wrapper) thrift.Logger {
	return w.ToThriftLogger()
}

// NopWrapper is a Wrapper implementation that does nothing.
//
// In most cases you don't need to use it directly.
// The zero value of log.Wrapper is essentially a NopWrapper.
func NopWrapper(ctx context.Context, msg string) {}

// StdWrapper wraps stdlib log package into a Wrapper.
func StdWrapper(logger *stdlog.Logger) Wrapper {
	if logger == nil {
		return NopWrapper
	}
	return func(_ context.Context, msg string) {
		logger.Print(msg)
	}
}

// TestWrapper is a wrapper can be used in test codes.
//
// It fails the test when called.
func TestWrapper(tb testing.TB) Wrapper {
	return func(_ context.Context, msg string) {
		tb.Helper()
		tb.Errorf("logger called with msg: %q", msg)
	}
}

// ZapWrapperArgs defines the args used in ZapWrapper.
type ZapWrapperArgs struct {
	Level   Level
	KVPairs map[string]interface{}
}

// ZapWrapper wraps zap log package into a Wrapper.
func ZapWrapper(args ZapWrapperArgs) Wrapper {
	if args.Level == NopLevel {
		return NopWrapper
	}

	kv := make([]interface{}, 0, len(args.KVPairs)*2)
	for k, v := range args.KVPairs {
		kv = append(kv, k, v)
	}

	return func(ctx context.Context, msg string) {
		logger := C(ctx)
		// For unknown values, fallback to info level.
		f := logger.Infow
		switch args.Level {
		case DebugLevel:
			f = logger.Debugw
		case WarnLevel:
			f = logger.Warnw
		case ErrorLevel:
			f = logger.Errorw
		case PanicLevel:
			f = logger.Panicw
		case FatalLevel:
			f = logger.Fatalw
		}
		f(msg, kv...)
	}
}

// ErrorWithSentryWrapper is a Wrapper implementation that both use Zap logger
// to log at error level, and also send the message to Sentry.
//
// In most cases this should be the one used to pass into Baseplate.go libraries
// expecting a log.Wrapper.
// If the service didn't configure sentry,
// then this wrapper is essentially the same as log.ZapWrapper(log.ErrorLevel).
//
// Note that this should not be used as the logger set into thrift.TSimpleServer,
// as that would use the logger to log network I/O errors,
// which would be too spammy to be sent to Sentry.
// For this reason, it's returning a Wrapper instead of being a Wrapper itself,
// thus forcing an extra typecasting to be used as a thrift.Logger.
func ErrorWithSentryWrapper() Wrapper {
	return func(ctx context.Context, msg string) {
		C(ctx).Error(msg)

		err := errors.New(msg)
		if hub := sentry.GetHubFromContext(ctx); hub != nil {
			hub.CaptureException(err)
		} else {
			sentry.CaptureException(err)
		}
	}
}

// Counter is a minimal interface for a counter.
//
// This is implemented by both prometheus counter and statsd counter from
// metricsbp.
type Counter interface {
	Add(float64)
}

// CounterWrapper returns a Wrapper implementation that increases
// counter by 1 then calls delegate to log the message.
//
// Please note that it's not possible to deserialize this Wrapper directly from
// yaml, so you usually need to override it in your main function, after
// baseplate.ParseConfigYAML call, for example:
//
//     // a global variable
//     var tracingFailures = promauto.NewCounter(prometheus.CounterOpts{
//       Namespace: "myservice",
//       Subsystem: "tracing",
//       Name:      "failures_total",
//       Help:      "Total number of failures when sending tracing spans to the sidecar",
//     })
//
//     // in main
//     if err := baseplate.ParseConfigYAML(&cfg); err != nil {
//       log.Fatal(err)
//     }
//     cfg.Config.Tracing.Logger = log.CounterWrapper(
//       cfg.Config.Tracing.Logger, // delegate
//       tracingFailures,           // counter
//     }
func CounterWrapper(delegate Wrapper, counter Counter) Wrapper {
	return func(ctx context.Context, msg string) {
		counter.Add(1)
		delegate.Log(ctx, msg)
	}
}

var (
	_ Wrapper = NopWrapper
)
