package config

import (
	"io/ioutil"

	"gopkg.in/yaml.v2"
)

// InstanceType is not used yet

// type InstanceType string

// const (
// 	Unknown     InstanceType = "unknown"
// 	AuroraMySQL InstanceType = "aurora_mysql"
// 	MySQL       InstanceType = "mysql"
// )

// Instance represents a single RDS information from configuration file.
type Instance struct {
	Region   string `yaml:"region"`
	Instance string `yaml:"instance"`
	// Type InstanceType `yaml:"type"` // may be empty for old pmm-managed
	AWSAccessKey string `yaml:"aws_access_key"` // may be empty
	AWSSecretKey string `yaml:"aws_secret_key"` // may be empty
}

func (i Instance) String() string {
	res := i.Region + "/" + i.Instance
	if i.AWSAccessKey != "" {
		res += " (" + i.AWSAccessKey + ")"
	}

	return res
}

// Config contains configuration file information.
type Config struct {
	Instances []Instance `yaml:"instances"`
}

// Load loads configuration from file.
func Load(filename string) (*Config, error) {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	if err = yaml.Unmarshal(b, &config); err != nil {
		return nil, err
	}
	return &config, nil
}
