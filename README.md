# RDS Exporter

[![Release](https://img.shields.io/github/release/percona/rds_exporter.svg?style=flat)](https://github.com/percona/rds_exporter/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/percona/rds_exporter)](https://goreportcard.com/report/github.com/percona/rds_exporter)
[![CLA assistant](https://cla-assistant.percona.com/readme/badge/percona/rds_exporter)](https://cla-assistant.percona.com/percona/rds_exporter)
[![codecov.io Code Coverage](https://img.shields.io/codecov/c/github/percona/rds_exporter.svg?maxAge=2592000)](https://codecov.io/github/percona/rds_exporter?branch=main)

An [AWS RDS](https://aws.amazon.com/ru/rds/) exporter for [Prometheus](https://github.com/prometheus/prometheus).
It gets metrics from both [basic CloudWatch Metrics](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/MonitoringOverview.html)
and [RDS Enhanced Monitoring via CloudWatch Logs](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_Monitoring.OS.html).

## Quick start

Create configration file `config.yml`:

```yaml
---
instances:
  - region: us-east-1
    instance: rds-aurora1

  - region: us-east-1
    instance: rds-mysql57
    aws_access_key: AKIAIOSFODNN7EXAMPLE
    aws_secret_key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
    aws_role_arn: arn:aws:iam::76784568345:role/my-role
    disable_basic_metrics: true
    disable_enhanced_metrics: false
    labels:
      foo: bar
      baz: qux
```

If `aws_role_arn` is present it will assume role otherwise if `aws_access_key` and `aws_secret_key` are present, they are used for that instance.
Otherwise, [default credential provider chain](https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html#specifying-credentials)
is used, which includes `AWS_ACCESS_KEY_ID`/`AWS_ACCESS_KEY` and `AWS_SECRET_ACCESS_KEY`/`AWS_SECRET_KEY` environment variables, `~/.aws/credentials` file,
and IAM role for EC2.

Returned metrics contain `instance` and `region` labels set. They also contain extra labels specified in the configuration file.

Start exporter by running:
```
rds_exporter
```

To see all flags run:
```
rds_exporter --help
```

Configure Prometheus:

```yaml
---
scrape_configs:
  - job_name: rds-basic
    scrape_interval: 60s
    scrape_timeout: 55s
    metrics_path: /basic
    honor_labels: true
    static_configs:
      - targets:
        - 127.0.0.1:9042

  - job_name: rds-enhanced
    scrape_interval: 10s
    scrape_timeout: 9s
    metrics_path: /enhanced
    honor_labels: true
    static_configs:
      - targets:
        - 127.0.0.1:9042
```

`honor_labels: true` is important because exporter returns metrics with `instance` label set.

## Metrics

Exporter synthesizes [node_exporter](https://github.com/prometheus/node_exporter)-like metrics where possible.

You can see a list of basic monitoring metrics [there](https://github.com/percona/rds_exporter/blob/main/basic/testdata/all.txt)
and a list of enhanced monitoring metrics in text files [there](https://github.com/percona/rds_exporter/tree/main/enhanced/testdata).

## Cost
Amazon charges for every CloudWatch API request, see the [current charges](http://aws.amazon.com/cloudwatch/pricing/).

Every metric retrieved requires one API request, which can include multiple statistics.

If you have 100 API requests every minute, with the price of $10 per million requests (as of Aug 2018), that is around $45 per month. 
