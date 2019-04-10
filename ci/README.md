# rds-exporter Concourse pipeline

This pipeline was generated using [Jetstream](https://github.com/hellofresh/jetstream) and is powered by the [SCM Back-end Chapter](https://hellofresh.atlassian.net/wiki/display/SBC/SCM+Back-end+Chapter+Home).

To set the pipeline do the following from project root folder:

```BASH
# Login to concourse
fly -t "platform-site-reliability" login --team-name "platform.site-reliability" --concourse-url "https://ci.hellofresh.io"
# Set pipeline
FLY_TARGET="platform-site-reliability" ./ci/set-pipeline.sh
```

## CI Badges

In order to show a badge for your project you will need to [expose the pipeline](https://concourse-ci.org/managing-pipelines.html#fly-expose-pipeline) as followed.

```BASH
# Login to concourse
fly -t "platform-site-reliability" login --team-name "platform.site-reliability" --concourse-url "https://ci.hellofresh.io"
# Expose the pipeline
fly -t "platform-site-reliability" expose-pipeline --pipeline "rds-exporter"
```

Now that you have exposed your pipeline you can use a url `https://webhook-proxy.hellofresh.io/concourse_badges/global/<team-name>/<pipeline-name>/<job-name>` a example for the markdown master test job can be found below.

```MARKDOWN
[![Master Test Status](https://webhook-proxy.hellofresh.io/concourse_badges/global/platform.site-reliability/rds-exporter/Test)](https://ci.hellofresh.io/teams/platform.site-reliability/pipelines/rds-exporter)
```
