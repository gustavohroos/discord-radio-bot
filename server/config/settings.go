package config

import "github.com/kelseyhightower/envconfig"

type Settings struct {
	DiscordToken string          `split_words:"true" required:"true"`
	LogLevel     LogLevelDecoder `split_words:"true" default:"info"`
}

func LoadSettings() (Settings, error) {
	var settings Settings
	err := envconfig.Process("", &settings)

	return settings, err
}
