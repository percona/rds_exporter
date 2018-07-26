# CloudWatch Exporter

[![Build Status](https://travis-ci.org/percona/rds_exporter.svg?branch=master)](https://travis-ci.org/percona/rds_exporter)
[![Go Report Card](https://goreportcard.com/badge/github.com/percona/rds_exporter)](https://goreportcard.com/report/github.com/percona/rds_exporter)
[![CLA assistant](https://cla-assistant.io/readme/badge/percona/rds_exporter)](https://cla-assistant.io/percona/rds_exporter)

An [AWS CloudWatch](http://aws.amazon.com/cloudwatch/) exporter for [Prometheus](https://github.com/prometheus/prometheus) coded in Go, with multi-region and dynamic target support.
Based on [Technofy/cloudwatch_exporter](https://github.com/Technofy/cloudwatch_exporter).

## How to configure
create config file
```yaml
instances:
- region: us-east-1
  instance: rds-aurora1
  type: aurora_mysql
  aws_access_key: ACCESS-KEY-HERE
  aws_secret_key: SECRET-KEY-HERE
- region: us-east-1
  instance: rds-mysql57
  type: aurora_mysql
  aws_access_key: ACCESS-KEY-HERE
  aws_secret_key: SECRET-KEY-HERE
```

## How to run

```yaml
rds_exporter --config.file=path/to/config.yml --web.listen-address=127.0.0.1:9042
```

## How to configure Prometheus

```yaml
global:
  scrape_interval: 1m
  scrape_timeout: 10s
  evaluation_interval: 1m
scrape_configs:
- job_name: rds-basic
  honor_labels: true
  scrape_interval: 1m
  scrape_timeout: 55s
  metrics_path: /basic
  static_configs:
  - targets:
    - 127.0.0.1:9042
    labels:
      aws_region: us-east-1
      instance: rds-aurora1
  - targets:
    - 127.0.0.1:9042
    labels:
      aws_region: us-east-1
      instance: rds-mysql57
- job_name: rds-enhanced
  honor_labels: true
  scrape_interval: 10s
  scrape_timeout: 9s
  metrics_path: /enhanced
  static_configs:
  - targets:
    - 127.0.0.1:9042
    labels:
      aws_region: us-east-1
      instance: rds-aurora1
  - targets:
    - 127.0.0.1:9042
    labels:
      aws_region: us-east-1
      instance: rds-mysql57
```
