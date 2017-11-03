package config

import (
	"errors"
	"fmt"
	"io/ioutil"
	"strings"

	"gopkg.in/yaml.v2"
)

type Metric struct {
	Namespace string `yaml:"aws_namespace"`
	Name      string `yaml:"aws_metric_name"`

	Statistics            []string            `yaml:"aws_statistics"`
	Dimensions            []string            `yaml:"aws_dimensions,omitempty"`
	DimensionsSelect      map[string][]string `yaml:"aws_dimensions_select,omitempty"`
	DimensionsSelectParam map[string][]string `yaml:"aws_dimensions_select_param,omitempty"`

	RangeSeconds  int `yaml:"range_seconds,omitempty"`
	PeriodSeconds int `yaml:"period_seconds,omitempty"`
	DelaySeconds  int `yaml:"delay_seconds,omitempty"`
}

type Task struct {
	Name          string   `yaml:"name"`
	DefaultRegion string   `yaml:"default_region,omitempty"`
	Metrics       []Metric `yaml:"metrics"`
}

type Settings struct {
	AutoReload  bool   `yaml:"auto_reload,omitempty"`
	ReloadDelay int    `yaml:"auto_reload_delay,omitempty"`
	Tasks       []Task `yaml:"tasks"`
}

func (s *Settings) GetTask(name string) (*Task, error) {
	for i := range s.Tasks {
		if strings.Compare(s.Tasks[i].Name, name) == 0 {
			return &s.Tasks[i], nil
		}
	}

	return nil, errors.New(fmt.Sprintf("can't find task '%s' in configuration", name))
}

func Load(filename string) (*Settings, error) {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	cfg := &Settings{}
	err = yaml.Unmarshal(content, cfg)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (m *Metric) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type plain Metric

	// These are the default values for a basic metric config
	rawMetric := plain{
		PeriodSeconds: 60,
		RangeSeconds:  600,
		DelaySeconds:  600,
	}
	if err := unmarshal(&rawMetric); err != nil {
		return err
	}

	*m = Metric(rawMetric)
	return nil
}
