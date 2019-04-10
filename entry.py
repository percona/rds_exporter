#!/usr/bin/env python


import boto3
import yaml
import subprocess


REGION = 'eu-west-1'


def get_db_instances(region, page_size=20):
    client = boto3.client('rds', region_name=region)
    marker = ""
    pool = []
    while True:
        for instance in pool:
            yield instance

        if marker is None:
            break
        result = client.describe_db_instances(MaxRecords=page_size,
                                              Marker=marker)
        marker = result.get("Marker")
        pool = result.get("DBInstances")


if __name__ == "__main__":
    em_enabled_instances = {'instances': []}

    for instance in get_db_instances(REGION):
        if instance.get('EnhancedMonitoringResourceArn', None):
            instance_name = instance['DBInstanceIdentifier']
            instance = {'instance': instance_name, 'region': REGION}
            em_enabled_instances['instances'].append(instance)

    with open('/config.yml', 'w') as yaml_file:
        yaml.dump(em_enabled_instances, yaml_file, default_flow_style=False)

    rds_exporter = '/rds_exporter \
                --config.file=/config.yml'

    subprocess.run(rds_exporter, shell=True)
