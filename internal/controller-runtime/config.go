package controllerruntime

import (
	"github.com/BurntSushi/toml"
)

type Conf struct {
	API      Listener
	Metrics  Listener
	Loglevel string
}

type Listener struct {
	Host   string
	Port   uint16
	Prefix string // this for relocating api
	Path   string // metrics uri
}

func ReadConfig(path string) (Conf, error) {
	var conf Conf

	_, err := toml.DecodeFile(path, &conf)
	if err != nil {
		return conf, err
	}

	return conf, nil
}
