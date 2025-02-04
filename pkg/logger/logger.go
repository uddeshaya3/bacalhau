package logger

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/pkgerrors"

	"github.com/filecoin-project/bacalhau/pkg/model"
	ipfslog2 "github.com/ipfs/go-log/v2"
	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var nodeIDFieldName = "NodeID"

func init() { //nolint:gochecknoinits // init with zerolog is idiomatic
	configureLogging()
}

type tTesting interface {
	Log(args ...interface{})
	Logf(format string, args ...interface{})
	Helper()
	Cleanup(f func())
}

// ConfigureTestLogging allows logs to be associated with individual tests
func ConfigureTestLogging(t tTesting) {
	oldLogger := log.Logger
	oldContextLogger := zerolog.DefaultContextLogger
	configureLogging(zerolog.ConsoleTestWriter(t))
	t.Cleanup(func() {
		log.Logger = oldLogger
		zerolog.DefaultContextLogger = oldContextLogger
		configureIpfsLogging(log.Logger)
	})
}

func configureLogging(loggingOptions ...func(w *zerolog.ConsoleWriter)) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	logLevelString := strings.ToLower(os.Getenv("LOG_LEVEL"))
	logTypeString := strings.ToLower(os.Getenv("LOG_TYPE"))

	switch {
	case logLevelString == "trace":
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case logLevelString == "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case logLevelString == "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case logLevelString == "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case logLevelString == "fatal":
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	isTerminal := isatty.IsTerminal(os.Stdout.Fd())

	defaultLogging := func(w *zerolog.ConsoleWriter) {
		w.Out = os.Stderr
		w.NoColor = !isTerminal
		w.TimeFormat = "15:04:05.999 |"
		w.PartsOrder = []string{
			zerolog.TimestampFieldName,
			zerolog.LevelFieldName,
			zerolog.CallerFieldName,
			zerolog.MessageFieldName,
		}

		// TODO: figure out a way to show the custom fields at the beginning of the log line rather than at the end.
		//  Adding the fields to the parts section didn't help as it just printed the fields twice.
		w.FormatFieldName = func(i interface{}) string {
			return fmt.Sprintf("[%s:", i)
		}

		w.FormatFieldValue = func(i interface{}) string {
			// don't print nil in case field value wasn't preset. e.g. no nodeID
			if i == nil {
				i = ""
			}
			return fmt.Sprintf("%s]", i)
		}
	}

	loggingOptions = append([]func(w *zerolog.ConsoleWriter){defaultLogging}, loggingOptions...)

	textWriter := zerolog.NewConsoleWriter(loggingOptions...)

	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Path != "" {
		// Branch that'll be used when the binary is run, as it is built as a Go module
		zerolog.CallerMarshalFunc = marshalCaller(info.Main.Path)
	} else {
		// Branch typically used when running under test as build info isn't populated
		// https://github.com/golang/go/issues/33976
		dir := findRepositoryRoot()
		if dir != "" {
			zerolog.CallerMarshalFunc = marshalCaller(dir)
		}
	}

	// we default to text output
	var useLogWriter io.Writer = textWriter

	if logTypeString == "json" {
		// we just want json
		useLogWriter = os.Stdout
	} else if logTypeString == "combined" {
		// we just want json and text and events
		useLogWriter = zerolog.MultiLevelWriter(textWriter, os.Stdout)
	} else if logTypeString == "event" {
		// we just want events
		useLogWriter = io.Discard
	}

	log.Logger = zerolog.New(useLogWriter).With().Timestamp().Caller().Stack().Logger()
	// While the normal flow will use ContextWithNodeIDLogger, this won't be so for tests.
	// Tests will use the DefaultContextLogger instead
	zerolog.DefaultContextLogger = &log.Logger

	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	configureIpfsLogging(log.Logger)
}

func loggerWithNodeID(nodeID string) zerolog.Logger {
	if len(nodeID) > 8 { //nolint:gomnd // 8 is a magic number
		nodeID = nodeID[:model.ShortIDLength]
	}
	return log.With().Str(nodeIDFieldName, nodeID).Logger()
}

// ContextWithNodeIDLogger will return a context with nodeID is added to the logging context.
func ContextWithNodeIDLogger(ctx context.Context, nodeID string) context.Context {
	l := loggerWithNodeID(nodeID)
	return l.WithContext(ctx)
}

type zerologWriteSyncer struct {
	l zerolog.Logger
}

var _ zapcore.WriteSyncer = (*zerologWriteSyncer)(nil)

func (z *zerologWriteSyncer) Write(b []byte) (int, error) {
	z.l.Log().CallerSkipFrame(5).Msg(string(b)) //nolint:gomnd
	return len(b), nil
}

func (z *zerologWriteSyncer) Sync() error {
	return nil
}

func configureIpfsLogging(l zerolog.Logger) {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {}
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encCfg.EncodeCaller = func(caller zapcore.EntryCaller, enc zapcore.PrimitiveArrayEncoder) {}
	encCfg.ConsoleSeparator = " "
	encoder := zapcore.NewConsoleEncoder(encCfg)

	core := zapcore.NewCore(encoder, &zerologWriteSyncer{l: l}, zap.NewAtomicLevelAt(zapcore.DebugLevel))

	ipfslog2.SetPrimaryCore(core)
}

func LogStream(ctx context.Context, r io.Reader) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		log.Ctx(ctx).Debug().Msg(s.Text())
	}
	if s.Err() != nil {
		log.Ctx(ctx).Error().Err(s.Err()).Msg("error consuming log")
	}
}

func findRepositoryRoot() string {
	dir, _ := os.Getwd()
	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if os.IsNotExist(err) {
			parentDir := filepath.Dir(dir)
			if dir == parentDir {
				return ""
			}
			dir = parentDir
			continue
		}

		if runtime.GOOS == "windows" {
			dir = strings.ReplaceAll(dir, "\\", "/")
		}

		return dir
	}
}

func marshalCaller(prefix string) func(uintptr, string, int) string {
	return func(_ uintptr, file string, line int) string {
		file = strings.TrimPrefix(file, prefix+"/")
		return file + ":" + strconv.Itoa(line)
	}
}
