package internal

import (
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func SetupLogging(level string) {
	zerolog.CallerMarshalFunc = func(_ uintptr, file string, line int) string {
		_, after, found := strings.Cut(file, Repopath+"/")
		if found {
			file = after
		}
		return file + ":" + strconv.Itoa(line)
	}
	parsedLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		log.Fatal().Str("loglevel", level).Msg("invalid loglevel")
	}
	zerolog.SetGlobalLevel(parsedLevel)

	log.Logger = zerolog.New(os.Stderr).With().Caller().Logger()
}
