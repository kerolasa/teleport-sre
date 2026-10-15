package internal

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func SetupLogging(level string) {
	zerolog.CallerMarshalFunc = marshalCaller
	parseLevel(level)

	log.Logger = zerolog.New(os.Stderr).With().Caller().Logger()
}

// SetupConsoleLogging reports the same events SetupLogging does, in the form a
// terminal renders them in.
func SetupConsoleLogging(level string) {
	zerolog.CallerMarshalFunc = marshalCaller
	parseLevel(level)

	// NoColor is left false, so the levels are coloured for the terminal that
	// is there to read them.
	log.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.Kitchen,
	}).With().Timestamp().Caller().Logger()
}

func parseLevel(level string) {
	parsedLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		log.Fatal().Str("loglevel", level).Msg("invalid loglevel")
	}
	zerolog.SetGlobalLevel(parsedLevel)
}

// marshalCaller is where an event was written from, counted from the root of
// the repository.
func marshalCaller(_ uintptr, file string, line int) string {
	_, after, found := strings.Cut(file, Repopath+"/")
	if found {
		file = after
	}
	return file + ":" + strconv.Itoa(line)
}
