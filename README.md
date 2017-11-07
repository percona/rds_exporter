# CloudWatch Exporter

[![Build Status](https://travis-ci.org/Percona-Lab/rds_exporter.svg?branch=master)](https://travis-ci.org/Percona-Lab/rds_exporter)
[![Go Report Card](https://goreportcard.com/badge/github.com/Percona-Lab/rds_exporter)](https://goreportcard.com/report/github.com/Percona-Lab/rds_exporter)
[![CLA assistant](https://cla-assistant.io/readme/badge/Percona-Lab/rds_exporter)](https://cla-assistant.io/Percona-Lab/rds_exporter)

An [AWS CloudWatch](http://aws.amazon.com/cloudwatch/) exporter for [Prometheus](https://github.com/prometheus/prometheus) coded in Go, with multi-region and dynamic target support.
Based on [Technofy/cloudwatch_exporter](https://github.com/Technofy/cloudwatch_exporter).

## How to configure

The configuration is in YAML and tries to stay in the same spirit as the official exporter.

```yaml
tasks:
 - name: billing
   default_region: us-east-1
   metrics:
    - aws_namespace: "AWS/Billing"
      aws_dimensions: [Currency]
      aws_dimensions_select:
        Currency: [USD]
      aws_metric_name: EstimatedCharges
      aws_statistics: [Maximum]
      range_seconds: 86400

 - name: ec2_cloudwatch
   metrics:
    - aws_namespace: "AWS/EC2"
      aws_dimensions: [InstanceId]
      aws_dimensions_select:
        InstanceId: [$_target]
      aws_metric_name: CPUUtilization
      aws_statistics: [Average]

    - aws_namespace: "AWS/EC2"
      aws_dimensions: [InstanceId]
      aws_dimensions_select:
        InstanceId: [$_target]
      aws_metric_name: NetworkOut
      aws_statistics: [Average]

  - name: vpn_mon
    metrics:
     - aws_namespace: "AWS/VPN"
       aws_dimensions: [VpnId]
       aws_dimensions_select:
         VpnId: [$_target]
       aws_metric_name: TunnelState
       aws_statistics: [Average]
       range_seconds: 3600
```


### What are "Tasks" and "$_target"?

Tasks are used to describe a CloudWatch scrape that can be reused on a whole set of instances and even cross-region.

The **$_target** token in the dimensions select is used to pass a parameter given by  Prometheus (for example a \__meta tag with service discovery).

For example a scrape URL looks like this:

`http://localhost:9042/scrape?task=ec2_cloudwatch&target=i-0123456789&region=eu-west-1`

With the example configuration above, this means that the CloudWatch exporter will scrape the `CPUUtilization` and the `NetworkOut` metrics when the dimension `InstanceId` will be equal to `i-0123456789` in the `eu-west-1` region according to the configuration of the task: `ec2_cloudwatch`.

### Hot reload of the configuration

Let's say you can't afford to kill the process and restart it for any reason and you need to modify the configuration on the fly. It's possible! Just call the `/reload` endpoint.


## How to configure Prometheus

```yaml
  - job_name: 'aws_billing'
    metrics_path: '/scrape'
    params:
      task: [billing]
    static_configs:
      - targets: ['localhost:9042']

  - job_name: 'ec2_cloudwatch'
    metrics_path: '/scrape'
    ec2_sd_configs:
      - region: eu-west-1
    params:
      region: [eu-west-1]
    relabel_configs:
      - source_labels: [__meta_ec2_tag_role]
        regex: webapp
        action: keep
      - source_labels: [job]
        target_label: __param_task
      - source_labels: [__meta_ec2_instance_id]
        target_label: __param_target
      - target_label: __address__
        replacement: 'localhost:9042'

  - job_name: 'vpn_mon'
    metrics_path: '/scrape'
    params:
      task: [vpn_mon]
    static_configs:
      - targets: ['vpn-aabbccdd']
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - target_label: __address__
        replacement: 'localhost:9042'
```

Thanks to Prometheus relabelling feature, in the second job configuration, we tell it to use the `job_name` as the `task` parameter and to use the `__meta_ec2_instance_id` as the `target` parameter. The region is specified in the `params` section.

The Billing example is there to demonstrate the multi-region capability of this exporter, the `default_region` parameter is specified in the exporter's configuration.

**Note:** It would also work if no default_region was specified but a `params` block with the `region` parameter was set in the Prometheus configuration.

## Endpoints


| Endpoint      | Description                                  |
| ------------- | -------------------------------------------- |
| `/metrics`    | Gathers metrics from the CloudWatch exporter itself such as the total number of requests made to the AWS CloudWatch API.
| `/scrape`     | Gathers metrics from the CloudWatch API depending on the task and (optionally) the target passed as parameters.
| `/reload`     | Does a live reload of the configuration without restarting the process

## Why this remake?

We felt left out when we wanted to monitor hundreds of machines on AWS thanks to CloudWatch when using the original exporter. We wanted to be able to use the service EC2 discovery functionnality provided by Prometheus to dynamically monitor our fleet.

Regarding our requirements, installing Java runtime was also a bit of an issue, so we decided to make it *"compliant"* with the rest of the Prometheus project by using Golang.

## TODO

This exporter is still in its early stages! It still lacks the `dimensions_select_regex` parameter and the DynamoDB special use-cases. Any help and/or criticism is welcome!


## End Note

This exporter is largely inspired by the [official CloudWatch Exporter](https://github.com/prometheus/cloudwatch_exporter) and we'd like to thank all the contributors who participated to the original project.

This project is licensed under the [Apache 2.0 license](https://github.com/Technofy/cloudwatch_exporter/blob/master/LICENSE).
