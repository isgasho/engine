package config

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/src-d/engine/api"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"gopkg.in/src-d/go-log.v1"
	yaml "gopkg.in/yaml.v2"
)

// File contains the config read from the file path used in Read
var File = &api.Config{}

// Read reads the config file values into File. If configFile path is empty,
// $HOME/.srcd/config.yml will be used, only if it exists.
// If configFile is empty and the default file does not exist the return value
// is nil
func Read(configFile string) error {
	if configFile == "" {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			return errors.Wrapf(err, "could not detect home directory")
		}

		configFile = filepath.Join(home, ".srcd", "config.yml")

		if _, err := os.Stat(configFile); os.IsNotExist(err) {
			return nil
		}
	}

	log.Debugf("Using config file: %s", configFile)

	content, err := ioutil.ReadFile(configFile)
	if err != nil {
		return errors.Wrapf(err, "failed to read config file %s", configFile)
	}

	err = yaml.UnmarshalStrict(content, File)
	if err != nil {
		return errors.Wrapf(err, "config file %s does not follow the expected format", configFile)
	}

	return nil
}
