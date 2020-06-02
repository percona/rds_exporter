# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).


## [Unreleased]
Nothing yet.


## [0.7.0] - 2020-06-02
### Added
- `disable_basic_metrics` and `disable_enhanced_metrics` configuration options.

### Changed
- Major changes in `node_exporter`-like metrics. See [PR #33](https://github.com/percona/rds_exporter/pull/33)
  and [here](https://github.com/percona/rds_exporter#metrics).
- `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment variables are honored now
  ([PR #43](https://github.com/percona/rds_exporter/pull/43) by [@ahmgeek](https://github.com/ahmgeek)).
- Tests and linting improvements.


## [0.6.0] - 2019-12-04
- Initial tagged release.


[Unreleased]: https:/github.com/percona/rds_exporter/compare/v0.7.0...master
[0.7.0]: https://github.com/percona/rds_exporter/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/percona/rds_exporter/releases/tag/v0.6.0
