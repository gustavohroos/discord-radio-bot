package config

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

type LogLevelDecoder log.Level

var mapLogLevel = map[string]log.Level{
	"DEBUG": log.DebugLevel,
	"INFO":  log.InfoLevel,
	"WARN":  log.WarnLevel,
	"ERROR": log.ErrorLevel,
}

func (lld *LogLevelDecoder) Decode(value string) error {
	upper := strings.ToUpper(value)
	if val, ok := mapLogLevel[upper]; ok {
		*lld = LogLevelDecoder(val)
		return nil
	}
	return fmt.Errorf("log level %s is not valid", value)
}
